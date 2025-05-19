package obituary

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

const watchInterval = 1 * time.Minute

type Config struct {
	NotifyChannel string
	DataDir       string
}

// User represents a simplified Slack user for persistence
type User struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	RealName string `json:"real_name"`
}

type Obituary struct {
	log           *zap.Logger
	slack         *slack.Client
	notifyChannel string
	ticker        *time.Ticker
	cancel        context.CancelFunc
	mutex         sync.Mutex
	knownUsers    map[string]*slack.User
	usersFile     string
}

func NewObituary(log *zap.Logger, config Config) *Obituary {
	// Make sure DataDir exists
	if config.DataDir != "" {
		if _, err := os.Stat(config.DataDir); os.IsNotExist(err) {
			if err := os.MkdirAll(config.DataDir, 0755); err != nil {
				log.Error("Failed to create data directory", zap.String("dir", config.DataDir), zap.Error(err))
			}
		}
	}

	usersFile := ""
	if config.DataDir != "" {
		usersFile = filepath.Join(config.DataDir, "obituary_users.json")
	}

	return &Obituary{
		log:           log,
		notifyChannel: config.NotifyChannel,
		knownUsers:    make(map[string]*slack.User),
		usersFile:     usersFile,
	}
}

func (o *Obituary) Start(ctx context.Context, slackClient *slack.Client) error {
	if o.notifyChannel == "" {
		return fmt.Errorf("notification channel is not set")
	}
	o.slack = slackClient

	ctx, cancel := context.WithCancel(ctx)
	o.cancel = cancel

	o.ticker = time.NewTicker(watchInterval)

	previousUsers, err := o.loadUsersFromDisk()
	if err != nil {
		o.log.Warn("Failed to load previous users from disk", zap.Error(err))
	}

	if err := o.fetchAllUsers(); err != nil {
		return fmt.Errorf("fetch initial user list: %w", err)
	}

	if len(previousUsers) > 0 {
		o.log.Debug("Checking for users deleted while service was down",
			zap.Int("previous_count", len(previousUsers)),
			zap.Int("current_count", len(o.knownUsers)))

		var deletedUsers []slack.User
		for id, user := range previousUsers {
			if _, exists := o.knownUsers[id]; !exists {
				deletedUsers = append(deletedUsers, *user)
			}
		}

		for _, user := range deletedUsers {
			o.notifyUserDeleted(ctx, &user)
		}

		if len(deletedUsers) > 0 {
			o.log.Info("Detected users deleted while service was down",
				zap.Int("count", len(deletedUsers)))
		}
	}

	if err := o.saveUsersToDisk(); err != nil {
		o.log.Warn("Failed to save initial users to disk", zap.Error(err))
	}

	o.log.Debug("Obituary service started, monitoring for deleted users")

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
	if err := o.saveUsersToDisk(); err != nil {
		o.log.Warn("Failed to save users before stopping", zap.Error(err))
	}

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

	users, err = o.slack.GetUsers()
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

	o.mutex.Lock()
	currentUsers := make(map[string]*slack.User)
	for id, user := range o.knownUsers {
		currentUsers[id] = user
	}
	o.mutex.Unlock()

	users, err := o.slack.GetUsers()
	if err != nil {
		return err
	}

	newUserMap := make(map[string]slack.User)
	for _, user := range users {
		newUserMap[user.ID] = user
	}

	var deletedUsers []slack.User
	for id, user := range currentUsers {
		if _, exists := newUserMap[id]; !exists {
			deletedUsers = append(deletedUsers, *user)
		}
	}

	// Check for new or modified users
	hasChanges := len(deletedUsers) > 0
	if !hasChanges {
		for id, newUser := range newUserMap {
			if oldUser, exists := currentUsers[id]; !exists ||
				oldUser.Name != newUser.Name ||
				oldUser.RealName != newUser.RealName {
				hasChanges = true
				break
			}
		}
	}

	o.mutex.Lock()
	o.knownUsers = make(map[string]*slack.User)
	for id, user := range newUserMap {
		userCopy := user // Create a copy to avoid reference issues
		o.knownUsers[id] = &userCopy
	}
	o.mutex.Unlock()

	for _, user := range deletedUsers {
		o.notifyUserDeleted(ctx, &user)
	}

	if len(deletedUsers) > 0 {
		o.log.Info("Detected deleted users", zap.Int("count", len(deletedUsers)))
	} else {
		o.log.Debug("No deleted users detected")
	}

	// Only save to disk if there were changes
	if hasChanges {
		o.log.Debug("Changes detected in user list, saving to disk")
		if err := o.saveUsersToDisk(); err != nil {
			o.log.Warn("Failed to save users to disk", zap.Error(err))
		}
	} else {
		o.log.Debug("No changes in user list, skipping save to disk")
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

	_, _, err := o.slack.PostMessage(
		o.notifyChannel,
		slack.MsgOptionAttachments(attachment),
		slack.MsgOptionAsUser(true),
	)
	if err != nil {
		o.log.Error("send notification", zap.Error(err), zap.String("channel", o.notifyChannel))
	}
}

// saveUsersToDisk saves the current known users to disk
func (o *Obituary) saveUsersToDisk() error {
	if o.usersFile == "" {
		o.log.Debug("No users file configured, skipping save")
		return nil
	}

	o.mutex.Lock()
	defer o.mutex.Unlock()

	users := make([]User, 0, len(o.knownUsers))
	for _, user := range o.knownUsers {
		users = append(users, User{
			ID:       user.ID,
			Name:     user.Name,
			RealName: user.RealName,
		})
	}

	tempFile := o.usersFile + ".tmp"

	data, err := json.MarshalIndent(users, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal users: %w", err)
	}

	if err := os.WriteFile(tempFile, data, 0644); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	if err := os.Rename(tempFile, o.usersFile); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	o.log.Debug("Saved users to disk", zap.String("file", o.usersFile), zap.Int("count", len(users)))
	return nil
}

func (o *Obituary) loadUsersFromDisk() (map[string]*slack.User, error) {
	if o.usersFile == "" {
		o.log.Debug("No users file configured, skipping load")
		return nil, nil
	}

	if _, err := os.Stat(o.usersFile); os.IsNotExist(err) {
		o.log.Debug("Users file doesn't exist, no previous state to load", zap.String("file", o.usersFile))
		return nil, nil
	}

	data, err := os.ReadFile(o.usersFile)
	if err != nil {
		return nil, fmt.Errorf("read users file: %w", err)
	}

	var users []User
	if err := json.Unmarshal(data, &users); err != nil {
		return nil, fmt.Errorf("unmarshal users: %w", err)
	}

	result := make(map[string]*slack.User)
	for _, user := range users {
		result[user.ID] = &slack.User{
			ID:       user.ID,
			Name:     user.Name,
			RealName: user.RealName,
		}
	}

	o.log.Debug("Loaded users from disk", zap.String("file", o.usersFile), zap.Int("count", len(result)))
	return result, nil
}
