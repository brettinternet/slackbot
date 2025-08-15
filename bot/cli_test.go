package bot

import (
	"context"
	"testing"

	"slackbot.arpa/bot/config"
)

func TestNewCommandRoot(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	start, cmd := NewCommandRoot(bot)

	if start == nil {
		t.Fatal("NewCommandRoot() start pointer is nil")
	}

	if cmd == nil {
		t.Fatal("NewCommandRoot() command is nil")
	}

	if cmd.Name != "slackbot" {
		t.Errorf("NewCommandRoot() command name = %v, want 'slackbot'", cmd.Name)
	}

	// Test that flags are properly set up
	if len(cmd.Flags) == 0 {
		t.Error("NewCommandRoot() should have flags configured")
	}

	// Initially start should be false
	if *start {
		t.Error("NewCommandRoot() start should initially be false")
	}
}

func TestCommandRoot_HelpCommand(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	_, cmd := NewCommandRoot(bot)

	// Test help command
	ctx := context.Background()
	err := cmd.Run(ctx, []string{"bot", "--help"})
	
	// Help should not error and should not trigger start
	if err != nil {
		t.Errorf("Help command error = %v, want nil", err)
	}
}

func TestCommandRoot_VersionCommand(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	_, cmd := NewCommandRoot(bot)

	// Test version command
	ctx := context.Background()
	err := cmd.Run(ctx, []string{"bot", "--version"})
	
	// Version should not error and should not trigger start
	if err != nil {
		t.Errorf("Version command error = %v, want nil", err)
	}
}

func TestCommandRoot_StartCommand(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	start, cmd := NewCommandRoot(bot)

	// Test start command with minimal required flags
	ctx := context.Background()
	args := []string{
		"bot",
		// No slack credentials - should fail early but still set start flag
	}

	err := cmd.Run(ctx, args)
	
	// Should fail with no credentials error but still set start flag 
	if err != nil && err.Error() != "no Slack authentication credentials provided" {
		t.Errorf("Start command should fail with no credentials error, got = %v", err)
	}

	// Start flag should be set
	if !*start {
		t.Error("Start command should set start flag to true")
	}
}

func TestCommandRoot_StartCommand_MissingFlags(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	start, cmd := NewCommandRoot(bot)

	// Test start command without required flags
	ctx := context.Background()
	args := []string{
		"bot",
		"start",
		// Missing required --slack-token and --slack-signing-secret
	}

	err := cmd.Run(ctx, args)
	
	// Should not error at CLI level - validation happens in Setup()
	if err != nil && err.Error() != "no Slack authentication credentials provided" {
		t.Errorf("Start command without flags should parse, got error = %v", err)
	}

	// Start flag should still be set even if validation fails later
	if !*start {
		t.Error("Start command should set start flag to true even with missing flags")
	}
}

func TestCommandRoot_WithConfigFile(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	start, cmd := NewCommandRoot(bot)

	// Test with config file flag
	ctx := context.Background()
	args := []string{
		"bot",
		"--config-file", "../cmd/bot/config.yaml",
		// No Slack credentials - should fail early but set start flag
	}

	err := cmd.Run(ctx, args)
	
	// Should fail with no credentials error but still set start flag
	if err != nil && err.Error() != "no Slack authentication credentials provided" {
		t.Errorf("Start command should fail with no credentials error, got = %v", err)
	}

	if !*start {
		t.Error("Start command should set start flag to true")
	}
}

func TestCommandRoot_WithFeatures(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	start, cmd := NewCommandRoot(bot)

	// Test with features
	ctx := context.Background()
	args := []string{
		"bot",
		"--openai-api-key", "test-openai-key",
		// No Slack credentials - should fail early but set start flag
	}

	err := cmd.Run(ctx, args)
	
	// Should fail with no credentials error but still set start flag
	if err != nil && err.Error() != "no Slack authentication credentials provided" {
		t.Errorf("Start command should fail with no credentials error, got = %v", err)
	}

	if !*start {
		t.Error("Start command should set start flag to true")
	}
}

func TestCommandRoot_EnvironmentOverride(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development", // Build env is development
	}

	bot := NewBot(buildOpts)
	start, cmd := NewCommandRoot(bot)

	// Test overriding environment via flag
	ctx := context.Background()
	args := []string{
		"bot",
		"--env", "production", // Override to production
		// No Slack credentials - should fail early but set start flag
	}

	err := cmd.Run(ctx, args)
	
	// Should fail with no credentials error but still set start flag
	if err != nil && err.Error() != "no Slack authentication credentials provided" {
		t.Errorf("Start command should fail with no credentials error, got = %v", err)
	}

	if !*start {
		t.Error("Start command should set start flag to true")
	}
}

func TestCommandRoot_LogLevels(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	logLevels := []string{"debug", "info", "warn", "error"}

	for _, level := range logLevels {
		t.Run("log-level-"+level, func(t *testing.T) {
			bot := NewBot(buildOpts)
			start, cmd := NewCommandRoot(bot)

			ctx := context.Background()
			args := []string{
				"bot",
				"--log-level", level,
				// No Slack credentials - should fail early but set start flag
			}

			err := cmd.Run(ctx, args)
			
			// Should fail with no credentials error but still set start flag
			if err != nil && err.Error() != "no Slack authentication credentials provided" {
				t.Errorf("Start command should fail with no credentials error, got = %v", err)
			}

			if !*start {
				t.Errorf("Start command should set start flag to true for log level %v", level)
			}
		})
	}
}

func TestCommandRoot_CustomPorts(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	start, cmd := NewCommandRoot(bot)

	// Test custom server port
	ctx := context.Background()
	args := []string{
		"bot",
		"--server-port", "9000",
		// No Slack credentials - should fail early but set start flag
	}

	err := cmd.Run(ctx, args)
	
	// Should fail with no credentials error but still set start flag
	if err != nil && err.Error() != "no Slack authentication credentials provided" {
		t.Errorf("Start command should fail with no credentials error, got = %v", err)
	}

	if !*start {
		t.Error("Start command should set start flag to true")
	}
}

func TestCommandRoot_SlackConfiguration(t *testing.T) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	bot := NewBot(buildOpts)
	start, cmd := NewCommandRoot(bot)

	// Test with full Slack configuration
	ctx := context.Background()
	args := []string{
		"bot",
		"--slack-preferred-users", "user1,user2,user3",
		"--slack-preferred-channels", "general,random",
		"--slack-user-notify-channel", "obituaries",
		"--slack-events-path", "/custom/events",
		// No Slack credentials - should fail early but set start flag
	}

	err := cmd.Run(ctx, args)
	
	// Should fail with no credentials error but still set start flag
	if err != nil && err.Error() != "no Slack authentication credentials provided" {
		t.Errorf("Start command should fail with no credentials error, got = %v", err)
	}

	if !*start {
		t.Error("Start command should set start flag to true")
	}
}

func BenchmarkCommandRoot_Parse(b *testing.B) {
	buildOpts := config.BuildOpts{
		BuildVersion:     "test-version",
		BuildTime:        "test-time",
		BuildEnvironment: "development",
	}

	args := []string{
		"bot",
		"start",
		"--slack-token", "test-token",
		"--slack-signing-secret", "test-secret",
		"--features", "chat,vibecheck",
		"--log-level", "info",
		"--server-port", "8080",
	}

	ctx := context.Background()

	for b.Loop() {
		bot := NewBot(buildOpts)
		_, cmd := NewCommandRoot(bot)
		_ = cmd.Run(ctx, args)
	}
}