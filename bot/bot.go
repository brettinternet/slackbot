package bot

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
	"slackbot.arpa/bot/ai"
	"slackbot.arpa/bot/aichat"
	"slackbot.arpa/bot/chat"
	"slackbot.arpa/bot/config"
	"slackbot.arpa/bot/http"
	"slackbot.arpa/bot/slack"
	"slackbot.arpa/bot/user"
	"slackbot.arpa/bot/vibecheck"
	"slackbot.arpa/logger"
)

type Bot struct {
	BuildOpts     config.BuildOpts
	logger        logger.Logger
	log           *zap.Logger
	configManager config.ConfigProvider
	http          *http.Server
	slack         *slack.Slack
	userWatch     *user.UserWatch
	chat          *chat.Chat
	vibecheck     *vibecheck.Vibecheck
	ai            *ai.AI
	aichat        *aichat.AIChat
}

func NewBot(buildOpts config.BuildOpts) *Bot {
	return &Bot{
		BuildOpts: buildOpts,
	}
}

func (s *Bot) Setup(ctx context.Context, cmd *cli.Command) (context.Context, error) {
	var err error
	cliOverrides := config.ExtractCLIOverrides(cmd)

	configPath := "./config.yaml"
	if cliOverrides.ConfigFile != nil {
		configPath = *cliOverrides.ConfigFile
	}

	logLevel := "info"
	if cliOverrides.LogLevel != nil {
		logLevel = *cliOverrides.LogLevel
	}

	isProd := s.BuildOpts.BuildEnvironment == "production"
	if cliOverrides.Environment != nil {
		isProd = *cliOverrides.Environment == "production"
	}

	s.logger, err = logger.NewLogger(logger.LoggerOpts{
		Level:        logLevel,
		IsProduction: isProd,
		JSONConsole:  isProd,
	})
	if err != nil {
		return ctx, fmt.Errorf("logger setup: %w", err)
	}
	s.log = s.logger.Get()

	s.configManager, err = config.NewConfigManager(s.log, s.BuildOpts, cliOverrides, configPath)
	if err != nil {
		return ctx, fmt.Errorf("config manager setup: %w", err)
	}

	currentConfig := s.configManager.GetConfig()
	if currentConfig == nil {
		return ctx, fmt.Errorf("failed to get initial configuration")
	}

	s.logger, err = logger.NewLogger(logger.LoggerOpts{
		Level:        currentConfig.LogLevel,
		IsProduction: currentConfig.Environment == config.EnvironmentProduction,
		JSONConsole:  currentConfig.Environment == config.EnvironmentProduction,
	})
	if err != nil {
		return ctx, fmt.Errorf("logger setup: %w", err)
	}
	s.log = s.logger.Get()

	// Initialize services with live config
	s.slack = slack.NewSlack(s.log, s.configManager.GetSlackConfig())
	if err := s.slack.Setup(ctx); err != nil {
		return ctx, fmt.Errorf("setup slack service: %w", err)
	}

	s.userWatch = user.NewUserWatch(s.log, s.configManager.GetUserConfig(), s.slack)

	// Initialize services conditionally based on their configuration
	s.initializeServices(ctx, currentConfig)

	s.http = http.NewServer(s.log, s.configManager.GetHTTPConfig(), s.slack)

	// Subscribe to config changes for dynamic service reconfiguration
	s.configManager.Subscribe(s.onConfigChange)

	return ctx, nil
}

