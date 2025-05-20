package vibecheck

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

const eventChannelSize = 100

var pattern = regexp.MustCompile(`(?i).*vibe.*`)

type Config struct {
	PreferredUsers []string
}

// Vibecheck handles responding to messages to verify the users vibe
type Vibecheck struct {
	log         *zap.Logger
	config      Config
	client      *slack.Client
	isConnected atomic.Bool
	stopCh      chan struct{}
	eventsCh    chan slackevents.EventsAPIEvent
}

func NewVibecheck(log *zap.Logger, config Config) *Vibecheck {
	return &Vibecheck{
		log:      log,
		config:   config,
		stopCh:   make(chan struct{}),
		eventsCh: make(chan slackevents.EventsAPIEvent, eventChannelSize),
	}
}

// ProcessorType returns a description of the processor type
func (c *Vibecheck) ProcessorType() string {
	return "Vibecheck"
}

// Start initializes the Vibecheck feature with a Slack client
func (c *Vibecheck) Start(ctx context.Context, client *slack.Client) error {
	if client == nil {
		return fmt.Errorf("slack client is not initialized")
	}

	c.client = client
	c.isConnected.Store(true)

	// Start listening for events in a goroutine
	go c.handleEvents(ctx)

	c.log.Debug("Vibecheck feature started successfully.")
	return nil
}

// Stop stops the Vibecheck service
func (c *Vibecheck) Stop(ctx context.Context) error {
	if !c.isConnected.Load() {
		return nil
	}

	close(c.stopCh)
	c.isConnected.Store(false)
	return nil
}

// PushEvent adds an event to be processed by the Vibecheck feature
func (c *Vibecheck) PushEvent(event slackevents.EventsAPIEvent) {
	if !c.isConnected.Load() {
		return
	}

	select {
	case c.eventsCh <- event:
		// Event pushed successfully
	default:
		c.log.Warn("Vibecheck events channel full, dropping event.")
	}
}

// handleEvents processes Slack events
func (c *Vibecheck) handleEvents(ctx context.Context) {
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
func (c *Vibecheck) processEvent(ctx context.Context, event slackevents.EventsAPIEvent) {
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
func (c *Vibecheck) handleMessageEvent(ctx context.Context, ev *slackevents.MessageEvent) {
	message := strings.TrimSpace(ev.Text)

	c.log.Debug("Processing message",
		zap.String("user", ev.User),
		zap.String("channel", ev.Channel),
		zap.String("text", message),
	)

	if pattern.MatchString(message) {
		c.log.Info("Message matched vibecheck pattern.",
			zap.String("channel", ev.Channel),
		)

		weight := 0.8
		if time.Now().Local().Weekday() == time.Wednesday {
			weight = 0.2
		}

		passed := randomBool(weight)
		response := randomResponse(passed)
		_, _, err := c.client.PostMessageContext(
			ctx,
			ev.Channel,
			slack.MsgOptionText(response, false),
			slack.MsgOptionAsUser(true), // legacy only
		)
		if err != nil {
			c.log.Error("Failed to post response",
				zap.String("channel", ev.Channel),
				zap.Error(err),
			)
		}

		if !passed && !slices.Contains(c.config.PreferredUsers, ev.User) {
			time.AfterFunc(5*time.Second, func() {
				if err := c.client.KickUserFromConversationContext(ctx, ev.Channel, ev.User); err != nil {
					c.log.Error("Failed to kick user from channel",
						zap.String("channel", ev.Channel),
						zap.String("user", ev.User),
						zap.Error(err),
					)
				} else {
					c.log.Info("User kicked from channel due to low vibe.",
						zap.String("channel", ev.Channel),
						zap.String("user", ev.User),
					)
				}
			})
		}
	}
}
