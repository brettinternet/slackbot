package bot

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
	"slackbot.arpa/bot/config"
	"slackbot.arpa/bot/http"
	"slackbot.arpa/bot/obituary"
	"slackbot.arpa/bot/slack"
	"slackbot.arpa/logger"
)

type Bot struct {
	BuildOpts  config.BuildOpts
	logger     logger.Logger
	log        *zap.Logger
	config     config.Config
	httpServer *http.Server
	slack      *slack.Slack
	obituary   *obituary.Obituary
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
	s.httpServer = http.NewServer(s.log, s.config.Server)
	s.slack = slack.NewSlack(s.log, s.config.Slack)

	if config.HasFeature(s.config.Features, config.FeatureObituary) {
		s.obituary = obituary.NewObituary(s.log, s.config.Obituary)
	}

	return ctx, nil
}

func (s *Bot) Run(runCtx context.Context) error {
	if err := s.slack.Start(runCtx); err != nil {
		return fmt.Errorf("start slack: %w", err)
	}
	if s.obituary != nil {
		client := s.slack.Client()
		if client == nil {
			return errors.New("slack client is unavailable")
		}
		if err := s.obituary.Start(runCtx, client); err != nil {
			return fmt.Errorf("start obituary: %w", err)
		}
	}

	return s.httpServer.Run(runCtx)
}

func (s *Bot) BeginShutdown(ctx context.Context) error {
	if err := s.httpServer.Shutdown(ctx); err != nil {
		return fmt.Errorf("begin shutdown http server: %w", err)
	}
	return nil
}

// Shutdown resources in reverse order of the Setup/Run
func (s *Bot) Shutdown(ctx context.Context) error {
	var errs error
	// Sync throws an error when logging to console (sync is for buffered file logging)
	// `sync /dev/stderr: inappropriate ioctl for device`
	// https://github.com/uber-go/zap/issues/880
	// https://github.com/uber-go/zap/issues/991#issuecomment-962098428
	if err := s.httpServer.Shutdown(ctx); err != nil {
		errs = errors.Join(errs, fmt.Errorf("shutdown http server: %w", err))
	}
	if s.obituary != nil {
		if err := s.obituary.Stop(ctx); err != nil {
			errs = errors.Join(errs, fmt.Errorf("stop obituary: %w", err))
		}
	}
	if err := s.slack.Stop(ctx); err != nil {
		errs = errors.Join(errs, fmt.Errorf("stop slack: %w", err))
	}
	if err := s.log.Sync(); err != nil && !errors.Is(err, syscall.ENOTTY) {
		errs = errors.Join(errs, fmt.Errorf("sync logger: %w", err))
	}
	return errs
}

func (s *Bot) ForceShutdown(ctx context.Context) error {
	return nil
}

func (s *Bot) Logger() *zap.Logger {
	return s.logger.Get()
}
