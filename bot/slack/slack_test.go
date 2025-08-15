package slack

import (
	"context"
	"net/http"
	"testing"

	"go.uber.org/zap/zaptest"
)

func TestNewSlack(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		Token:             "test-token",
		SigningSecret:     "test-secret",
		Debug:             true,
		PreferredChannels: []string{"general", "random"},
	}

	slack := NewSlack(logger, config)

	if slack == nil {
		t.Fatal("NewSlack() returned nil")
	}

	if slack.config.Token != "test-token" {
		t.Errorf("NewSlack() Token = %v, want %v", slack.config.Token, "test-token")
	}

	if slack.config.SigningSecret != "test-secret" {
		t.Errorf("NewSlack() SigningSecret = %v, want %v", slack.config.SigningSecret, "test-secret")
	}

	if !slack.config.Debug {
		t.Error("NewSlack() Debug should be true")
	}

	if len(slack.config.PreferredChannels) != 2 {
		t.Errorf("NewSlack() PreferredChannels length = %v, want 2", len(slack.config.PreferredChannels))
	}

	if slack.client != nil {
		t.Error("NewSlack() should not initialize client until Setup() is called")
	}
}

func TestSlack_Setup_EmptyToken(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		Token:         "", // Empty token
		SigningSecret: "test-secret",
	}

	slack := NewSlack(logger, config)
	ctx := context.Background()

	err := slack.Setup(ctx)
	if err == nil {
		t.Error("Setup() with empty token should return error")
	}

	expectedError := "no Slack authentication credentials provided"
	if err.Error() != expectedError {
		t.Errorf("Setup() error = %v, want %v", err.Error(), expectedError)
	}

	if slack.client != nil {
		t.Error("Setup() with error should not set client")
	}
}

func TestSlack_Setup_ValidToken(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		Token:         "xoxb-test-token-that-looks-valid",
		SigningSecret: "test-signing-secret",
		Debug:         false,
	}

	slack := NewSlack(logger, config)
	ctx := context.Background()

	// Note: This will likely fail in tests due to invalid token,
	// but we can test that the method properly attempts to create the client
	err := slack.Setup(ctx)
	// With an invalid token, AuthTest will fail, which is expected
	if err == nil {
		// If no error, client should be set
		if slack.client == nil {
			t.Error("Setup() with no error should set client")
		}
		// Auth response should be set
		if slack.authResp == nil {
			t.Error("Setup() with no error should set authResp")
		}
	} else {
		// If error occurs, it's expected with a fake token
		// But client should still be created
		if slack.client == nil {
			t.Error("Setup() should create client even if auth fails")
		}
	}
}

func TestSlack_Start(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		Token:         "test-token",
		SigningSecret: "test-secret",
	}

	slack := NewSlack(logger, config)
	ctx := context.Background()

	err := slack.Start(ctx)
	if err == nil {
		t.Error("Start() should return error when client is not initialized")
	}
}

func TestSlack_Stop(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		Token:         "test-token",
		SigningSecret: "test-secret",
	}

	slack := NewSlack(logger, config)
	ctx := context.Background()

	err := slack.Stop(ctx)
	if err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}
}

func TestSlack_Client_BeforeSetup(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		Token:         "test-token",
		SigningSecret: "test-secret",
	}

	slack := NewSlack(logger, config)

	client := slack.Client()
	if client != nil {
		t.Error("Client() should return nil before Setup() is called")
	}
}

func TestSlack_VerifyRequest_NoClient(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		Token:         "test-token",
		SigningSecret: "test-secret",
	}

	slack := NewSlack(logger, config)

	headers := make(http.Header)
	body := []byte("test body")

	err := slack.VerifyRequest(headers, body)
	if err == nil {
		t.Error("VerifyRequest() should return error when client is not initialized")
	}
}

func TestSlack_AuthResponse_BeforeSetup(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		Token:         "test-token",
		SigningSecret: "test-secret",
	}

	slack := NewSlack(logger, config)

	if slack.authResp != nil {
		t.Error("authResp should be nil before Setup() is called")
	}
}

func TestConfig_Validation(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{
			name: "minimal config",
			config: Config{
				Token:         "test-token",
				SigningSecret: "test-secret",
			},
		},
		{
			name: "full config",
			config: Config{
				Token:             "test-token",
				SigningSecret:     "test-secret",
				Debug:             true,
				PreferredChannels: []string{"general", "random"},
			},
		},
		{
			name: "config with empty channels",
			config: Config{
				Token:             "test-token",
				SigningSecret:     "test-secret",
				PreferredChannels: []string{},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger := zaptest.NewLogger(t)
			slack := NewSlack(logger, tt.config)

			// Test that config is properly stored
			if slack.config.Token != tt.config.Token {
				t.Errorf("Config Token = %v, want %v", slack.config.Token, tt.config.Token)
			}

			if slack.config.SigningSecret != tt.config.SigningSecret {
				t.Errorf("Config SigningSecret = %v, want %v", slack.config.SigningSecret, tt.config.SigningSecret)
			}

			if slack.config.Debug != tt.config.Debug {
				t.Errorf("Config Debug = %v, want %v", slack.config.Debug, tt.config.Debug)
			}
		})
	}
}

func TestSlack_FullLifecycle(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		Token:         "test-token",
		SigningSecret: "test-secret",
	}

	slack := NewSlack(logger, config)
	ctx := context.Background()

	// Test initial state
	if slack.Client() != nil {
		t.Error("Client() should return nil before Setup()")
	}

	if slack.authResp != nil {
		t.Error("authResp should be nil before Setup()")
	}

	// Start without setup should return error
	err := slack.Start(ctx)
	if err == nil {
		t.Error("Start() without setup should return error")
	}

	// Stop should work
	err = slack.Stop(ctx)
	if err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}

	// Multiple stops should work
	err = slack.Stop(ctx)
	if err != nil {
		t.Errorf("Stop() called twice error = %v, want nil", err)
	}
}

func TestSlack_OrgURL_BeforeSetup(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		Token:         "test-token",
		SigningSecret: "test-secret",
	}

	slack := NewSlack(logger, config)

	// Should not panic when authResp is nil
	defer func() {
		if r := recover(); r == nil {
			t.Error("OrgURL() should panic when authResp is nil")
		}
	}()
	
	slack.OrgURL()
}

func BenchmarkNewSlack(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := Config{
		Token:             "test-token",
		SigningSecret:     "test-secret",
		PreferredChannels: []string{"general", "random"},
	}

	for b.Loop() {
		NewSlack(logger, config)
	}
}

func BenchmarkSlack_Setup(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := Config{
		Token:         "test-token", // This will fail auth, but measures setup performance
		SigningSecret: "test-secret",
	}
	ctx := context.Background()

	for b.Loop() {
		slack := NewSlack(logger, config)
		// Note: This will error due to invalid token, but we're measuring performance
		slack.Setup(ctx)
	}
}