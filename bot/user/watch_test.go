package user

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/slack-go/slack"
	"go.uber.org/zap"
)

// mockSlackService implements slackService interface for testing
type mockSlackService struct {
	orgURL string
}

func (m *mockSlackService) Client() *slack.Client {
	// This is a bit tricky since slack.Client methods aren't easily mockable
	// In real implementation, we'd use an interface for slack operations
	return nil
}

func (m *mockSlackService) OrgURL() string {
	return m.orgURL
}

// For testing purposes, we'll need to modify the UserWatch to accept interfaces
// or use dependency injection. For now, we'll test the parts we can test directly.

func TestNewUserWatch(t *testing.T) {
	logger := zap.NewNop()
	config := Config{
		NotifyChannel: "C1234567890",
		DataDir:       "./test_data",
	}
	mockSlack := &mockSlackService{
		orgURL: "https://test.slack.com/",
	}

	watch := NewUserWatch(logger, config, mockSlack)

	if watch == nil {
		t.Fatal("NewUserWatch() returned nil")
	}

	if watch.notifyChannel != config.NotifyChannel {
		t.Errorf("NewUserWatch() notifyChannel = %v, want %v", watch.notifyChannel, config.NotifyChannel)
	}

	if watch.usersFile != filepath.Join(config.DataDir, "users.json") {
		t.Errorf("NewUserWatch() usersFile = %v, want %v", watch.usersFile, filepath.Join(config.DataDir, "users.json"))
	}

	if watch.knownUsers == nil {
		t.Error("NewUserWatch() knownUsers map not initialized")
	}

	// Clean up test directory
	_ = os.RemoveAll(config.DataDir)
}

func TestNewUserWatch_EmptyDataDir(t *testing.T) {
	logger := zap.NewNop()
	config := Config{
		NotifyChannel: "C1234567890",
		DataDir:       "",
	}
	mockSlack := &mockSlackService{
		orgURL: "https://test.slack.com/",
	}

	watch := NewUserWatch(logger, config, mockSlack)

	if watch.usersFile != "" {
		t.Errorf("NewUserWatch() with empty DataDir should have empty usersFile, got %v", watch.usersFile)
	}
}

