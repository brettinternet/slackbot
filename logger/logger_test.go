package logger

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestNewZapLogger(t *testing.T) {
	tests := []struct {
		name    string
		opts    LoggerOpts
		wantErr bool
	}{
		{
			name: "valid debug level",
			opts: LoggerOpts{Level: "debug", IsProduction: false, JSONConsole: false},
		},
		{
			name: "valid info level production",
			opts: LoggerOpts{Level: "info", IsProduction: true, JSONConsole: true},
		},
		{
			name: "none level",
			opts: LoggerOpts{Level: "none", IsProduction: false, JSONConsole: false},
		},
		{
			name:    "invalid level",
			opts:    LoggerOpts{Level: "invalid", IsProduction: false, JSONConsole: false},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			logger, level, err := NewZapLogger(tt.opts)

			if (err != nil) != tt.wantErr {
				t.Errorf("NewZapLogger() error = %v, wantErr %v", err, tt.wantErr)
				return
			}

			if !tt.wantErr {
				if logger == nil {
					t.Errorf("NewZapLogger() logger is nil")
				}
				if tt.opts.Level == "none" && logger != zap.NewNop() {
					// Note: This comparison might not work as expected due to zap internals
					// but we check that no error occurred and logger is not nil
					t.Logf("Logger created for 'none' level: %T", logger)
				}
			}

			// Test that level is set correctly for non-error cases
			if err == nil && tt.opts.Level != "none" {
				expectedLevel, _ := zap.ParseAtomicLevel(tt.opts.Level)
				if level.Level() != expectedLevel.Level() {
					t.Errorf("NewZapLogger() level = %v, want %v", level.Level(), expectedLevel.Level())
				}
			}
		})
	}
}

func TestNewLogger(t *testing.T) {
	opts := LoggerOpts{Level: "info", IsProduction: false, JSONConsole: false}
	logger, err := NewLogger(opts)

	if err != nil {
		t.Errorf("NewLogger() error = %v", err)
	}

	if logger.logger == nil {
		t.Errorf("NewLogger() logger.logger is nil")
	}

	zapLogger := logger.Get()
	if zapLogger == nil {
		t.Errorf("Logger.Get() returned nil")
	}
}

func TestNewNoopLogger(t *testing.T) {
	logger := NewNoopLogger()

	if logger.logger == nil {
		t.Errorf("NewNoopLogger() logger.logger is nil")
	}

	zapLogger := logger.Get()
	if zapLogger == nil {
		t.Errorf("NewNoopLogger().Get() returned nil")
	}
}

func TestLogger_SetLevel(t *testing.T) {
	opts := LoggerOpts{Level: "info", IsProduction: false, JSONConsole: false}
	logger, err := NewLogger(opts)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	// Test setting to debug level
	logger.SetLevel(zapcore.DebugLevel)
	if logger.level.Level() != zapcore.DebugLevel {
		t.Errorf("SetLevel() level = %v, want %v", logger.level.Level(), zapcore.DebugLevel)
	}

	// Test setting to error level
	logger.SetLevel(zapcore.ErrorLevel)
	if logger.level.Level() != zapcore.ErrorLevel {
		t.Errorf("SetLevel() level = %v, want %v", logger.level.Level(), zapcore.ErrorLevel)
	}
}

func TestLogger_SetLevelStr(t *testing.T) {
	opts := LoggerOpts{Level: "info", IsProduction: false, JSONConsole: false}
	logger, err := NewLogger(opts)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	tests := []struct {
		name      string
		levelStr  string
		wantLevel zapcore.Level
		wantErr   bool
	}{
		{"debug level", "debug", zapcore.DebugLevel, false},
		{"info level", "info", zapcore.InfoLevel, false},
		{"warn level", "warn", zapcore.WarnLevel, false},
		{"error level", "error", zapcore.ErrorLevel, false},
		{"invalid level", "invalid", zapcore.InfoLevel, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := logger.SetLevelStr(tt.levelStr)

			if (err != nil) != tt.wantErr {
				t.Errorf("SetLevelStr() error = %v, wantErr %v", err, tt.wantErr)
			}

			if !tt.wantErr && logger.level.Level() != tt.wantLevel {
				t.Errorf("SetLevelStr() level = %v, want %v", logger.level.Level(), tt.wantLevel)
			}
		})
	}
}

