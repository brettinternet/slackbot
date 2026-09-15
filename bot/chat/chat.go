package chat

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
	"slackbot.arpa/tools/random"
)

const (
	eventChannelSize = 100
	dedupeDuration   = 30 * time.Second
)

var appMentionPrefix = regexp.MustCompile(`^<@[^>]+>\s*`)

// Response defines a pattern to match and the corresponding response.
type Response struct {
	Pattern        string   `json:"pattern" yaml:"pattern"`                 // Can be plain text or a regular expression
	Message        string   `json:"message" yaml:"message"`                 // Message to always send
	Messages       string   `json:"messages" yaml:"messages"`               // Deprecated: use Message
	RandomMessages []string `json:"random_messages" yaml:"random_messages"` // One randomly selected message to send
	Reactions      []string `json:"reactions" yaml:"reactions"`             // Reactions to add to the message
	IsRegexp       bool     `json:"is_regexp" yaml:"is_regexp"`             // Whether Pattern is a regular expression
}

type slackService interface {
	AddReaction(context.Context, string, string, string) error
	PostMessage(context.Context, string, string, string) error
}

// FileConfig represents the structure of the chat section in the config file.
type FileConfig struct {
	Responses []Response `json:"responses" yaml:"responses"`
}

// Config defines the runtime configuration for the Chat feature.
type Config struct {
	Responses []Response
}

type compiledResponse struct {
	pattern        string
	message        string
	randomMessages []string
	reactions      []string
	regexp         *regexp.Regexp
}

type compiledConfig struct {
	responses []compiledResponse
}

type recentMessageKey struct {
	userID    string
	channelID string
	messageID string
}

type messageDeduplicator struct {
	mu       sync.Mutex
	recent   map[recentMessageKey]time.Time
	duration time.Duration
}

func newMessageDeduplicator(duration time.Duration) *messageDeduplicator {
	return &messageDeduplicator{
		recent:   make(map[recentMessageKey]time.Time),
		duration: duration,
	}
}

func (d *messageDeduplicator) isDuplicate(userID, channelID, messageID string) bool {
	if messageID == "" {
		return false
	}

	now := time.Now()
	key := recentMessageKey{userID: userID, channelID: channelID, messageID: messageID}

	d.mu.Lock()
	defer d.mu.Unlock()

	for candidate, seenAt := range d.recent {
		if now.Sub(seenAt) > d.duration {
			delete(d.recent, candidate)
		}
	}
	if _, exists := d.recent[key]; exists {
		return true
	}
	d.recent[key] = now
	return false
}

// Chat handles responding to messages based on configured patterns.
type Chat struct {
	log      *zap.Logger
	slack    slackService
	config   atomic.Pointer[compiledConfig]
	eventsCh chan slackevents.EventsAPIEvent
	dedupe   *messageDeduplicator
	pending  atomic.Int64

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	isConnected atomic.Bool
}

func NewChat(log *zap.Logger, cfg Config, service slackService) (*Chat, error) {
	compiled, err := compileConfig(cfg)
	if err != nil {
		return nil, err
	}

	chat := &Chat{
		log:      log,
		slack:    service,
		eventsCh: make(chan slackevents.EventsAPIEvent, eventChannelSize),
		dedupe:   newMessageDeduplicator(dedupeDuration),
	}
	chat.config.Store(compiled)
	return chat, nil
}

// ProcessorType returns a description of the processor type.
func (c *Chat) ProcessorType() string {
	return "chat"
}

// Start starts the Chat event worker. Repeated calls while running are harmless.
func (c *Chat) Start(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	if c.isConnected.Load() {
		return nil
	}

	workerCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	c.cancel = cancel
	c.done = done
	c.isConnected.Store(true)

	go c.handleEvents(workerCtx, done)

	c.log.Debug("Chat feature started successfully.",
		zap.Int("responses", len(c.config.Load().responses)),
	)
	return nil
}

// Stop stops the Chat event worker and waits for it to exit.
func (c *Chat) Stop(ctx context.Context) error {
	c.lifecycleMu.Lock()
	defer c.lifecycleMu.Unlock()

	if !c.isConnected.Load() {
		return nil
	}

	c.cancel()
	select {
	case <-c.done:
		c.cancel = nil
		c.done = nil
		c.isConnected.Store(false)
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop chat: %w", ctx.Err())
	}
}

// PushEvent adds an event to be processed by the Chat feature.
func (c *Chat) PushEvent(event slackevents.EventsAPIEvent) {
	if !c.isConnected.Load() {
		return
	}

	c.pending.Add(1)
	select {
	case c.eventsCh <- event:
	default:
		c.pending.Add(-1)
		c.log.Warn("Chat events channel full, dropping event.")
	}
}

func (c *Chat) PendingEvents() int64 { return c.pending.Load() }

