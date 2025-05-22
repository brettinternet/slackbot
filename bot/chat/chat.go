package chat

import (
	"context"
	"math/rand"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

const eventChannelSize = 100

// Response defines a pattern to match and the corresponding response
type Response struct {
	Pattern        string   `json:"pattern" yaml:"pattern"`                 // Can be a plain text or a regular expression
	Message        string   `json:"message" yaml:"message"`                 // response
	Messages       string   `json:"messages" yaml:"messages"`               // Deprecated, use Message
	RandomMessages []string `json:"random_messages" yaml:"random_messages"` // Random messages to respond with
	Reactions      []string `json:"reactions" yaml:"reactions"`             // Reactions to add to the message
	IsRegexp       bool     `json:"is_regexp" yaml:"is_regexp"`             // Whether the pattern is a regular expression
}

type slackService interface {
	Client() *slack.Client
}

// FileConfig represents the structure of the chat section in the config file
type FileConfig struct {
	Responses []Response `json:"responses" yaml:"responses"`
}

// Config defines the configuration for the Chat feature
type Config struct {
	PreferredUsers []string
}

// ChatConfig contains configuration specific to the chat module
type ChatConfig struct {
	Responses []Response
}

// Chat handles responding to messages based on configured patterns
type Chat struct {
	log         *zap.Logger
	config      Config
	slack       slackService
	regexps     map[string]*regexp.Regexp
	stopCh      chan struct{}
	eventsCh    chan slackevents.EventsAPIEvent
	isConnected atomic.Bool
	fileConfig  FileConfig
}

func NewChat(log *zap.Logger, c Config, s slackService) *Chat {
	return &Chat{
		log:      log,
		config:   c,
		regexps:  make(map[string]*regexp.Regexp),
		stopCh:   make(chan struct{}),
		eventsCh: make(chan slackevents.EventsAPIEvent, eventChannelSize),
		slack:    s,
	}
}

// ProcessorType returns a description of the processor type
func (c *Chat) ProcessorType() string {
	return "chat"
}

// Start initializes the Chat feature with a Slack slack
func (c *Chat) Start(ctx context.Context) error {
	c.isConnected.Store(true)

	go c.handleEvents(ctx)

	c.log.Debug("Chat feature started successfully.",
		zap.Int("responses", len(c.fileConfig.Responses)),
	)
	return nil
}

// Stop stops the chat service
func (c *Chat) Stop(ctx context.Context) error {
	if !c.isConnected.Load() {
		return nil
	}

	close(c.stopCh)
	c.isConnected.Store(false)

	return nil
}

// PushEvent adds an event to be processed by the Chat feature
func (c *Chat) PushEvent(event slackevents.EventsAPIEvent) {
	if !c.isConnected.Load() {
		return
	}

	select {
	case c.eventsCh <- event:
		// Event pushed successfully
	default:
		c.log.Warn("Chat events channel full, dropping event.")
	}
}

// handleEvents processes Slack events
func (c *Chat) handleEvents(ctx context.Context) {
	for {
		select {
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		case event := <-c.eventsCh:
			c.processEvent(ctx, event)
		}
	}
}

// processEvent handles a single Slack event
func (c *Chat) processEvent(ctx context.Context, event slackevents.EventsAPIEvent) {
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			// Ignore bot messages to prevent loops
			if ev.BotID != "" || ev.User == "" {
				return
			}
			c.handleMessageEvent(ctx, ev)
		}
	}
}

// handleMessageEvent processes a message event and responds if it matches a pattern
func (c *Chat) handleMessageEvent(ctx context.Context, ev *slackevents.MessageEvent) {
	message := strings.TrimSpace(ev.Text)

	c.log.Debug("Processing message",
		zap.String("user", ev.User),
		zap.String("channel", ev.Channel),
		zap.String("text", message),
		zap.String("type", c.ProcessorType()),
	)

	var messageReplied bool
	for _, resp := range c.fileConfig.Responses {
		var isMatch bool
		if resp.IsRegexp {
			re, exists := c.regexps[resp.Pattern]
			if !exists {
				rec, err := regexp.Compile("(?i)" + resp.Pattern)
				if err != nil {
					c.log.Error("Failed to compile regex pattern",
						zap.String("pattern", resp.Pattern),
						zap.Error(err),
					)
					continue
				}
				re = rec
				c.regexps[resp.Pattern] = re
			}
			isMatch = re.MatchString(message)
		} else {
			isMatch = strings.EqualFold(message, resp.Pattern)
		}

		if isMatch {
			c.log.Info("Message matched pattern",
				zap.String("pattern", resp.Pattern),
				zap.String("channel", ev.Channel),
			)

			if len(resp.Reactions) > 0 {
				for _, reaction := range resp.Reactions {
					err := c.slack.Client().AddReactionContext(
						ctx,
						reaction,
						slack.NewRefToMessage(ev.Channel, ev.TimeStamp),
					)
					if err != nil {
						c.log.Error("Failed to add reaction",
							zap.String("channel", ev.Channel),
							zap.String("user", ev.User),
							zap.String("reaction", reaction),
							zap.Error(err),
						)
					}
				}
			}

			// Check if the message is already replied to, so we can still add all reactions from responses
			if !messageReplied && resp.Message != "" {
				messageReplied = true
				messages := append([]string{resp.Message}, resp.RandomMessages...)
				if len(resp.RandomMessages) > 0 {
					messages = append([]string{randomString(resp.RandomMessages)}, messages...)
				}
				for _, msg := range messages {
					if msg == "" {
						continue
					}
					_, _, err := c.slack.Client().PostMessageContext(
						ctx,
						ev.Channel,
						slack.MsgOptionText(msg, false),
						slack.MsgOptionAsUser(true),
					)
					if err != nil {
						c.log.Error("Failed to post response",
							zap.String("channel", ev.Channel),
							zap.Error(err),
						)
					}
				}
			}
		}
	}

	c.log.Debug("No matching response found for message",
		zap.String("text", message),
		zap.String("channel", ev.Channel),
		zap.String("type", c.ProcessorType()),
	)
}

// SetConfig updates the chat configuration with values from the centralized config
func (c *Chat) SetConfig(cfg FileConfig) error {
	c.log.Info("Updating chat configuration",
		zap.Int("responses", len(cfg.Responses)))

	c.fileConfig = cfg

	c.regexps = make(map[string]*regexp.Regexp)
	for _, resp := range c.fileConfig.Responses {
		if resp.IsRegexp {
			re, err := regexp.Compile("(?i)" + resp.Pattern)
			if err != nil {
				c.log.Error("Failed to compile regex pattern",
					zap.String("pattern", resp.Pattern),
					zap.Error(err),
				)
				continue
			}
			c.regexps[resp.Pattern] = re
		}
	}

	return nil
}

func randomString(values []string) string {
	rand.New(rand.NewSource(time.Now().UnixNano()))
	return values[rand.Intn(len(values))]
}
