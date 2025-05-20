package chat

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

// Response defines a pattern to match and the corresponding response
type Response struct {
	Pattern  string // Can be a plain text or a regular expression
	Message  string // The message to respond with
	IsRegexp bool   // Whether the pattern is a regular expression
}

// Config defines the configuration for the Chat feature
type Config struct {
	Responses []Response // List of configured responses
	UseRegexp bool       // Whether to use regular expressions for matching (global setting)
}

// Chat handles responding to messages based on configured patterns
type Chat struct {
	log       *zap.Logger
	config    Config
	client    *slack.Client
	regexps   map[string]*regexp.Regexp
	stopCh    chan struct{}
	eventsCh  chan slackevents.EventsAPIEvent
	connected bool
}

// ProcessorType returns a description of the processor type
func (t *Chat) ProcessorType() string {
	return "chat"
}

func NewTalk(log *zap.Logger, config Config) *Chat {
	t := &Chat{
		log:      log,
		config:   config,
		regexps:  make(map[string]*regexp.Regexp),
		stopCh:   make(chan struct{}),
		eventsCh: make(chan slackevents.EventsAPIEvent, 10),
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
			t.regexps[resp.Pattern] = re
		}
	}

	return t
}

// Start initializes the Chat feature with a Slack client
func (t *Chat) Start(ctx context.Context, client *slack.Client) error {
	if client == nil {
		return fmt.Errorf("slack client is not initialized")
	}

	t.client = client
	t.connected = true

	// Start listening for events in a goroutine
	go t.handleEvents(ctx)

	t.log.Info("Chat feature started successfully")
	return nil
}

// Stop stops the chat service
func (t *Chat) Stop(ctx context.Context) error {
	if !t.connected {
		return nil
	}

	close(t.stopCh)
	t.connected = false
	t.log.Info("Chat feature stopped")
	return nil
}

// PushEvent adds an event to be processed by the Chat feature
func (t *Chat) PushEvent(event slackevents.EventsAPIEvent) {
	if !t.connected {
		return
	}

	select {
	case t.eventsCh <- event:
		// Event pushed successfully
	default:
		t.log.Warn("Events channel full, dropping event")
	}
}

// handleEvents processes Slack events
func (t *Chat) handleEvents(ctx context.Context) {
	for {
		select {
		case <-t.stopCh:
			return
		case <-ctx.Done():
			return
		case event := <-t.eventsCh:
			t.processEvent(event)
		}
	}
}

// processEvent handles a single Slack event
func (t *Chat) processEvent(event slackevents.EventsAPIEvent) {
	switch event.Type {
	case slackevents.CallbackEvent:
		innerEvent := event.InnerEvent
		switch ev := innerEvent.Data.(type) {
		case *slackevents.MessageEvent:
			// Ignore bot messages to prevent loops
			if ev.BotID != "" || ev.User == "" {
				return
			}
			t.handleMessageEvent(ev)
		}
	}
}

// handleMessageEvent processes a message event and responds if it matches a pattern
func (t *Chat) handleMessageEvent(ev *slackevents.MessageEvent) {
	message := strings.TrimSpace(ev.Text)

	t.log.Debug("Processing message",
		zap.String("user", ev.User),
		zap.String("channel", ev.Channel),
		zap.String("text", message),
	)

	// Check if the message matches any of our configured responses
	for _, resp := range t.config.Responses {
		var matches bool

		if resp.IsRegexp {
			re, exists := t.regexps[resp.Pattern]
			if !exists {
				continue
			}
			matches = re.MatchString(message)
		} else {
			// Case-insensitive plain text match
			matches = strings.EqualFold(message, resp.Pattern)
		}

		if matches {
			t.log.Info("Message matched pattern",
				zap.String("pattern", resp.Pattern),
				zap.String("channel", ev.Channel),
			)

			_, _, err := t.client.PostMessage(
				ev.Channel,
				slack.MsgOptionText(resp.Message, false),
				slack.MsgOptionAsUser(true),
			)

			if err != nil {
				t.log.Error("Failed to post response",
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
func (t *Chat) AddResponse(pattern string, message string, isRegexp bool) error {
	if isRegexp {
		re, err := regexp.Compile("(?i)" + pattern)
		if err != nil {
			return fmt.Errorf("invalid regular expression pattern: %w", err)
		}
		t.regexps[pattern] = re
	}

	t.config.Responses = append(t.config.Responses, Response{
		Pattern:  pattern,
		Message:  message,
		IsRegexp: isRegexp,
	})

	return nil
}
