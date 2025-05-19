package logger

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"golang.org/x/term"
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
		if consoleCore := maybeConsoleJSONEncoder(ecfg, level); consoleCore != nil {
			cores = append(cores, consoleCore)
		}
	} else {
		if consoleCore := maybeConsoleEncoder(ecfg, level); consoleCore != nil {
			cores = append(cores, consoleCore)
		}
	}
	core := zapcore.NewTee(cores...)
	return zap.New(core), level, err
}

// Core to write pretty output to the console
func maybeConsoleEncoder(ecfg zapcore.EncoderConfig, level zap.AtomicLevel) zapcore.Core {
	if !isTTY() {
		return nil
	}
	ecfg.EncodeLevel = zapcore.CapitalColorLevelEncoder
	consoleEncoder := zapcore.NewConsoleEncoder(ecfg)
	return zapcore.NewCore(consoleEncoder, zapcore.AddSync(os.Stdout), level)
}

// Core to write only JSON to the console
func maybeConsoleJSONEncoder(ecfg zapcore.EncoderConfig, level zap.AtomicLevel) zapcore.Core {
	if !isTTY() {
		return nil
	}
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

func isTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}