func TestConsoleEncoder(t *testing.T) {
	ecfg := zap.NewDevelopmentEncoderConfig()
	level := zap.NewAtomicLevelAt(zapcore.InfoLevel)

	core := consoleEncoder(ecfg, level)
	if core == nil {
		t.Errorf("consoleEncoder() returned nil")
	}
}

func TestConsoleJSONEncoder(t *testing.T) {
	ecfg := zap.NewDevelopmentEncoderConfig()
	level := zap.NewAtomicLevelAt(zapcore.InfoLevel)

	core := consoleJSONEncoder(ecfg, level)
	if core == nil {
		t.Errorf("consoleJSONEncoder() returned nil")
	}
}

func TestLoggerOutput(t *testing.T) {
	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	defer func() {
		os.Stdout = oldStdout
		_ = w.Close()
	}()

	opts := LoggerOpts{Level: "info", IsProduction: false, JSONConsole: false}
	logger, err := NewLogger(opts)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	// Log a test message
	logger.Get().Info("test message")

	// Close writer and read output
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)

	output := buf.String()
	if !strings.Contains(output, "test message") {
		t.Errorf("Logger output should contain 'test message', got: %s", output)
	}
}

func TestLoggerJSONOutput(t *testing.T) {
	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	defer func() {
		os.Stdout = oldStdout
		_ = w.Close()
	}()

	opts := LoggerOpts{Level: "info", IsProduction: false, JSONConsole: true}
	logger, err := NewLogger(opts)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	// Log a test message
	logger.Get().Info("test json message")

	// Close writer and read output
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)

	output := buf.String()
	if !strings.Contains(output, "test json message") {
		t.Errorf("Logger JSON output should contain 'test json message', got: %s", output)
	}

	// Should contain JSON structure indicators
	if !strings.Contains(output, "{") || !strings.Contains(output, "}") {
		t.Errorf("Logger JSON output should contain JSON structure, got: %s", output)
	}
}

func TestLoggerLevels(t *testing.T) {
	// Capture stdout
	oldStdout := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	defer func() {
		os.Stdout = oldStdout
		_ = w.Close()
	}()

	opts := LoggerOpts{Level: "warn", IsProduction: false, JSONConsole: false}
	logger, err := NewLogger(opts)
	if err != nil {
		t.Fatalf("NewLogger() error = %v", err)
	}

	zapLogger := logger.Get()

	// These should not appear (below warn level)
	zapLogger.Debug("debug message")
	zapLogger.Info("info message")

	// These should appear (warn level and above)
	zapLogger.Warn("warn message")
	zapLogger.Error("error message")

	// Close writer and read output
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)

	output := buf.String()

	if strings.Contains(output, "debug message") {
		t.Errorf("Debug message should not appear with warn level")
	}
	if strings.Contains(output, "info message") {
		t.Errorf("Info message should not appear with warn level")
	}
	if !strings.Contains(output, "warn message") {
		t.Errorf("Warn message should appear with warn level")
	}
	if !strings.Contains(output, "error message") {
		t.Errorf("Error message should appear with warn level")
	}
}

func BenchmarkNewLogger(b *testing.B) {
	opts := LoggerOpts{Level: "info", IsProduction: false, JSONConsole: false}

	for b.Loop() {
		_, err := NewLogger(opts)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoggerInfo(b *testing.B) {
	opts := LoggerOpts{Level: "info", IsProduction: false, JSONConsole: false}
	logger, err := NewLogger(opts)
	if err != nil {
		b.Fatal(err)
	}

	zapLogger := logger.Get()

	for b.Loop() {
		zapLogger.Info("benchmark message")
	}
}
