package obituary

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"maps"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

const watchInterval = 1 * time.Minute

type slackService interface {
	Client() *slack.Client
	OrgURL() string
}

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
	slack         slackService
	notifyChannel string
	ticker        *time.Ticker
	cancel        context.CancelFunc
	mutex         sync.Mutex
	knownUsers    map[string]*slack.User
	usersFile     string
}

func NewObituary(log *zap.Logger, c Config, s slackService) *Obituary {
	if c.DataDir != "" {
		if _, err := os.Stat(c.DataDir); os.IsNotExist(err) {
			if err := os.MkdirAll(c.DataDir, 0755); err != nil {
				log.Error("Failed to create data directory", zap.String("dir", c.DataDir), zap.Error(err))
			}
		}
	}

	usersFile := ""
	if c.DataDir != "" {
		usersFile = filepath.Join(c.DataDir, "obituary_users.json")
	}

	return &Obituary{
		log:           log,
		notifyChannel: c.NotifyChannel,
		knownUsers:    make(map[string]*slack.User),
		usersFile:     usersFile,
		slack:         s,
	}
}

func (o *Obituary) Start(ctx context.Context) error {
	if o.notifyChannel == "" {
		return fmt.Errorf("notification channel is not set")
	}

	// Verify the channel format and existence early
	if !o.validateChannel(ctx) {
		o.log.Warn("Continuing despite invalid notification channel", zap.String("channel", o.notifyChannel))
	}

	ctx, cancel := context.WithCancel(ctx)
	o.cancel = cancel

	o.ticker = time.NewTicker(watchInterval)

	previousUsers, err := o.loadUsersFromDisk()
	if err != nil {
		o.log.Warn("Failed to load previous users from disk", zap.Error(err))
	}

	if err := o.fetchAllUsers(ctx); err != nil {
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

	o.sendStartupMessage(ctx)

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
	return nil
}

// fetchAllUsers gets all users from the Slack workspace and stores them in a map
func (o *Obituary) fetchAllUsers(ctx context.Context) error {
	o.mutex.Lock()
	defer o.mutex.Unlock()

	o.log.Debug("Fetching all users from Slack")

	var users []slack.User
	var err error

	users, err = o.slack.Client().GetUsersContext(ctx)
	if err != nil {
		return err
	}

	// Update our known users map
	for _, user := range users {
		if !isValidUser(user) {
			continue
		}
		o.knownUsers[user.ID] = &user
	}

	o.log.Debug("Fetched all users", zap.Int("count", len(o.knownUsers)))
	return nil
}

func isValidUser(user slack.User) bool {
	return user.ID != "" && !user.Deleted && !user.IsBot
}

// checkForDeletedUsers compares the current user list with our stored list
func (o *Obituary) checkForDeletedUsers(ctx context.Context) error {
	o.log.Debug("Checking for deleted users")

	o.mutex.Lock()
	currentUsers := make(map[string]*slack.User)
	maps.Copy(currentUsers, o.knownUsers)
	o.mutex.Unlock()

	users, err := o.slack.Client().GetUsersContext(ctx)
	if err != nil {
		return err
	}

	newUserMap := make(map[string]slack.User)
	for _, user := range users {
		if !isValidUser(user) {
			continue
		}
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
		o.log.Info("Detected deleted users.", zap.Int("count", len(deletedUsers)))
	}

	if hasChanges {
		o.log.Debug("Changes detected in user list, saving to disk.")
		if err := o.saveUsersToDisk(); err != nil {
			o.log.Warn("Failed to save users to disk.", zap.Error(err))
		}
	}

	return nil
}

// TODO: batch attachments together in single message for multiple users
// notifyUserDeleted sends a notification to the configured channel about a deleted user
func (o *Obituary) notifyUserDeleted(ctx context.Context, user *slack.User) {
	o.log.Info("User deleted.", zap.String("user_id", user.ID), zap.String("user_name", user.RealName))

	var identity string
	if user.RealName != "" && user.RealName != user.Name {
		identity = fmt.Sprintf("*%s* (%s)", user.RealName, user.Name)
	} else {
		identity = fmt.Sprintf("*%s*", user.Name)
	}
	userTitle := "User"
	if user.IsBot {
		userTitle = "Bot"
	}

	message := fmt.Sprintf("%s %s has been deleted from the Slack organization.", userTitle, identity)
	profileLink := fmt.Sprintf("%steam/%s", o.slack.OrgURL(), user.ID)
	actions := []slack.AttachmentAction{
		{
			Type: "button",
			Text: "View Profile",
			URL:  profileLink,
		},
	}
	if user.Profile.RealName != "" && !user.IsBot {
		actions = append(actions, slack.AttachmentAction{
			Type: "button",
			Text: "View LinkedIn",
			URL:  linkedinURL(user.Profile.RealName),
		})
	}
	attachment := slack.Attachment{
		Color:      "#FF5733", // Red-orange color
		Title:      fmt.Sprintf(":rip: %s Deleted", userTitle),
		Text:       message,
		Footer:     fmt.Sprintf("%s ID: %s; Monitoring %d remaining users", userTitle, user.ID, len(o.knownUsers)),
		FooterIcon: "https://platform.slack-edge.com/img/default_application_icon.png",
		Ts:         json.Number(fmt.Sprintf("%d", time.Now().Unix())),
		Actions:    actions,
	}

	_, _, err := o.slack.Client().PostMessageContext(
		ctx,
		o.notifyChannel,
		slack.MsgOptionAttachments(attachment),
		slack.MsgOptionAsUser(true),
	)
	if err != nil {
		o.log.Error("send notification", zap.Error(err), zap.String("channel", o.notifyChannel))
	}
}

func linkedinURL(name string) string {
	return fmt.Sprintf("https://www.linkedin.com/search/results/people/?keywords=%s", url.PathEscape(name))
}

// sendStartupMessage sends a notification to the configured channel to confirm the bot is running
// but only if the bot hasn't posted any messages to the channel before
func (o *Obituary) sendStartupMessage(ctx context.Context) {
	authTest, err := o.slack.Client().AuthTestContext(ctx)
	if err != nil {
		o.log.Error("Failed to get bot identity, sending notification anyway", zap.Error(err))
	} else {
		botUserID := authTest.UserID
		o.log.Debug("Bot identity", zap.String("user_id", botUserID), zap.String("bot_name", authTest.User))

		if !o.validateChannel(ctx) {
			o.log.Error("Skipping startup notification due to channel validation failure")
			return
		}

		history, err := o.slack.Client().GetConversationHistoryContext(ctx, &slack.GetConversationHistoryParameters{
			ChannelID: o.notifyChannel,
			Limit:     100,
		})
		if err != nil {
			o.log.Error("Failed to get channel history, sending notification anyway",
				zap.Error(err),
				zap.String("channel", o.notifyChannel))
		} else {
			for _, msg := range history.Messages {
				if (msg.User == botUserID || (msg.BotID != "" && msg.Username == authTest.User)) && isIntroMessage(msg) {
					o.log.Debug("Bot has already posted messages to the channel, skipping startup notification")
					return
				}
			}
		}
	}

	o.log.Info("Sending startup notification", zap.String("channel", o.notifyChannel))

	attachment := slack.Attachment{
		Color:      "#36a64f", // Green color
		Title:      "Status",
		Text:       "🟢 *Slack user obituary feature is now running*",
		Footer:     fmt.Sprintf("Monitoring %d users", len(o.knownUsers)),
		FooterIcon: "https://platform.slack-edge.com/img/default_application_icon.png",
		Ts:         json.Number(fmt.Sprintf("%d", time.Now().Unix())),
	}

	_, _, err = o.slack.Client().PostMessageContext(
		ctx,
		o.notifyChannel,
		slack.MsgOptionAttachments(attachment),
		slack.MsgOptionAsUser(true),
	)
	if err != nil {
		// Log the channel ID for debugging purposes
		o.log.Error("Failed to send startup notification - check that the channel ID is correct and in the format 'C0123456789'",
			zap.Error(err),
			zap.String("channel", o.notifyChannel))

		// The channel ID is likely incorrect. Let's output some recommendations.
		if err.Error() == "channel_not_found" {
			o.log.Warn("The channel may not exist or the bot may not have been added to the channel.",
				zap.String("channel", o.notifyChannel),
				zap.String("recommendation", "Make sure to invite the bot to the channel or check the channel ID"))
		}
	}
}

func isIntroMessage(msg slack.Message) bool {
	for _, a := range msg.Attachments {
		if a.Title == "Status" && strings.Contains(a.Text, "user obituary feature") {
			return true
		}
	}
	return false
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

func (o *Obituary) validateChannel(ctx context.Context) bool {
	if len(o.notifyChannel) < 9 || !strings.HasPrefix(o.notifyChannel, "C") {
		o.log.Warn("Channel ID format may be invalid - should typically be 'C' followed by alphanumeric chars",
			zap.String("channel", o.notifyChannel))
	}

	_, err := o.slack.Client().GetConversationInfoContext(ctx, &slack.GetConversationInfoInput{
		ChannelID:     o.notifyChannel,
		IncludeLocale: false,
	})
	if err != nil {
		o.log.Error("Channel not found or not accessible - check the channel ID and bot permissions",
			zap.Error(err),
			zap.String("channel", o.notifyChannel),
			zap.String("recommendation", "Make sure to invite the bot to the channel"))
		return false
	}

	o.log.Debug("Channel validation successful", zap.String("channel", o.notifyChannel))
	return true
}
