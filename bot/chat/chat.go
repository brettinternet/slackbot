package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path"
	"regexp"
	"strings"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/goccy/go-yaml"
	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

const eventChannelSize = 100

// Response defines a pattern to match and the corresponding response
type Response struct {
	Pattern        string   `json:"pattern"` // Can be a plain text or a regular expression
	Message        string   `json:"message"` // The message to respond with
	Messages       string   `json:"messages"`
	RandomMessages []string `json:"randomMessages"` // Random messages to respond with
	Reactions      []string `json:"reactions"`      // Reactions to add to the message
	IsRegexp       bool     `json:"isRegexp"`       // Whether the pattern is a regular expression
}

// Config defines the configuration for the Chat feature
type Config struct {
	ResponsesFile  string
	UseRegexp      bool
	PreferredUsers []string
}

// Chat handles responding to messages based on configured patterns
type Chat struct {
	log           *zap.Logger
	config        Config
	slack         *slack.Client
	regexps       map[string]*regexp.Regexp
	stopCh        chan struct{}
	eventsCh      chan slackevents.EventsAPIEvent
	isConnected   atomic.Bool
	responses     []Response
	watcher       *fsnotify.Watcher
	lastModTime   time.Time
	pollingTicker *time.Ticker
}

func NewChat(log *zap.Logger, config Config, client *slack.Client) *Chat {
	return &Chat{
		log:      log,
		config:   config,
		regexps:  make(map[string]*regexp.Regexp),
		stopCh:   make(chan struct{}),
		eventsCh: make(chan slackevents.EventsAPIEvent, eventChannelSize),
		slack:    client,
	}
}

// ProcessorType returns a description of the processor type
func (c *Chat) ProcessorType() string {
	return "chat"
}

// Start initializes the Chat feature with a Slack slack
func (c *Chat) Start(ctx context.Context) error {
	if c.config.ResponsesFile == "" {
		return fmt.Errorf("responses file not specified")
	}

	// Initialize file watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("create file watcher: %w", err)
	}
	c.watcher = watcher

	// Get initial file stats and load responses
	fileInfo, err := os.Stat(c.config.ResponsesFile)
	if err != nil {
		return fmt.Errorf("stat responses file: %w", err)
	}
	c.lastModTime = fileInfo.ModTime()

	if err := c.readResponses(); err != nil {
		return fmt.Errorf("read responses file: %w", err)
	}

	// Add file to watch
	if err := watcher.Add(c.config.ResponsesFile); err != nil {
		c.log.Warn("Could not watch responses file, falling back to polling only",
			zap.String("file", c.config.ResponsesFile),
			zap.Error(err),
		)
	}

	// Create polling ticker (check every 30 seconds)
	c.pollingTicker = time.NewTicker(30 * time.Second)

	c.isConnected.Store(true)

	// Start listening for events in a goroutine
	go c.handleEvents(ctx)

	// Start watching for file changes
	go c.watchResponsesFile(ctx)

	c.log.Debug("Chat feature started successfully.",
		zap.Int("responses", len(c.responses)),
		zap.String("responses_file", c.config.ResponsesFile),
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

	// Stop the polling ticker if it exists
	if c.pollingTicker != nil {
		c.pollingTicker.Stop()
	}

	// The watcher is closed in the watchResponsesFile goroutine
	// when it exits after receiving the stop signal

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
	for _, resp := range c.responses {
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
					err := c.slack.AddReactionContext(
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
					_, _, err := c.slack.PostMessageContext(
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

// AddResponse adds a new response pattern dynamically
func (c *Chat) AddResponse(pattern string, message string, isRegexp bool) error {
	if isRegexp {
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			return fmt.Errorf("invalid regular expression pattern: %w", err)
		}
		c.regexps[pattern] = re
	}

	c.responses = append(c.responses, Response{
		Pattern:  pattern,
		Message:  message,
		IsRegexp: isRegexp,
	})

	return nil
}

// AddResponse adds a new response pattern dynamically
func (c *Chat) readResponses() error {
	var responses []Response
	ext := path.Ext(c.config.ResponsesFile)
	switch ext {
	case ".json":
		if err := parseJSONFile(c.config.ResponsesFile, &responses); err != nil {
			return fmt.Errorf("parse responses file: %w", err)
		}
	case ".yaml", ".yml":
		if err := parseYAMLFile(c.config.ResponsesFile, &responses); err != nil {
			return fmt.Errorf("parse responses file: %w", err)
		}
	default:
		return fmt.Errorf("unsupported responses file format: %s", ext)
	}
	c.responses = responses

	// Compile regular expressions for faster matching
	for _, resp := range c.responses {
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

// watchResponsesFile monitors the responses file for changes
func (c *Chat) watchResponsesFile(ctx context.Context) {
	defer c.watcher.Close()

	for {
		select {
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		case <-c.pollingTicker.C:
			// Regular polling fallback for Docker/Alpine environments
			c.checkFileModification()
		case event, ok := <-c.watcher.Events:
			if !ok {
				return
			}

			// Check if this is a write or create event
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				c.checkFileModification()
			}
		case err, ok := <-c.watcher.Errors:
			if !ok {
				return
			}
			c.log.Error("File watcher error", zap.Error(err))
		}
	}
}

// checkFileModification checks if the file has been modified and reloads if needed
func (c *Chat) checkFileModification() {
	// Get file info to check modification time
	fileInfo, err := os.Stat(c.config.ResponsesFile)
	if err != nil {
		c.log.Error("Failed to stat responses file",
			zap.String("file", c.config.ResponsesFile),
			zap.Error(err),
		)
		return
	}

	// Only reload if the file was actually modified
	if fileInfo.ModTime().After(c.lastModTime) {
		c.lastModTime = fileInfo.ModTime()
		c.log.Info("Responses file changed, reloading",
			zap.String("file", c.config.ResponsesFile),
		)

		if err := c.readResponses(); err != nil {
			c.log.Error("Failed to reload responses file",
				zap.String("file", c.config.ResponsesFile),
				zap.Error(err),
			)
		} else {
			c.log.Info("Successfully reloaded responses",
				zap.String("file", c.config.ResponsesFile),
				zap.Int("count", len(c.responses)),
			)
		}
	}
}

func parseJSONFile(filePath string, v any) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read responses file: %w", err)
	}
	if err := json.Unmarshal(content, v); err != nil {
		return fmt.Errorf("unmarshal responses: %w", err)
	}
	return nil
}

func parseYAMLFile(filePath string, v any) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("open file: %w", err)
	}

	if err := yaml.Unmarshal(content, v); err != nil {
		return fmt.Errorf("unmarshal yaml: %w", err)
	}
	return nil
}

func randomString(values []string) string {
	rand.New(rand.NewSource(time.Now().UnixNano()))
	return values[rand.Intn(len(values))]
}