func (c *Chat) handleEvents(ctx context.Context, done chan<- struct{}) {
	defer close(done)
	defer c.isConnected.Store(false)

	for {
		select {
		case <-ctx.Done():
			return
		case event := <-c.eventsCh:
			c.processEvent(ctx, event)
			c.pending.Add(-1)
		}
	}
}

type eventMessage struct {
	userID          string
	channel         string
	text            string
	timestamp       string
	threadTimestamp string
}

// processEvent handles a single Slack event.
func (c *Chat) processEvent(ctx context.Context, event slackevents.EventsAPIEvent) {
	if event.Type != slackevents.CallbackEvent {
		return
	}

	var message eventMessage
	switch ev := event.InnerEvent.Data.(type) {
	case *slackevents.AppMentionEvent:
		if ev.BotID != "" || ev.User == "" {
			return
		}
		message = eventMessage{
			userID:          ev.User,
			channel:         ev.Channel,
			text:            appMentionPrefix.ReplaceAllString(ev.Text, ""),
			timestamp:       ev.TimeStamp,
			threadTimestamp: ev.ThreadTimeStamp,
		}
	case *slackevents.MessageEvent:
		if ev.BotID != "" || ev.User == "" || ev.SubType != "" {
			return
		}
		message = eventMessage{
			userID:          ev.User,
			channel:         ev.Channel,
			text:            ev.Text,
			timestamp:       ev.TimeStamp,
			threadTimestamp: ev.ThreadTimeStamp,
		}
	default:
		return
	}

	if c.dedupe.isDuplicate(message.userID, message.channel, message.timestamp) {
		c.log.Debug("Skipping duplicate chat message",
			zap.String("user", message.userID),
			zap.String("channel", message.channel),
			zap.String("timestamp", message.timestamp),
		)
		return
	}
	c.handleMessageEvent(ctx, message)
}

// handleMessageEvent responds to a message when it matches configured patterns.
func (c *Chat) handleMessageEvent(ctx context.Context, event eventMessage) {
	message := strings.TrimSpace(event.text)

	c.log.Debug("Processing message",
		zap.String("user", event.userID),
		zap.String("channel", event.channel),
		zap.String("text", message),
		zap.String("type", c.ProcessorType()),
	)

	matched := false
	messageReplied := false
	for _, response := range c.config.Load().responses {
		isMatch := response.regexp != nil && response.regexp.MatchString(message)
		if response.regexp == nil {
			isMatch = strings.EqualFold(message, response.pattern)
		}
		if !isMatch {
			continue
		}

		matched = true
		c.log.Info("Message matched pattern",
			zap.String("pattern", response.pattern),
			zap.String("channel", event.channel),
		)

		for _, reaction := range response.reactions {
			if err := c.slack.AddReaction(ctx, reaction, event.channel, event.timestamp); err != nil {
				c.log.Error("Failed to add reaction",
					zap.String("channel", event.channel),
					zap.String("user", event.userID),
					zap.String("reaction", reaction),
					zap.Error(err),
				)
			}
		}

		if messageReplied {
			continue
		}
		messages := responseMessages(response)
		if len(messages) == 0 {
			continue
		}
		messageReplied = true
		for _, responseMessage := range messages {
			if err := c.slack.PostMessage(
				ctx, event.channel, responseMessage, event.threadTimestamp,
			); err != nil {
				c.log.Error("Failed to post response",
					zap.String("channel", event.channel),
					zap.Error(err),
				)
			}
		}
	}

	if !matched {
		c.log.Debug("No matching response found for message",
			zap.String("text", message),
			zap.String("channel", event.channel),
			zap.String("type", c.ProcessorType()),
		)
	}
}

func responseMessages(response compiledResponse) []string {
	messages := make([]string, 0, 2)
	if response.message != "" {
		messages = append(messages, response.message)
	}
	if len(response.randomMessages) > 0 {
		messages = append(messages, random.String(response.randomMessages))
	}
	return messages
}

// SetConfig validates and atomically updates the chat configuration.
func (c *Chat) SetConfig(cfg Config) error {
	compiled, err := compileConfig(cfg)
	if err != nil {
		return err
	}

	c.config.Store(compiled)
	c.log.Info("Updated chat configuration", zap.Int("responses", len(compiled.responses)))
	return nil
}

func compileConfig(cfg Config) (*compiledConfig, error) {
	compiled := &compiledConfig{responses: make([]compiledResponse, 0, len(cfg.Responses))}
	for _, response := range cfg.Responses {
		message := response.Message
		if message == "" {
			message = response.Messages
		}

		item := compiledResponse{
			pattern:        response.Pattern,
			message:        message,
			randomMessages: append([]string(nil), response.RandomMessages...),
			reactions:      append([]string(nil), response.Reactions...),
		}
		if response.IsRegexp {
			re, err := regexp.Compile("(?i)" + response.Pattern)
			if err != nil {
				return nil, fmt.Errorf("compile chat pattern %q: %w", response.Pattern, err)
			}
			item.regexp = re
		}
		compiled.responses = append(compiled.responses, item)
	}
	return compiled, nil
}
