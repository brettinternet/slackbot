package chat

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync/atomic"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

const eventChannelSize = 100

// Response defines a pattern to match and the corresponding response
type Response struct {
	Pattern  string // Can be a plain text or a regular expression
	Message  string // The message to respond with
	IsRegexp bool   // Whether the pattern is a regular expression
}

// Config defines the configuration for the Chat feature
type Config struct {
	Responses      []Response
	UseRegexp      bool
	PreferredUsers []string
}

// Chat handles responding to messages based on configured patterns
type Chat struct {
	log         *zap.Logger
	config      Config
	client      *slack.Client
	regexps     map[string]*regexp.Regexp
	stopCh      chan struct{}
	eventsCh    chan slackevents.EventsAPIEvent
	isConnected atomic.Bool
}

// ProcessorType returns a description of the processor type
func (c *Chat) ProcessorType() string {
	return "chat"
}

func NewChat(log *zap.Logger, config Config) *Chat {
	c := &Chat{
		log:      log,
		config:   config,
		regexps:  make(map[string]*regexp.Regexp),
		stopCh:   make(chan struct{}),
		eventsCh: make(chan slackevents.EventsAPIEvent, eventChannelSize),
	}

	// Compile regular expressions for faster matching
	for _, resp := range config.Responses {
		if resp.IsRegexp {
			re, err := regexp.Compile("(?i)" + resp.Pattern)
			if err != nil {
				log.Error("Failed to compile regex pattern",
					zap.String("pattern", resp.Pattern),
					zap.Error(err),
				)
				continue
			}
			c.regexps[resp.Pattern] = re
		}
	}

	return c
}

// Start initializes the Chat feature with a Slack client
func (c *Chat) Start(ctx context.Context, client *slack.Client) error {
	if client == nil {
		return fmt.Errorf("slack client is not initialized")
	}

	c.client = client
	c.isConnected.Store(true)

	// Start listening for events in a goroutine
	go c.handleEvents(ctx)

	c.log.Debug("Chat feature started successfully.")
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
			c.processEvent(event)
		}
	}
}

// processEvent handles a single Slack event
func (c *Chat) processEvent(event slackevents.EventsAPIEvent) {
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			// Ignore bot messages to prevent loops
			if ev.BotID != "" || ev.User == "" {
				return
			}
			c.handleMessageEvent(ev)
		}
	}
}

// handleMessageEvent processes a message event and responds if it matches a pattern
func (c *Chat) handleMessageEvent(ev *slackevents.MessageEvent) {
	message := strings.TrimSpace(ev.Text)

	c.log.Debug("Processing message",
		zap.String("user", ev.User),
		zap.String("channel", ev.Channel),
		zap.String("text", message),
	)

	// Check if the message matches any of our configured responses
	for _, resp := range c.config.Responses {
		var matches bool

		if resp.IsRegexp {
			re, exists := c.regexps[resp.Pattern]
			if !exists {
				continue
			}
			matches = re.MatchString(message)
		} else {
			// Case-insensitive plain text match
			matches = strings.EqualFold(message, resp.Pattern)
		}

		if matches {
			c.log.Info("Message matched pattern",
				zap.String("pattern", resp.Pattern),
				zap.String("channel", ev.Channel),
			)

			_, _, err := c.client.PostMessage(
				ev.Channel,
				slack.MsgOptionText(resp.Message, false),
				slack.MsgOptionAsUser(true),
			)

			if err != nil {
				c.log.Error("Failed to post response",
					zap.String("channel", ev.Channel),
					zap.Error(err),
				)
			}

			// Stop after the first match
			break
		}
	}
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

	c.config.Responses = append(c.config.Responses, Response{
		Pattern:  pattern,
		Message:  message,
		IsRegexp: isRegexp,
	})

	return nil
}
