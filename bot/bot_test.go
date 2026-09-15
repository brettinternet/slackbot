package bot

import (
	"context"
	"errors"
	"sync/atomic"
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
	if config := bot.configManager.GetConfig(); config == nil || config.Version != "test-version" {
		if config == nil {
			t.Error("Setup() should initialize config manager")
		} else {
			t.Errorf("Setup() config.Version = %v, want %v", config.Version, "test-version")
		}
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

func TestBot_Shutdown_EmptyBot(t *testing.T) {
	bot := NewBot(config.BuildOpts{})

	if err := bot.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() on uninitialized bot error = %v, want nil", err)
	}
	if err := bot.Shutdown(context.Background()); err != nil {
		t.Fatalf("repeated Shutdown() error = %v, want nil", err)
	}
}

func TestBot_Shutdown_AttemptsEveryStepAndJoinsErrors(t *testing.T) {
	firstErr := errors.New("first failure")
	secondErr := errors.New("second failure")
	var calls []string
	bot := NewBot(config.BuildOpts{})
	addTestShutdownStep(t, bot, "first", func(context.Context) error {
		calls = append(calls, "first")
		return firstErr
	})
	addTestShutdownStep(t, bot, "middle", func(context.Context) error {
		calls = append(calls, "middle")
		return nil
	})
	addTestShutdownStep(t, bot, "second", func(context.Context) error {
		calls = append(calls, "second")
		return secondErr
	})

	err := bot.Shutdown(context.Background())
	if !errors.Is(err, firstErr) || !errors.Is(err, secondErr) {
		t.Fatalf("Shutdown() error = %v, want both shutdown failures", err)
	}
	want := []string{"second", "middle", "first"}
	if len(calls) != len(want) {
		t.Fatalf("Shutdown() calls = %v, want %v", calls, want)
	}
	for i := range want {
		if calls[i] != want[i] {
			t.Fatalf("Shutdown() calls = %v, want %v", calls, want)
		}
	}
}

func TestBot_Shutdown_RepeatedCallsRunCleanupOnce(t *testing.T) {
	var calls atomic.Int32
	stepErr := errors.New("shutdown failure")
	bot := NewBot(config.BuildOpts{})
	addTestShutdownStep(t, bot, "service", func(context.Context) error {
		calls.Add(1)
		return stepErr
	})

	firstErr := bot.Shutdown(context.Background())
	secondErr := bot.Shutdown(context.Background())
	if !errors.Is(firstErr, stepErr) || !errors.Is(secondErr, stepErr) {
		t.Fatalf("Shutdown() errors = (%v, %v), want cached service failure", firstErr, secondErr)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("shutdown calls = %d, want 1", got)
	}
}

func TestBot_Shutdown_ExpiredDeadlineStillAttemptsEveryStep(t *testing.T) {
	var calls atomic.Int32
	bot := NewBot(config.BuildOpts{})
	for range 2 {
		addTestShutdownStep(t, bot, "service", func(ctx context.Context) error {
			calls.Add(1)
			return ctx.Err()
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := bot.Shutdown(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Shutdown() error = %v, want context canceled", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("shutdown calls = %d, want 2", got)
	}
}

func TestBot_Shutdown_CompletedResultWinsOverCanceledCaller(t *testing.T) {
	bot := NewBot(config.BuildOpts{})
	if err := bot.Shutdown(context.Background()); err != nil {
		t.Fatalf("first Shutdown() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := bot.Shutdown(ctx); err != nil {
		t.Fatalf("repeated Shutdown() error = %v, want cached nil result", err)
	}
}

func TestBot_AddShutdownStepAfterShutdownStopsResourceImmediately(t *testing.T) {
	bot := NewBot(config.BuildOpts{})
	if err := bot.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	var stops atomic.Int32

	err := bot.addShutdownStep("late service", func(context.Context) error {
		stops.Add(1)
		return nil
	})
	if err == nil {
		t.Fatal("addShutdownStep() error = nil, want shutdown-started error")
	}
	if got := stops.Load(); got != 1 {
		t.Fatalf("stop calls = %d, want 1", got)
	}
}

func TestBot_StartServiceStopsServiceWhenShutdownRacesStart(t *testing.T) {
	bot := NewBot(config.BuildOpts{})
	startEntered := make(chan struct{})
	allowStart := make(chan struct{})
	var stops atomic.Int32
	startResult := make(chan error, 1)
	go func() {
		startResult <- bot.startService(
			context.Background(),
			"test service",
			func(context.Context) error {
				close(startEntered)
				<-allowStart
				return nil
			},
			func(context.Context) error {
				stops.Add(1)
				return nil
			},
		)
	}()

	<-startEntered
	if err := bot.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown() error = %v", err)
	}
	close(allowStart)
	if err := <-startResult; err == nil {
		t.Fatal("startService() error = nil, want concurrent shutdown error")
	}
	if got := stops.Load(); got != 1 {
		t.Fatalf("stop calls = %d, want 1", got)
	}
}

func addTestShutdownStep(
	t *testing.T,
	bot *Bot,
	name string,
	stop func(context.Context) error,
) {
	t.Helper()
	if err := bot.addShutdownStep(name, stop); err != nil {
		t.Fatalf("addShutdownStep() error = %v", err)
	}
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
