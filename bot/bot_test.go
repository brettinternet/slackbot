package bot

import (
	"context"
	"testing"

	"github.com/urfave/cli/v3"
	"slackbot.arpa/bot/config"
)

func TestNewBot(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)

	if bot == nil {
		t.Fatal("NewBot() returned nil")
	}

	if bot.BuildOpts != buildOpts {
		t.Errorf("NewBot() BuildOpts = %v, want %v", bot.BuildOpts, buildOpts)
	}
}

func TestBot_Setup_MinimalConfig(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	ctx := context.Background()

	// Create a minimal CLI command with required flags
	cmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "env", Value: "development"},
			&cli.StringFlag{Name: "log-level", Value: "info"},
			&cli.StringFlag{Name: "data-dir", Value: "./tmp"},
			&cli.UintFlag{Name: "server-port", Value: 8080},
			&cli.StringFlag{Name: "slack-token", Value: "test-token"},
			&cli.StringFlag{Name: "slack-signing-secret", Value: "test-secret"},
			&cli.StringFlag{Name: "openai-api-key", Value: ""},
			&cli.StringSliceFlag{Name: "slack-preferred-users"},
			&cli.StringSliceFlag{Name: "slack-preferred-channels"},
			&cli.StringFlag{Name: "slack-obituary-notify-channel", Value: ""},
			&cli.StringFlag{Name: "slack-events-path", Value: "/slack/events"},
			&cli.StringFlag{Name: "config-file", Value: ""},
		},
	}

	// Parse minimal args to set up the command context
	err := cmd.Run(context.Background(), []string{"test"})
	if err != nil {
		// We expect this to fail since we're not actually running the command
		// We just need the flags to be parsed
		t.Logf("Expected command run error: %v", err)
	}

	_, err = bot.Setup(ctx, cmd)
	// Setup will fail due to invalid Slack credentials, but we can test basic initialization
	if err == nil {
		t.Error("Setup() should fail with invalid Slack credentials")
	}

	// Even with error, basic config should be set up
	if bot.config.Version != "test-version" {
		t.Errorf("Setup() config.Version = %v, want %v", bot.config.Version, "test-version")
	}

	if bot.log == nil {
		t.Error("Setup() should initialize logger even if Slack setup fails")
	}
}

func TestBot_Logger(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	ctx := context.Background()

	cmd := createMinimalCommand()
	_, err := bot.Setup(ctx, cmd)
	// Setup will fail due to Slack, but logger should still be initialized
	if err == nil {
		t.Error("Setup() should fail with invalid Slack credentials")
	}

	logger := bot.Logger()
	if logger == nil {
		t.Error("Logger() returned nil")
	}

	// Test that it's the same logger instance
	if logger != bot.log {
		t.Error("Logger() should return the same logger instance as bot.log")
	}
}

func TestBot_BeginShutdown_NoHTTP(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	ctx := context.Background()

	// Test BeginShutdown when http is nil
	err := bot.BeginShutdown(ctx)
	if err != nil {
		t.Errorf("BeginShutdown() with nil http should not error, got %v", err)
	}
}

func TestBot_ForceShutdown(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	ctx := context.Background()

	err := bot.ForceShutdown(ctx)
	if err != nil {
		t.Errorf("ForceShutdown() error = %v, want nil", err)
	}
}

func TestBot_Shutdown_EmptyBot(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	ctx := context.Background()

	// Test shutdown on empty bot - this will panic because slack is nil
	// This test demonstrates that Setup() must be called before Shutdown()
	defer func() {
		if r := recover(); r == nil {
			t.Error("Shutdown() on uninitialized bot should panic")
		}
	}()

	_ = bot.Shutdown(ctx)
}

// Helper function to create a minimal command for testing
func createMinimalCommand() *cli.Command {
	app := &cli.Command{
		Name: "test-app",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "env", Value: "development"},
			&cli.StringFlag{Name: "log-level", Value: "info"},
			&cli.StringFlag{Name: "data-dir", Value: "./tmp"},
			&cli.UintFlag{Name: "server-port", Value: 8080},
			&cli.StringFlag{Name: "slack-token", Value: "test-token"},
			&cli.StringFlag{Name: "slack-signing-secret", Value: "test-secret"},
			&cli.StringFlag{Name: "openai-api-key", Value: ""},
			&cli.StringSliceFlag{Name: "slack-preferred-users"},
			&cli.StringSliceFlag{Name: "slack-preferred-channels"},
			&cli.StringFlag{Name: "slack-obituary-notify-channel", Value: ""},
			&cli.StringFlag{Name: "slack-events-path", Value: "/slack/events"},
			&cli.StringFlag{Name: "config-file", Value: ""},
		},
		Action: func(ctx context.Context, cmd *cli.Command) error {
			// No-op action for testing
			return nil
		},
	}

	// Initialize the command with test args
	_ = app.Run(context.Background(), []string{"test-app"})
	return app
}

func BenchmarkNewBot(b *testing.B) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	b.ResetTimer()
	for b.Loop() {
		NewBot(buildOpts)
	}
}

func BenchmarkBot_Setup(b *testing.B) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	cmd := createMinimalCommand()
	ctx := context.Background()

	b.ResetTimer()
	for b.Loop() {
		bot := NewBot(buildOpts)
		_, _ = bot.Setup(ctx, cmd)
	}
}
