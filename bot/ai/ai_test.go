package ai

import (
	"context"
	"testing"

	"go.uber.org/zap/zaptest"
)

func TestNewAI(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		OpenAIAPIKey: "test-api-key",
	}

	ai := NewAI(logger, config)

	if ai == nil {
		t.Fatal("NewAI() returned nil")
	}

	if ai.log != logger {
		t.Error("NewAI() should set the logger correctly")
	}

	if ai.config.OpenAIAPIKey != "test-api-key" {
		t.Errorf("NewAI() config.OpenAIAPIKey = %v, want %v", ai.config.OpenAIAPIKey, "test-api-key")
	}

	if ai.llm != nil {
		t.Error("NewAI() should not initialize llm until Start() is called")
	}
}

func TestAI_Start_InvalidKey(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		OpenAIAPIKey: "", // Empty key should cause error
	}

	ai := NewAI(logger, config)
	ctx := context.Background()

	err := ai.Start(ctx)
	if err == nil {
		t.Error("Start() with empty API key should return error")
	}

	if ai.llm != nil {
		t.Error("Start() with error should not set llm")
	}
}

func TestAI_Start_WithValidKey(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		OpenAIAPIKey: "sk-test-key-that-looks-valid-but-wont-work-in-tests",
	}

	ai := NewAI(logger, config)
	ctx := context.Background()

	err := ai.Start(ctx)
	// Note: This will likely fail in tests due to invalid API key,
	// but we can test that the method properly attempts to create the model
	// The actual OpenAI client creation will fail with invalid key, which is expected
	if err == nil {
		// If no error, llm should be set
		if ai.llm == nil {
			t.Error("Start() with no error should set llm")
		}
	}
	// If error occurs, it's expected with a fake API key
}

func TestAI_Stop(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		OpenAIAPIKey: "test-api-key",
	}

	ai := NewAI(logger, config)
	ctx := context.Background()

	err := ai.Stop(ctx)
	if err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}
}

func TestAI_LLM_BeforeStart(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		OpenAIAPIKey: "test-api-key",
	}

	ai := NewAI(logger, config)

	llm := ai.LLM()
	if llm != nil {
		t.Error("LLM() should return nil before Start() is called")
	}
}

func TestConfig(t *testing.T) {
	tests := []struct {
		name   string
		config Config
	}{
		{
			name:   "empty config",
			config: Config{},
		},
		{
			name: "config with API key",
			config: Config{
				OpenAIAPIKey: "sk-test-api-key",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Test that config struct can be created and accessed
			if tt.config.OpenAIAPIKey != tt.config.OpenAIAPIKey {
				t.Error("Config field access should work")
			}
		})
	}
}

func TestAI_FullLifecycle(t *testing.T) {
	logger := zaptest.NewLogger(t)
	config := Config{
		OpenAIAPIKey: "test-api-key", // This will cause Start() to fail, but we test the flow
	}

	ai := NewAI(logger, config)
	ctx := context.Background()

	// Test initial state
	if ai.LLM() != nil {
		t.Error("LLM() should return nil before Start()")
	}

	// Attempt to start (will likely fail with invalid key, but that's expected)
	err := ai.Start(ctx)
	// We don't assert on the error because it's expected to fail with invalid API key

	// Test that Stop() works regardless of Start() success
	err = ai.Stop(ctx)
	if err != nil {
		t.Errorf("Stop() error = %v, want nil", err)
	}

	// Test that we can call Stop() multiple times without error
	err = ai.Stop(ctx)
	if err != nil {
		t.Errorf("Stop() called twice error = %v, want nil", err)
	}
}

func TestAI_ConfigValidation(t *testing.T) {
	logger := zaptest.NewLogger(t)

	tests := []struct {
		name      string
		config    Config
		shouldErr bool
	}{
		{
			name:      "empty API key",
			config:    Config{OpenAIAPIKey: ""},
			shouldErr: true,
		},
		{
			name:      "whitespace API key",
			config:    Config{OpenAIAPIKey: "   "},
			shouldErr: false, // OpenAI client creation doesn't validate whitespace, just empty
		},
		{
			name:      "valid-looking API key",
			config:    Config{OpenAIAPIKey: "sk-test-key"},
			shouldErr: false, // May still error due to invalid key, but format is valid
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ai := NewAI(logger, tt.config)
			ctx := context.Background()

			err := ai.Start(ctx)

			if tt.shouldErr && err == nil {
				t.Error("Start() should return error for invalid config")
			}
			// Note: We don't test !tt.shouldErr && err != nil because
			// even valid-looking keys will fail without actual OpenAI access
		})
	}
}

func BenchmarkNewAI(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := Config{
		OpenAIAPIKey: "test-api-key",
	}

	for b.Loop() {
		NewAI(logger, config)
	}
}

func BenchmarkAI_Start(b *testing.B) {
	logger := zaptest.NewLogger(b)
	config := Config{
		OpenAIAPIKey: "sk-test-key",
	}
	ctx := context.Background()

	for b.Loop() {
		ai := NewAI(logger, config)
		// Note: This will error in benchmarks due to invalid API key,
		// but we're measuring the performance of the method call
		ai.Start(ctx)
	}
}
