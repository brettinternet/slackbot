package bot

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
	"slackbot.arpa/bot/ai"
	"slackbot.arpa/bot/chat"
	"slackbot.arpa/bot/config"
	"slackbot.arpa/bot/http"
	"slackbot.arpa/bot/obituary"
	"slackbot.arpa/bot/slack"
	"slackbot.arpa/bot/vibecheck"
	"slackbot.arpa/logger"
)

type Bot struct {
	BuildOpts     config.BuildOpts
	logger        logger.Logger
	log           *zap.Logger
	config        config.Config
	http          *http.Server
	slack         *slack.Slack
	obituary      *obituary.Obituary
	chat          *chat.Chat
	vibecheck     *vibecheck.Vibecheck
	configWatcher *config.ConfigWatcher
	ai            *ai.AI
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
		return ctx, fmt.Errorf("logger setup: %w", err)
	}

	s.log = s.logger.Get()

	// Initialize config configWatcher if config file is specified
	if s.config.ConfigFile != "" {
		s.configWatcher, err = config.NewConfigWatcher(s.log, s.config.ConfigFile)
		if err != nil {
			return ctx, fmt.Errorf("config configWatcher setup: %w", err)
		}
	}

	s.slack = slack.NewSlack(s.log, s.config.Slack)

	if s.config.Features != nil {
		s.log.Debug("Running bot with features.", zap.Any("features", s.config.Features))
	} else {
		s.log.Warn("No features enabled.")
	}

	if err := s.slack.Setup(ctx); err != nil {
		return ctx, fmt.Errorf("start slack: %w", err)
	}

	if config.HasFeature(s.config.Features, config.FeatureObituary) {
		s.obituary = obituary.NewObituary(s.log, s.config.Obituary, s.slack)
	}

	if config.HasFeature(s.config.Features, config.FeatureChat) {
		s.chat = chat.NewChat(s.log, s.config.Chat, s.slack)
	}

	if config.HasFeature(s.config.Features, config.FeatureVibecheck) {
		s.vibecheck = vibecheck.NewVibecheck(s.log, s.config.Vibecheck, s.slack)
	}

	if config.HasFeature(s.config.Features, config.FeatureAIChat) {
		s.ai = ai.NewAI(s.log, s.config.AI)
		// s.aiChat = aichat.NewAIChat(s.log, s.ai)
	}

	s.http = http.NewServer(s.log, s.config.Server, s.slack)
	return ctx, nil
}

func (s *Bot) Run(runCtx context.Context) error {
	if err := s.slack.Start(runCtx); err != nil {
		return fmt.Errorf("start slack: %w", err)
	}

	// Start the config configWatcher if we have one
	if s.configWatcher != nil {
		// Set up callbacks for each module that needs to reload config
		if s.chat != nil {
			s.configWatcher.AddCallback("chat", func(c config.FileConfig) {
				s.log.Info("Updating chat configuration")
				s.chat.SetConfig(c.Chat)
			})
		}

		if s.vibecheck != nil {
			s.configWatcher.AddCallback("vibecheck", func(c config.FileConfig) {
				s.log.Info("Updating vibecheck configuration")
				s.vibecheck.SetConfig(c.Vibecheck)
			})
		}

		// Start the configWatcher
		if err := s.configWatcher.Start(runCtx); err != nil {
			return fmt.Errorf("start config configWatcher: %w", err)
		}
	}

	if s.chat != nil && s.http != nil {
		s.http.RegisterEventProcessor(s.chat)
		if err := s.chat.Start(runCtx); err != nil {
			return fmt.Errorf("start chat: %w", err)
		}
	}

	if s.vibecheck != nil && s.http != nil {
		s.http.RegisterEventProcessor(s.vibecheck)
		if err := s.vibecheck.Start(runCtx); err != nil {
			return fmt.Errorf("start vibecheck: %w", err)
		}
	}

	if s.obituary != nil {
		if err := s.obituary.Start(runCtx); err != nil {
			return fmt.Errorf("start obituary: %w", err)
		}
	}

	if s.ai != nil {
		if err := s.ai.Start(runCtx); err != nil {
			return fmt.Errorf("start ai: %w", err)
		}
	}

	// if s.aichat != nil {
	// 	if err := s.aichat.Start(runCtx); err != nil {
	// 		return fmt.Errorf("start aichat: %w", err)
	// 	}
	// }

	return s.http.Run(runCtx)
}

func (s *Bot) BeginShutdown(ctx context.Context) error {
	if s.http == nil {
		return nil
	}
	if err := s.http.BeginShutdown(ctx); err != nil {
		return fmt.Errorf("begin shutdown http server: %w", err)
	}
	return nil
}

// Shutdown resources in reverse order of the Setup/Run
func (s *Bot) Shutdown(ctx context.Context) error {
	var errs error
	if s.http != nil {
		if err := s.http.Shutdown(ctx); err != nil {
			errs = errors.Join(errs, fmt.Errorf("shutdown http server: %w", err))
		}
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
	if s.configWatcher != nil {
		s.configWatcher.Stop()
	}
	if err := s.slack.Stop(ctx); err != nil {
		errs = errors.Join(errs, fmt.Errorf("stop slack: %w", err))
	}
	// Sync throws an error when logging to console (sync is for buffered file logging)
	// `sync /dev/stderr: inappropriate ioctl for device`
	// https://github.com/uber-go/zap/issues/880
	// https://github.com/uber-go/zap/issues/991#issuecomment-962098428
	if err := s.log.Sync(); err != nil && !errors.Is(err, syscall.ENOTTY) && !errors.Is(err, syscall.EINVAL) {
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
