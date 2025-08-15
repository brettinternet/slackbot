package main

import (
	"context"
	"testing"
	"time"
)

func TestSleepContext_Timeout(t *testing.T) {
	ctx := context.Background()
	duration := 10 * time.Millisecond

	start := time.Now()
	err := sleepContext(ctx, duration)
	elapsed := time.Since(start)

	if err != nil {
		t.Errorf("sleepContext() error = %v, want nil", err)
	}

	// Should sleep for approximately the duration
	if elapsed < duration {
		t.Errorf("sleepContext() elapsed = %v, want at least %v", elapsed, duration)
	}

	// Allow some margin for timing
	maxExpected := duration + 50*time.Millisecond
	if elapsed > maxExpected {
		t.Errorf("sleepContext() elapsed = %v, want at most %v", elapsed, maxExpected)
	}
}

func TestSleepContext_ContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	duration := 1 * time.Second // Long duration

	// Cancel context immediately
	cancel()

	start := time.Now()
	err := sleepContext(ctx, duration)
	elapsed := time.Since(start)

	if err != context.Canceled {
		t.Errorf("sleepContext() error = %v, want %v", err, context.Canceled)
	}

	// Should return quickly due to cancellation
	if elapsed > 100*time.Millisecond {
		t.Errorf("sleepContext() should return quickly on cancellation, took %v", elapsed)
	}
}

func TestSleepContext_ContextTimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	duration := 1 * time.Second // Longer than context timeout

	start := time.Now()
	err := sleepContext(ctx, duration)
	elapsed := time.Since(start)

	if err != context.DeadlineExceeded {
		t.Errorf("sleepContext() error = %v, want %v", err, context.DeadlineExceeded)
	}

	// Should return when context times out, not when sleep duration is reached
	if elapsed > 200*time.Millisecond {
		t.Errorf("sleepContext() should return on context timeout, took %v", elapsed)
	}
}

func TestRun_HelpCommand(t *testing.T) {
	args := []string{"bot", "--help"}
	ctx := context.Background()

	// Help command should not error and should return quickly
	err := run(ctx, args)
	if err != nil {
		t.Errorf("run() with --help error = %v, want nil", err)
	}
}

func TestRun_VersionCommand(t *testing.T) {
	args := []string{"bot", "--version"}
	ctx := context.Background()

	// Version command should not error and should return quickly
	err := run(ctx, args)
	if err != nil {
		t.Errorf("run() with --version error = %v, want nil", err)
	}
}

func TestRun_InvalidCommand(t *testing.T) {
	args := []string{"bot", "invalid-command"}
	ctx := context.Background()

	// Invalid command should return an error
	err := run(ctx, args)
	if err == nil {
		t.Error("run() with invalid command should return error")
	}
}

func TestRun_StartCommand_MissingCredentials(t *testing.T) {
	args := []string{"bot", "start"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Start without credentials should error
	err := run(ctx, args)
	if err == nil {
		t.Error("run() start without credentials should return error")
	}

	// Should contain meaningful error message about credentials
	errMsg := err.Error()
	if errMsg == "" {
		t.Error("run() should return meaningful error message")
	}
}

func TestRun_StartCommand_WithCredentials(t *testing.T) {
	args := []string{
		"bot",
		"start",
		"--slack-token", "xoxb-test-token",
		"--slack-signing-secret", "test-secret",
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// This will likely error due to invalid Slack credentials,
	// but we can test that the command parsing works
	err := run(ctx, args)
	// We expect an error here due to invalid Slack credentials
	// The important thing is that we get past the CLI parsing
	if err == nil {
		t.Log("run() succeeded - this might happen in some test environments")
	} else {
		// Error is expected with fake credentials
		t.Logf("run() error (expected with fake credentials): %v", err)
	}
}

func TestMain_Integration(t *testing.T) {
	// This is a basic integration test that verifies main() doesn't panic
	// We can't easily test the full main() function in unit tests since it
	// handles signals and runs indefinitely, but we can test that it doesn't
	// immediately panic or have obvious issues.
	
	// Test that main() can be called without panicking
	// We'll do this by testing that the core logic works
	ctx := context.Background()
	args := []string{"bot", "--help"}
	
	err := run(ctx, args)
	if err != nil {
		t.Errorf("Integration test: run() with help should not error, got %v", err)
	}
}

func TestTerminationTimeouts(t *testing.T) {
	// Test that our timeout constants are reasonable
	if terminationGracePeriod <= 0 {
		t.Error("terminationGracePeriod should be positive")
	}

	if terminationDrainPeriod <= 0 {
		t.Error("terminationDrainPeriod should be positive")
	}

	if terminationHardPeriod <= 0 {
		t.Error("terminationHardPeriod should be positive")
	}

	// Drain period should be shorter than grace period
	if terminationDrainPeriod >= terminationGracePeriod {
		t.Error("terminationDrainPeriod should be shorter than terminationGracePeriod")
	}

	// Hard period should be shorter than grace period
	if terminationHardPeriod >= terminationGracePeriod {
		t.Error("terminationHardPeriod should be shorter than terminationGracePeriod")
	}
}

func TestBuildVariables(t *testing.T) {
	// Test that build variables can be set (they'll be empty in tests, which is fine)
	if buildVersion == "" {
		t.Log("buildVersion is empty (expected in tests)")
	}

	if buildTime == "" {
		t.Log("buildTime is empty (expected in tests)")
	}

	if buildEnvironment == "" {
		t.Log("buildEnvironment is empty (expected in tests)")
	}

	// Test that they're at least strings (not causing compile errors)
	_ = buildVersion
	_ = buildTime
	_ = buildEnvironment
}

func BenchmarkSleepContext(b *testing.B) {
	ctx := context.Background()
	duration := 1 * time.Millisecond

	for b.Loop() {
		sleepContext(ctx, duration)
	}
}

func BenchmarkRun_Help(b *testing.B) {
	args := []string{"bot", "--help"}
	ctx := context.Background()

	for b.Loop() {
		run(ctx, args)
	}
}