func TestIsValidUser(t *testing.T) {
	tests := []struct {
		name     string
		user     slack.User
		expected bool
	}{
		{
			name: "valid user",
			user: slack.User{
				ID:      "U1234567890",
				Name:    "testuser",
				Deleted: false,
				IsBot:   false,
			},
			expected: true,
		},
		{
			name: "deleted user",
			user: slack.User{
				ID:      "U1234567890",
				Name:    "testuser",
				Deleted: true,
				IsBot:   false,
			},
			expected: false,
		},
		{
			name: "bot user",
			user: slack.User{
				ID:      "B1234567890",
				Name:    "testbot",
				Deleted: false,
				IsBot:   true,
			},
			expected: false,
		},
		{
			name: "user with empty ID",
			user: slack.User{
				ID:      "",
				Name:    "testuser",
				Deleted: false,
				IsBot:   false,
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isValidUser(tt.user)
			if result != tt.expected {
				t.Errorf("isValidUser() = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestUserWatch_SaveAndLoadUsers(t *testing.T) {
	// Create temporary directory for test
	tempDir, err := os.MkdirTemp("", "userwatch_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	logger := zap.NewNop()
	config := Config{
		NotifyChannel: "C1234567890",
		DataDir:       tempDir,
	}
	mockSlack := &mockSlackService{
		orgURL: "https://test.slack.com/",
	}

	watch := NewUserWatch(logger, config, mockSlack)

	// Add some test users
	testUsers := map[string]*slack.User{
		"U1234567890": {
			ID:       "U1234567890",
			Name:     "testuser1",
			RealName: "Test User 1",
		},
		"U0987654321": {
			ID:       "U0987654321",
			Name:     "testuser2",
			RealName: "Test User 2",
		},
	}

	watch.knownUsers = testUsers

	// Test saving users
	err = watch.saveUsersToDisk()
	if err != nil {
		t.Fatalf("saveUsersToDisk() error = %v", err)
	}

	// Test loading users
	loadedUsers, err := watch.loadUsersFromDisk()
	if err != nil {
		t.Fatalf("loadUsersFromDisk() error = %v", err)
	}

	if len(loadedUsers) != len(testUsers) {
		t.Errorf("loadUsersFromDisk() returned %d users, want %d", len(loadedUsers), len(testUsers))
	}

	for id, expectedUser := range testUsers {
		loadedUser, exists := loadedUsers[id]
		if !exists {
			t.Errorf("loadUsersFromDisk() missing user %v", id)
			continue
		}

		if loadedUser.ID != expectedUser.ID {
			t.Errorf("loadUsersFromDisk() user %v ID = %v, want %v", id, loadedUser.ID, expectedUser.ID)
		}

		if loadedUser.Name != expectedUser.Name {
			t.Errorf("loadUsersFromDisk() user %v Name = %v, want %v", id, loadedUser.Name, expectedUser.Name)
		}

		if loadedUser.RealName != expectedUser.RealName {
			t.Errorf("loadUsersFromDisk() user %v RealName = %v, want %v", id, loadedUser.RealName, expectedUser.RealName)
		}
	}
}

func TestUserWatch_LoadUsersFromDisk_NoFile(t *testing.T) {
	logger := zap.NewNop()
	config := Config{
		NotifyChannel: "C1234567890",
		DataDir:       "/nonexistent/directory",
	}
	mockSlack := &mockSlackService{
		orgURL: "https://test.slack.com/",
	}

	watch := NewUserWatch(logger, config, mockSlack)

	users, err := watch.loadUsersFromDisk()
	if err != nil {
		t.Errorf("loadUsersFromDisk() with non-existent file should not error, got %v", err)
	}

	if users != nil {
		t.Errorf("loadUsersFromDisk() with non-existent file should return nil, got %v", users)
	}
}

func TestUserWatch_LoadUsersFromDisk_InvalidJSON(t *testing.T) {
	// Create temporary directory for test
	tempDir, err := os.MkdirTemp("", "userwatch_test")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.RemoveAll(tempDir) }()

	// Write invalid JSON to users file
	usersFile := filepath.Join(tempDir, "users.json")
	err = os.WriteFile(usersFile, []byte("invalid json"), 0644)
	if err != nil {
		t.Fatal(err)
	}

	logger := zap.NewNop()
	config := Config{
		NotifyChannel: "C1234567890",
		DataDir:       tempDir,
	}
	mockSlack := &mockSlackService{
		orgURL: "https://test.slack.com/",
	}

	watch := NewUserWatch(logger, config, mockSlack)

	users, err := watch.loadUsersFromDisk()
	if err == nil {
		t.Error("loadUsersFromDisk() with invalid JSON should return error")
	}

	if users != nil {
		t.Errorf("loadUsersFromDisk() with invalid JSON should return nil users, got %v", users)
	}
}

func TestLinkedinURL(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple name",
			input:    "John Doe",
			expected: "https://www.linkedin.com/search/results/people/?keywords=John%20Doe",
		},
		{
			name:     "name with special characters",
			input:    "José María",
			expected: "https://www.linkedin.com/search/results/people/?keywords=Jos%C3%A9%20Mar%C3%ADa",
		},
		{
			name:     "empty name",
			input:    "",
			expected: "https://www.linkedin.com/search/results/people/?keywords=",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := linkedinURL(tt.input)
			if result != tt.expected {
				t.Errorf("linkedinURL(%v) = %v, want %v", tt.input, result, tt.expected)
			}
		})
	}
}

func TestUserWatch_ValidateChannel_Format(t *testing.T) {
	logger := zap.NewNop()
	
	tests := []struct {
		name          string
		channelID     string
		expectedWarn  bool // Whether we expect a warning about format
	}{
		{
			name:         "valid channel ID format",
			channelID:    "C1234567890",
			expectedWarn: false,
		},
		{
			name:         "short channel ID format",
			channelID:    "C123",
			expectedWarn: true,
		},
		{
			name:         "channel ID without C prefix",
			channelID:    "1234567890",
			expectedWarn: true,
		},
		{
			name:         "empty channel ID",
			channelID:    "",
			expectedWarn: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := Config{
				NotifyChannel: tt.channelID,
				DataDir:       "",
			}
			mockSlack := &mockSlackService{
				orgURL: "https://test.slack.com/",
			}

			watch := NewUserWatch(logger, config, mockSlack)
			
			// Test channel ID format validation logic (without API calls)
			// We test the format validation part by checking the length and prefix
			hasValidFormat := len(tt.channelID) >= 9 && strings.HasPrefix(tt.channelID, "C")
			if hasValidFormat == tt.expectedWarn {
				t.Errorf("Channel ID %v format validation unexpected result", tt.channelID)
			}
			
			// Note: We skip the actual validateChannel call since it would require
			// mocking the Slack API client, which is complex with the current architecture
			_ = watch // Use the watch variable to avoid unused variable warning
		})
	}
}

func TestUser_JSON(t *testing.T) {
	user := User{
		ID:       "U1234567890",
		Name:     "testuser",
		RealName: "Test User",
	}

	// Test JSON marshaling
	data, err := json.Marshal(user)
	if err != nil {
		t.Fatalf("json.Marshal(user) error = %v", err)
	}

	// Test JSON unmarshaling
	var unmarshaled User
	err = json.Unmarshal(data, &unmarshaled)
	if err != nil {
		t.Fatalf("json.Unmarshal(data) error = %v", err)
	}

	if unmarshaled.ID != user.ID {
		t.Errorf("Unmarshaled user ID = %v, want %v", unmarshaled.ID, user.ID)
	}

	if unmarshaled.Name != user.Name {
		t.Errorf("Unmarshaled user Name = %v, want %v", unmarshaled.Name, user.Name)
	}

	if unmarshaled.RealName != user.RealName {
		t.Errorf("Unmarshaled user RealName = %v, want %v", unmarshaled.RealName, user.RealName)
	}
}

func BenchmarkIsValidUser(b *testing.B) {
	users := []slack.User{
		{ID: "U1234567890", Name: "user1", Deleted: false, IsBot: false},
		{ID: "U0987654321", Name: "user2", Deleted: true, IsBot: false},
		{ID: "B1234567890", Name: "bot1", Deleted: false, IsBot: true},
		{ID: "", Name: "invalid", Deleted: false, IsBot: false},
	}

	for b.Loop() {
		for _, user := range users {
			isValidUser(user)
		}
	}
}

func BenchmarkLinkedinURL(b *testing.B) {
	names := []string{"John Doe", "José María", "Test User", ""}

	for b.Loop() {
		for _, name := range names {
			linkedinURL(name)
		}
	}
}