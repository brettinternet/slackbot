package logger

import (
	"os"
	"strings"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

type LoggerOpts struct {
	Level        string
	IsProduction bool
	JSONConsole  bool // Whether to use JSON encoding for the console output
}

// Use zap WrapCore if interface is required
func NewZapLogger(opts LoggerOpts) (*zap.Logger, zap.AtomicLevel, error) {
	if opts.Level == "none" {
		return zap.NewNop(), zap.AtomicLevel{}, nil
	}
	level, err := zap.ParseAtomicLevel(opts.Level)
	if err != nil {
		return nil, level, err
	}
	var ecfg zapcore.EncoderConfig
	if opts.IsProduction {
		ecfg = zap.NewProductionEncoderConfig()
	} else {
		ecfg = zap.NewDevelopmentEncoderConfig()
	}
	ecfg.EncodeTime = zapcore.ISO8601TimeEncoder

	var cores []zapcore.Core
	if opts.JSONConsole {
		if consoleCore := consoleJSONEncoder(ecfg, level); consoleCore != nil {
			cores = append(cores, consoleCore)
		}
	} else {
		if consoleCore := consoleEncoder(ecfg, level); consoleCore != nil {
			cores = append(cores, consoleCore)
		}
	}
	core := redactingCore{Core: zapcore.NewTee(cores...)}
	return zap.New(core), level, err
}

const redactedValue = "[REDACTED]"

type redactingCore struct {
	zapcore.Core
}

func (c redactingCore) With(fields []zapcore.Field) zapcore.Core {
	return redactingCore{Core: c.Core.With(redactFields(fields))}
}

func (c redactingCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return checked.AddCore(entry, c)
	}
	return checked
}

func (c redactingCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	return c.Core.Write(entry, redactFields(fields))
}

func redactFields(fields []zapcore.Field) []zapcore.Field {
	var result []zapcore.Field
	for i := range fields {
		if !sensitiveLogKey(fields[i].Key) {
			continue
		}
		if result == nil {
			result = append([]zapcore.Field(nil), fields...)
		}
		result[i] = zap.String(fields[i].Key, redactedValue)
	}
	if result != nil {
		return result
	}
	return fields
}

func sensitiveLogKey(key string) bool {
	canonical := strings.Map(func(character rune) rune {
		if character >= 'A' && character <= 'Z' {
			return character + ('a' - 'A')
		}
		if character >= 'a' && character <= 'z' || character >= '0' && character <= '9' {
			return character
		}
		return -1
	}, key)
	for _, suffix := range []string{
		"apikey", "authorization", "body", "error", "message", "panic", "password", "prompt",
		"secret", "text", "token",
	} {
		if strings.HasSuffix(canonical, suffix) {
			return true
		}
	}
	return false
}

// Core to write pretty output to the console
func consoleEncoder(ecfg zapcore.EncoderConfig, level zap.AtomicLevel) zapcore.Core {
	ecfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
	consoleEncoder := zapcore.NewConsoleEncoder(ecfg)
	return zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), level)
}

// Core to write only JSON to the console
func consoleJSONEncoder(ecfg zapcore.EncoderConfig, level zap.AtomicLevel) zapcore.Core {
	consoleEncoder := zapcore.NewJSONEncoder(ecfg)
	return zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), level)
}

type Logger struct {
	logger *zap.Logger
	level  zap.AtomicLevel
}

// New wrapped Zap logger.
func NewLogger(opts LoggerOpts) (Logger, error) {
	logger, level, err := NewZapLogger(opts)
	return Logger{logger, level}, err
}

func NewNoopLogger() Logger {
	return Logger{logger: zap.NewNop(), level: zap.AtomicLevel{}}
}

// Return usable Zap logger.
func (l Logger) Get() *zap.Logger {
	return l.logger
}

// Change the log level at runtime
func (l Logger) SetLevel(level zapcore.Level) {
	l.level.SetLevel(level)
}

// Change the log level at runtime
func (l Logger) SetLevelStr(input string) error {
	level, err := zap.ParseAtomicLevel(input)
	if err != nil {
		return err
	}
	l.level.SetLevel(level.Level())
	return nil
}