// initializeServices conditionally initializes services based on configuration
func (s *Bot) initializeServices(ctx context.Context, currentConfig *config.Config) {
	// Only initialize chat service if there are chat responses configured
	fileConfig := s.configManager.GetConfig()
	var chatResponses int
	if fileConfig != nil {
		// Load current file config to check responses
		var fc config.FileConfig
		if currentConfig.ConfigFile != "" {
			if err := config.ReadConfig(currentConfig.ConfigFile, &fc); err == nil {
				chatResponses = len(fc.Chat.Responses)
			}
		}
	}

	if chatResponses > 0 {
		s.chat = chat.NewChat(s.log, s.configManager.GetChatConfig(), s.slack)
		s.log.Info("Chat service initialized", zap.Int("responses", chatResponses))
	} else {
		s.log.Info("Chat service disabled - no responses configured")
	}

	// Only initialize vibecheck service if there are reactions configured
	var hasReactions bool
	if fileConfig != nil {
		var fc config.FileConfig
		if currentConfig.ConfigFile != "" {
			if err := config.ReadConfig(currentConfig.ConfigFile, &fc); err == nil {
				hasReactions = len(fc.Vibecheck.GoodReactions) > 0 || len(fc.Vibecheck.BadReactions) > 0
			}
		}
	}

	if hasReactions {
		s.vibecheck = vibecheck.NewVibecheck(s.log, s.configManager.GetVibecheckConfig(), s.slack)
		s.log.Info("Vibecheck service initialized")
	} else {
		s.log.Info("Vibecheck service disabled - no reactions configured")
	}

	// Only initialize AI services if OpenAI API key is provided
	aiConfig := s.configManager.GetAIConfig()
	if aiConfig.OpenAIAPIKey != "" {
		s.ai = ai.NewAI(s.log, aiConfig)

		// Only initialize aichat service if there are personas configured
		aichatConfig := s.configManager.GetAIChatConfig()
		if len(aichatConfig.Personas) > 0 {
			s.aichat = aichat.NewAIChat(s.log, aichatConfig, s.slack, s.ai)
			s.log.Info("AI Chat service initialized", zap.Int("personas", len(aichatConfig.Personas)))
		} else {
			s.log.Info("AI Chat service disabled - no personas configured")
		}
	} else {
		s.log.Info("AI services disabled - no OpenAI API key provided")
	}
}

// onConfigChange handles configuration changes and reconfigures services
func (s *Bot) onConfigChange(newConfig *config.Config) {
	s.log.Info("Configuration changed, updating services")

	// Update logger if log level changed
	if s.log != nil {
		newLogger, err := logger.NewLogger(logger.LoggerOpts{
			Level:        newConfig.LogLevel,
			IsProduction: newConfig.Environment == config.EnvironmentProduction,
			JSONConsole:  newConfig.Environment == config.EnvironmentProduction,
		})
		if err != nil {
			s.log.Error("Failed to update logger with new config", zap.Error(err))
		} else {
			s.logger = newLogger
			s.log = s.logger.Get()
			s.log.Info("Logger updated with new configuration")
		}
	}

	// Note: Services will use the updated config from ConfigManager automatically
	// Some services may need to be reinitialized for certain config changes

	// Note: AI services may need restart for some changes (like API keys)
	// For now, we'll just log the change
	if s.ai != nil || s.aichat != nil {
		s.log.Info("AI service configuration changed - may require restart for some changes")
	}

	s.log.Info("Service configuration update completed")
}

func (s *Bot) Run(runCtx context.Context) error {
	if err := s.slack.Start(runCtx); err != nil {
		return fmt.Errorf("start slack service: %w", err)
	}

	// ConfigManager is already running and providing live config updates

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

	if s.userWatch != nil {
		if err := s.userWatch.Start(runCtx); err != nil {
			return fmt.Errorf("start user watch: %w", err)
		}
	}

	if s.ai != nil {
		if err := s.ai.Start(runCtx); err != nil {
			return fmt.Errorf("start ai: %w", err)
		}
	}

	if s.aichat != nil {
		s.http.RegisterEventProcessor(s.aichat)
		if err := s.aichat.Start(runCtx); err != nil {
			return fmt.Errorf("start aichat: %w", err)
		}
	}

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
	if s.aichat != nil {
		if err := s.aichat.Stop(ctx); err != nil {
			return fmt.Errorf("stop aichat: %w", err)
		}
	}
	if s.userWatch != nil {
		if err := s.userWatch.Stop(ctx); err != nil {
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
	if s.configManager != nil {
		if err := s.configManager.Close(); err != nil {
			errs = errors.Join(errs, fmt.Errorf("close config manager: %w", err))
		}
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
	if s.log != nil {
		return s.log
	}
	// Return a no-op logger if not initialized
	return zap.NewNop()
}
