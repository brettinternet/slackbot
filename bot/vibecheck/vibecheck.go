package vibecheck

import (
	"context"
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
	DataDir        string
}

// Vibecheck handles responding to messages to verify the users vibe
type Vibecheck struct {
	log         *zap.Logger
	config      Config
	slack       *slack.Client
	isConnected atomic.Bool
	stopCh      chan struct{}
	eventsCh    chan slackevents.EventsAPIEvent
	kickedUsers *kickedUsersManager
	ticker      *time.Ticker
}

func NewVibecheck(log *zap.Logger, config Config, client *slack.Client) *Vibecheck {
	return &Vibecheck{
		log:         log,
		config:      config,
		stopCh:      make(chan struct{}),
		eventsCh:    make(chan slackevents.EventsAPIEvent, eventChannelSize),
		slack:       client,
		kickedUsers: newKickedUsersManager(log, config.DataDir),
		ticker:      time.NewTicker(30 * time.Second), // Check for users to reinvite every 30 seconds
	}
}

// ProcessorType returns a description of the processor type
func (c *Vibecheck) ProcessorType() string {
	return "vibecheck"
}

// Start initializes the Vibecheck feature with a Slack client
func (c *Vibecheck) Start(ctx context.Context) error {
	c.isConnected.Store(true)

	// Start listening for events in a goroutine
	go c.handleEvents(ctx)

	// Start the reinvite checker goroutine
	go c.checkReinvites(ctx)

	c.log.Debug("Vibecheck feature started successfully.")
	return nil
}

// Stop stops the Vibecheck service
func (c *Vibecheck) Stop(ctx context.Context) error {
	if !c.isConnected.Load() {
		return nil
	}

	c.ticker.Stop()
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
		zap.String("type", c.ProcessorType()),
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
		reaction := "vibecheck"
		if passed {
			reaction = "ok"
		}
		err := c.slack.AddReactionContext(ctx, reaction, slack.NewRefToMessage(ev.Channel, ev.TimeStamp))
		if err != nil {
			c.log.Error("Failed to add reaction",
				zap.String("channel", ev.Channel),
				zap.String("user", ev.User),
				zap.Error(err),
			)
		}

		response := randomResponse(passed)
		_, _, err = c.slack.PostMessageContext(
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

		if !passed && !slices.Contains(c.config.PreferredUsers, ev.User) && !slices.Contains(c.config.PreferredUsers, ev.Username) {
			time.AfterFunc(5*time.Second, func() {
				if err := c.slack.KickUserFromConversationContext(ctx, ev.Channel, ev.User); err != nil {
					c.log.Error("Failed to kick user from channel",
						zap.String("channel", ev.Channel),
						zap.String("user", ev.User),
						zap.Error(err),
					)
				} else {
					c.kickedUsers.AddKickedUser(ev.User, ev.Channel, 5*time.Minute)
					c.log.Info("User kicked from channel due to low vibe.",
						zap.String("channel", ev.Channel),
						zap.String("user", ev.User),
					)
				}
			})
		}
	}
}

// checkReinvites periodically checks for users to reinvite
func (c *Vibecheck) checkReinvites(ctx context.Context) {
	for {
		select {
		case <-c.stopCh:
			return
		case <-ctx.Done():
			return
		case <-c.ticker.C:
			c.processReinvites(ctx)
		}
	}
}

// processReinvites handles reinviting users who have been kicked
func (c *Vibecheck) processReinvites(ctx context.Context) {
	usersToReinvite := c.kickedUsers.GetUsersToReinvite()

	for _, user := range usersToReinvite {
		_, err := c.slack.InviteUsersToConversationContext(
			ctx,
			user.ChannelID,
			user.UserID,
		)

		if err != nil {
			c.log.Error("Failed to reinvite user to channel",
				zap.String("channel", user.ChannelID),
				zap.String("user", user.UserID),
				zap.Error(err),
			)
		} else {
			c.log.Info("Successfully reinvited user to channel after timeout",
				zap.String("channel", user.ChannelID),
				zap.String("user", user.UserID),
				zap.Time("kicked_at", user.KickedAt),
				zap.Time("reinvited_at", time.Now()),
			)
		}
	}

	c.kickedUsers.CleanupReinvitedUsers()
}
