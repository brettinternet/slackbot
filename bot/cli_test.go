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
	// Clear any environment variables that might provide credentials
	t.Setenv("SLACK_TOKEN", "")
	t.Setenv("SLACK_SIGNING_SECRET", "")
	
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
		"--config-file=/dev/null", // Use non-existent config to avoid path issues
		// No slack credentials - should fail early but still set start flag
	}

	err := cmd.Run(ctx, args)

	// Should fail with no credentials error  
	if err == nil {
		t.Error("Start command should fail when no credentials provided")
	}

	// Start flag should NOT be set when CLI validation fails
	if *start {
		t.Error("Start command should not set start flag when validation fails")
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
		"--config-file=/dev/null", // Use non-existent config to avoid path issues
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
		"--config-file", "../config.yaml",
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
	// Clear any environment variables that might provide credentials
	t.Setenv("SLACK_TOKEN", "")
	t.Setenv("SLACK_SIGNING_SECRET", "")
	
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
		"--config-file=/dev/null", // Use non-existent config to avoid path issues
		// No Slack credentials - should fail early but set start flag
	}

	err := cmd.Run(ctx, args)

	// Should fail with no credentials error
	if err == nil {
		t.Error("Start command should fail when no credentials provided")
	}

	// Start flag should NOT be set when CLI validation fails
	if *start {
		t.Error("Start command should not set start flag when validation fails")
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
			// Clear any environment variables that might provide credentials
			t.Setenv("SLACK_TOKEN", "")
			t.Setenv("SLACK_SIGNING_SECRET", "")
			
			bot := NewBot(buildOpts)
			start, cmd := NewCommandRoot(bot)

			ctx := context.Background()
			args := []string{
				"bot",
				"--log-level", level,
				"--config-file=/dev/null", // Use non-existent config to avoid path issues
				// No Slack credentials - should fail early but set start flag
			}

			err := cmd.Run(ctx, args)

			// Should fail with no credentials error
			if err == nil {
				t.Error("Start command should fail when no credentials provided")
			}

			// Start flag should NOT be set when CLI validation fails
			if *start {
				t.Errorf("Start command should not set start flag when validation fails for log level %v", level)
			}
		})
	}
}

func TestCommandRoot_CustomPorts(t *testing.T) {
	// Clear any environment variables that might provide credentials
	t.Setenv("SLACK_TOKEN", "")
	t.Setenv("SLACK_SIGNING_SECRET", "")
	
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
		"--config-file=/dev/null", // Use non-existent config to avoid path issues
		// No Slack credentials - should fail early but set start flag
	}

	err := cmd.Run(ctx, args)

	// Should fail with no credentials error
	if err == nil {
		t.Error("Start command should fail when no credentials provided")
	}

	// Start flag should NOT be set when CLI validation fails
	if *start {
		t.Error("Start command should not set start flag when validation fails")
	}
}

func TestCommandRoot_SlackConfiguration(t *testing.T) {
	// Clear any environment variables that might provide credentials
	t.Setenv("SLACK_TOKEN", "")
	t.Setenv("SLACK_SIGNING_SECRET", "")
	
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
		"--config-file=/dev/null", // Use non-existent config to avoid path issues
		// No Slack credentials - should fail early but set start flag
	}

	err := cmd.Run(ctx, args)

	// Should fail with no credentials error
	if err == nil {
		t.Error("Start command should fail when no credentials provided")
	}

	// Start flag should NOT be set when CLI validation fails
	if *start {
		t.Error("Start command should not set start flag when validation fails")
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
