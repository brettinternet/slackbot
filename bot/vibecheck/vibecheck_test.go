package vibecheck

import (
	"context"
	"testing"
	"time"

	"github.com/slack-go/slack"
	"github.com/slack-go/slack/slackevents"
	"go.uber.org/zap"
)

// mockSlackService implements the slackService interface for testing
type mockSlackService struct {
	client *slack.Client
}

func (m *mockSlackService) Client() *slack.Client {
	return m.client
}

func TestConfig_BanDuration(t *testing.T) {
	config := Config{
		PreferredUsers: []string{"user1"},
		DataDir:        "/tmp/test",
		BanDuration:    10 * time.Minute,
	}

	if config.BanDuration != 10*time.Minute {
		t.Errorf("Expected ban duration to be 10 minutes, got %v", config.BanDuration)
	}
}

func TestIsUserBanned(t *testing.T) {
	logger := zap.NewNop()
	manager := newKickedUsersManager(logger, "/tmp/test")

	// Test user not banned
	_, isBanned := manager.IsUserBanned("user1", "channel1")
	if isBanned {
		t.Error("Expected user to not be banned")
	}

	// Add banned user
	manager.AddKickedUser("user1", "channel1", 5*time.Minute)

	// Test user is banned
	user, isBanned := manager.IsUserBanned("user1", "channel1")
	if !isBanned {
		t.Error("Expected user to be banned")
	}

	if user.UserID != "user1" {
		t.Errorf("Expected user ID to be user1, got %s", user.UserID)
	}

	if user.ChannelID != "channel1" {
		t.Errorf("Expected channel ID to be channel1, got %s", user.ChannelID)
	}

	// Test expired ban
	manager.AddKickedUser("user2", "channel1", -1*time.Minute) // Already expired
	_, isBanned = manager.IsUserBanned("user2", "channel1")
	if isBanned {
		t.Error("Expected expired ban to not be active")
	}
}

func TestHandleMemberJoinedEvent_UserNotBanned(t *testing.T) {
	logger := zap.NewNop()
	mockSlack := &mockSlackService{}
	
	config := Config{
		PreferredUsers: []string{},
		DataDir:        "/tmp/test",
		BanDuration:    5 * time.Minute,
	}

	vibecheck := &Vibecheck{
		log:         logger,
		config:      config,
		slack:       mockSlack,
		kickedUsers: newKickedUsersManager(logger, "/tmp/test"),
	}

	ctx := context.Background()
	event := &slackevents.MemberJoinedChannelEvent{
		User:    "user1",
		Channel: "channel1",
	}

	// Should not do anything for non-banned user
	vibecheck.handleMemberJoinedEvent(ctx, event)

	// In a real test, we'd verify no kick operations occurred
	// For now, we just verify the method runs without error
}

func TestHandleMemberJoinedEvent_UserBanned(t *testing.T) {
	logger := zap.NewNop()
	mockSlack := &mockSlackService{}
	
	config := Config{
		PreferredUsers: []string{},
		DataDir:        "/tmp/test",
		BanDuration:    5 * time.Minute,
	}

	vibecheck := &Vibecheck{
		log:         logger,
		config:      config,
		slack:       mockSlack,
		kickedUsers: newKickedUsersManager(logger, "/tmp/test"),
	}

	// Ban user first
	vibecheck.kickedUsers.AddKickedUser("user1", "channel1", 5*time.Minute)

	ctx := context.Background()
	event := &slackevents.MemberJoinedChannelEvent{
		User:    "user1",
		Channel: "channel1",
	}

	vibecheck.handleMemberJoinedEvent(ctx, event)

	// Note: In a real test we'd need to wait for the AfterFunc to execute
	// or mock time.AfterFunc, but this tests the logic path
}