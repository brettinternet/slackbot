package vibecheck

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestConfig_BanDuration(t *testing.T) {
	config := Config{
		BanDuration: 10 * time.Minute,
	}

	if config.BanDuration != 10*time.Minute {
		t.Errorf("Expected ban duration to be 10 minutes, got %v", config.BanDuration)
	}
}

func TestIsUserBanned(t *testing.T) {
	logger := zap.NewNop()
	manager := newKickedUsersManager(logger, "/tmp/test-"+time.Now().Format("20060102150405"))

	// Test user not banned initially
	_, isBanned := manager.IsUserBanned("testuser1", "testchannel1")
	if isBanned {
		t.Error("Expected user to not be banned initially")
	}

	// Add banned user
	manager.AddKickedUser("testuser1", "testchannel1", 5*time.Minute)

	// Test user is banned
	user, isBanned := manager.IsUserBanned("testuser1", "testchannel1")
	if !isBanned {
		t.Error("Expected user to be banned after adding")
	}

	if user.UserID != "testuser1" {
		t.Errorf("Expected user ID to be testuser1, got %s", user.UserID)
	}

	if user.ChannelID != "testchannel1" {
		t.Errorf("Expected channel ID to be testchannel1, got %s", user.ChannelID)
	}

	// Test expired ban
	manager.AddKickedUser("testuser2", "testchannel1", -1*time.Minute) // Already expired
	_, isBanned = manager.IsUserBanned("testuser2", "testchannel1")
	if isBanned {
		t.Error("Expected expired ban to not be active")
	}
}

func TestHandleMemberJoinedEvent_UserNotBanned(t *testing.T) {
	logger := zap.NewNop()
	manager := newKickedUsersManager(logger, "/tmp/test-nonbanned-"+time.Now().Format("20060102150405"))

	// This test verifies that non-banned users are correctly identified as not banned
	_, isBanned := manager.IsUserBanned("testuser3", "testchannel3")
	if isBanned {
		t.Error("User should not be banned initially")
	}
}

func TestHandleMemberJoinedEvent_UserBanned(t *testing.T) {
	logger := zap.NewNop()
	manager := newKickedUsersManager(logger, "/tmp/test-banned-"+time.Now().Format("20060102150405"))

	// Ban user first
	manager.AddKickedUser("testuser4", "testchannel4", 5*time.Minute)

	// Test that the user is now considered banned
	user, isBanned := manager.IsUserBanned("testuser4", "testchannel4")
	if !isBanned {
		t.Error("User should be banned after adding to kicked users")
	}
	
	if user.UserID != "testuser4" {
		t.Errorf("Expected banned user ID to be testuser4, got %s", user.UserID)
	}
	
	if user.ChannelID != "testchannel4" {
		t.Errorf("Expected banned channel ID to be testchannel4, got %s", user.ChannelID)
	}

	// Test that time remaining is positive
	timeRemaining := time.Until(user.ReinviteAt)
	if timeRemaining <= 0 {
		t.Error("Expected positive time remaining for banned user")
	}
}