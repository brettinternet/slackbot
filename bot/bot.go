package bot

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
	"slackbot.arpa/bot/chat"
	"slackbot.arpa/bot/config"
	"slackbot.arpa/bot/http"
	"slackbot.arpa/bot/obituary"
	"slackbot.arpa/bot/slack"
	"slackbot.arpa/bot/vibecheck"
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
	chat       *chat.Chat
	vibecheck  *vibecheck.Vibecheck
}

func NewBot(buildOpts config.BuildOpts) *Bot {
	return &Bot{
		BuildOpts: buildOpts,
	}
}

func (s *Bot) Setup(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	var err error
	s.config, err = s.BuildOpts.MakeConfig(cmd)
	if err != nil {
		return ctx, fmt.Errorf("config setup: %w", err)
	}

	isProd := s.config.Environment == config.EnvironmentProduction
	s.logger, err = logger.NewLogger(logger.LoggerOpts{
		Level:        s.config.LogLevel,
		IsProduction: isProd,
		JSONConsole:  isProd,
	})
	if err != nil {
		return ctx, err
	}

	s.log = s.logger.Get()
	s.slack = slack.NewSlack(s.log, s.config.Slack)
	s.httpServer = http.NewServer(s.log, s.config.Server, s.slack)

	s.log.Debug("Running bot with features.", zap.Any("features", s.config.Features))

	if config.HasFeature(s.config.Features, config.FeatureObituary) {
		s.obituary = obituary.NewObituary(s.log, s.config.Obituary)
	}

	if config.HasFeature(s.config.Features, config.FeatureChat) {
		s.chat = chat.NewChat(s.log, s.config.Chat)
	}

	if config.HasFeature(s.config.Features, config.FeatureVibecheck) {
		s.vibecheck = vibecheck.NewVibecheck(s.log)
	}

	return ctx, nil
}

func (s *Bot) Run(runCtx context.Context) error {
	if err := s.slack.Start(runCtx); err != nil {
		return fmt.Errorf("start slack: %w", err)
	}

	slackClient := s.slack.Client()
	if slackClient == nil {
		return errors.New("slack client is unavailable")
	}

	if s.chat != nil {
		s.httpServer.RegisterEventProcessor(s.chat)
		if err := s.chat.Start(runCtx, slackClient); err != nil {
			return fmt.Errorf("start chat: %w", err)
		}
	}

	if s.vibecheck != nil {
		s.httpServer.RegisterEventProcessor(s.vibecheck)
		if err := s.vibecheck.Start(runCtx, slackClient); err != nil {
			return fmt.Errorf("start vibecheck: %w", err)
		}
	}

	if s.obituary != nil {
		if err := s.obituary.Start(runCtx, slackClient); err != nil {
			return fmt.Errorf("start obituary: %w", err)
		}
	}

	return s.httpServer.Run(runCtx)
}

func (s *Bot) BeginShutdown(ctx context.Context) error {
	if err := s.httpServer.BeginShutdown(ctx); err != nil {
		return fmt.Errorf("begin shutdown http server: %w", err)
	}
	return nil
}

// Shutdown resources in reverse order of the Setup/Run
func (s *Bot) Shutdown(ctx context.Context) error {
	var errs error
	if err := s.httpServer.Shutdown(ctx); err != nil {
		errs = errors.Join(errs, fmt.Errorf("shutdown http server: %w", err))
	}
	if s.obituary != nil {
		if err := s.obituary.Stop(ctx); err != nil {
			errs = errors.Join(errs, fmt.Errorf("stop obituary: %w", err))
		}
	}
	if s.chat != nil {
		if err := s.chat.Stop(ctx); err != nil {
			errs = errors.Join(errs, fmt.Errorf("stop chat: %w", err))
		}
	}
	if s.vibecheck != nil {
		if err := s.vibecheck.Stop(ctx); err != nil {
			errs = errors.Join(errs, fmt.Errorf("stop vibecheck: %w", err))
		}
	}
	if err := s.slack.Stop(ctx); err != nil {
		errs = errors.Join(errs, fmt.Errorf("stop slack: %w", err))
	}
	// Sync throws an error when logging to console (sync is for buffered file logging)
	// `sync /dev/stderr: inappropriate ioctl for device`
	// https://github.com/uber-go/zap/issues/880
	// https://github.com/uber-go/zap/issues/991#issuecomment-962098428
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
