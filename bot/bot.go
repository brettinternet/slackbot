package bot

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"syscall"

	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
	"slackbot.arpa/bot/config"
	"slackbot.arpa/bot/obituary"
	"slackbot.arpa/logger"
)

type Bot struct {
	BuildOpts      config.BuildOpts
	logger         logger.Logger
	log            *zap.Logger
	config         config.Config
	isShuttingDown atomic.Bool
	obituary       *obituary.Obituary
}

func NewBot(buildOpts config.BuildOpts) *Bot {
	return &Bot{
		BuildOpts: buildOpts,
	}
}

func (s *Bot) Setup(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	s.config = s.BuildOpts.MakeConfig(cmd)

	var err error
	s.logger, err = logger.NewLogger(logger.LoggerOpts{
		Level:        s.config.LogLevel,
		IsProduction: s.config.Environment == config.EnvironmentProduction,
	})
	if err != nil {
		return ctx, err
	}

	s.log = s.logger.Get()
	s.obituary = obituary.NewObituary(s.log)

	return ctx, nil
}

func (s *Bot) Run(runCtx context.Context) error {
	var errs error
	//
	return errs
}

func (s *Bot) BeginShutdown(ctx context.Context) error {
	s.isShuttingDown.Store(true)
	var errs error
	//
	return errs
}

// Shutdown resources in reverse order of the Setup/Run
func (s *Bot) Shutdown(ctx context.Context) error {
	var errs error
	// Sync throws an error when logging to console (sync is for buffered file logging)
	// `sync /dev/stderr: inappropriate ioctl for device`
	// https://github.com/uber-go/zap/issues/880
	// https://github.com/uber-go/zap/issues/991#issuecomment-962098428
	if err := s.log.Sync(); err != nil && !errors.Is(err, syscall.ENOTTY) {
		errs = errors.Join(errs, fmt.Errorf("sync logger: %w", err))
	}
	//
	return errs
}

func (s *Bot) ForceShutdown(ctx context.Context) error {
	return nil
}

func (r *Bot) Logger() *zap.Logger {
	return r.logger.Get()
}
