package obituary

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

type Config struct {
	NotifyChannel string
	DataDir       string
}

type Obituary struct {
	log           *zap.Logger
	client        *slack.Client
	notifyChannel string
	ticker        *time.Ticker
	cancel        context.CancelFunc
	mutex         sync.Mutex
	knownUsers    map[string]*slack.User
}

func NewObituary(log *zap.Logger, client *slack.Client, config Config) *Obituary {
	return &Obituary{
		log:           log,
		client:        client,
		notifyChannel: config.NotifyChannel,
		knownUsers:    make(map[string]*slack.User),
	}
}

func (o *Obituary) Start(ctx context.Context) error {
	if o.notifyChannel == "" {
		return fmt.Errorf("notification channel is not set")
	}

	// Create a cancellable context for our goroutine
	ctx, cancel := context.WithCancel(ctx)
	o.cancel = cancel

	// Set up a ticker to check for deleted users every 5 minutes
	o.ticker = time.NewTicker(5 * time.Minute)

	// First, fetch all current users and store them
	if err := o.fetchAllUsers(); err != nil {
		return fmt.Errorf("fetch initial user list: %w", err)
	}

	o.log.Debug("Obituary service started, monitoring for deleted users")

	// Start a goroutine to periodically check for deleted users
	go func() {
		defer o.ticker.Stop()
		for {
			select {
			case <-o.ticker.C:
				if err := o.checkForDeletedUsers(ctx); err != nil {
					o.log.Error("Error checking for deleted users", zap.Error(err))
				}
			case <-ctx.Done():
				o.log.Debug("Obituary service stopping")
				return
			}
		}
	}()

	return nil
}

func (o *Obituary) Stop(ctx context.Context) error {
	if o.cancel != nil {
		o.cancel()
	}
	if o.ticker != nil {
		o.ticker.Stop()
	}
	o.log.Debug("Obituary service stopped")
	return nil
}

// fetchAllUsers gets all users from the Slack workspace and stores them in a map
func (o *Obituary) fetchAllUsers() error {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	o.log.Debug("Fetching all users from Slack")

	var users []slack.User
	var err error

	users, err = o.client.GetUsers()
	if err != nil {
		return err
	}

	// Update our known users map
	for _, user := range users {
		o.knownUsers[user.ID] = &user
	}

	o.log.Debug("Fetched all users", zap.Int("count", len(o.knownUsers)))
	return nil
}

// checkForDeletedUsers compares the current user list with our stored list
func (o *Obituary) checkForDeletedUsers(ctx context.Context) error {
	o.log.Debug("Checking for deleted users")

	// Make a copy of the current user map to avoid modifying while iterating
	o.mutex.Lock()
	currentUsers := make(map[string]*slack.User)
	for id, user := range o.knownUsers {
		currentUsers[id] = user
	}
	o.mutex.Unlock()

	// Get the current list of users from Slack
	users, err := o.client.GetUsers()
	if err != nil {
		return err
	}

	// Create a map of current users for quick lookup
	newUserMap := make(map[string]slack.User)
	for _, user := range users {
		newUserMap[user.ID] = user
	}

	// Check if any users from our stored map are missing from the new list
	var deletedUsers []slack.User
	for id, user := range currentUsers {
		if _, exists := newUserMap[id]; !exists {
			deletedUsers = append(deletedUsers, *user)
		}
	}

	// Update our stored map with the new list
	o.mutex.Lock()
	o.knownUsers = make(map[string]*slack.User)
	for id, user := range newUserMap {
		userCopy := user // Create a copy to avoid reference issues
		o.knownUsers[id] = &userCopy
	}
	o.mutex.Unlock()

	// Notify about deleted users
	for _, user := range deletedUsers {
		o.notifyUserDeleted(ctx, &user)
	}

	if len(deletedUsers) > 0 {
		o.log.Info("Detected deleted users", zap.Int("count", len(deletedUsers)))
	} else {
		o.log.Debug("No deleted users detected")
	}

	return nil
}

// notifyUserDeleted sends a notification to the configured channel about a deleted user
func (o *Obituary) notifyUserDeleted(ctx context.Context, user *slack.User) {
	if o.notifyChannel == "" {
		o.log.Warn("No notification channel configured, skipping notification")
		return
	}

	o.log.Info("User deleted", zap.String("user_id", user.ID), zap.String("user_name", user.RealName))

	// Create a rich message with a user profile link
	var message string
	if user.RealName != "" {
		message = fmt.Sprintf("User *%s* (%s) has been deleted from the Slack organization.",
			user.RealName, user.Name)
	} else {
		message = fmt.Sprintf("User *%s* has been deleted from the Slack organization.",
			user.Name)
	}

	profileLink := fmt.Sprintf("https://slack.com/team/%s", user.ID)

	attachment := slack.Attachment{
		Color:      "#FF5733", // Red-orange color
		Text:       message,
		Footer:     fmt.Sprintf("User ID: %s", user.ID),
		FooterIcon: "https://platform.slack-edge.com/img/default_application_icon.png",
		Ts:         json.Number(fmt.Sprintf("%d", time.Now().Unix())),
		Actions: []slack.AttachmentAction{
			{
				Type: "button",
				Text: "View Profile",
				URL:  profileLink,
			},
		},
	}

	// Send the message to the notification channel
	_, _, err := o.client.PostMessage(
		o.notifyChannel,
		slack.MsgOptionAttachments(attachment),
		slack.MsgOptionAsUser(true),
	)
	if err != nil {
		o.log.Error("send notification", zap.Error(err), zap.String("channel", o.notifyChannel))
	}
}
