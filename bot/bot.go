package bot

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"syscall"
	"time"

	"github.com/urfave/cli/v3"
	"go.uber.org/zap"
	"slackbot.arpa/bot/ai"
	"slackbot.arpa/bot/aichat"
	"slackbot.arpa/bot/chat"
	"slackbot.arpa/bot/config"
	"slackbot.arpa/bot/http"
	"slackbot.arpa/bot/showerthought"
	"slackbot.arpa/bot/slack"
	"slackbot.arpa/bot/user"
	"slackbot.arpa/bot/vibecheck"
	"slackbot.arpa/logger"
)

type shutdownStep struct {
	name string
	stop func(context.Context) error
}

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
	showerThought *showerthought.ShowerThought

	lifecycleMu     sync.Mutex
	shutdownSteps   []shutdownStep
	shutdownDone    chan struct{}
	shutdownErr     error
	shutdownStarted bool
}

func NewBot(buildOpts config.BuildOpts) *Bot {
	return &Bot{
		BuildOpts: buildOpts,
	}
}

func (s *Bot) Setup(ctx context.Context, cmd *cli.Command) (_ context.Context, setupErr error) {
	defer func() {
		if setupErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(cleanupCtx); err != nil {
			setupErr = errors.Join(setupErr, fmt.Errorf("clean up partial setup: %w", err))
		}
	}()

	if s.isShuttingDown() {
		return ctx, errors.New("setup bot: shutdown already started")
	}

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
	if err := s.addShutdownStep("sync logger", func(context.Context) error {
		if err := s.log.Sync(); err != nil && !errors.Is(err, syscall.ENOTTY) && !errors.Is(err, syscall.EINVAL) {
			return err
		}
		return nil
	}); err != nil {
		return ctx, err
	}

	s.configManager, err = config.NewConfigManager(s.log, s.BuildOpts, cliOverrides, configPath)
	if err != nil {
		return ctx, fmt.Errorf("config manager setup: %w", err)
	}
	if err := s.addShutdownStep("close config manager", func(context.Context) error {
		return s.configManager.Close()
	}); err != nil {
		return ctx, err
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
	if err := s.addShutdownStep("stop slack", s.slack.Stop); err != nil {
		return ctx, err
	}

	s.userWatch = user.NewUserWatch(s.log, s.configManager.GetUserConfig(), s.slack)
	if err := s.addShutdownStep("stop user watch", s.userWatch.Stop); err != nil {
		return ctx, err
	}

	// Initialize services conditionally based on their configuration
	if err := s.initializeServices(ctx); err != nil {
		return ctx, err
	}

	s.http = http.NewServer(s.log, s.configManager.GetHTTPConfig(), s.slack)
	if err := s.addShutdownStep("shutdown http server", s.http.Shutdown); err != nil {
		return ctx, err
	}

	// Subscribe to config changes for dynamic service reconfiguration
	s.configManager.Subscribe(s.onConfigChange)

	return ctx, nil
}

// initializeServices conditionally initializes services based on configuration
func (s *Bot) initializeServices(ctx context.Context) error {
	chatConfig := s.configManager.GetChatConfig()
	chatService, err := chat.NewChat(s.log, chatConfig, s.slack)
	if err != nil {
		return fmt.Errorf("initialize chat: %w", err)
	}
	s.chat = chatService
	if err := s.addShutdownStep("stop chat", s.chat.Stop); err != nil {
		return err
	}
	s.log.Info("Chat service initialized", zap.Int("responses", len(chatConfig.Responses)))

	// Keep the service running so configuration reloads can enable it and existing bans
	// can still expire while new vibechecks are disabled.
	vibecheckConfig := s.configManager.GetVibecheckConfig()
	vibecheckService, err := vibecheck.NewVibecheck(s.log, vibecheckConfig, s.slack.Client())
	if err != nil {
		return fmt.Errorf("initialize vibecheck: %w", err)
	}
	s.vibecheck = vibecheckService
	if err := s.addShutdownStep("stop vibecheck", s.vibecheck.Stop); err != nil {
		return err
	}
	s.log.Info("Vibecheck service initialized", zap.Bool("enabled", vibecheckConfig.Enabled))

	// Only initialize AI services if OpenAI API key is provided
	aiConfig := s.configManager.GetAIConfig()
	if aiConfig.OpenAIAPIKey != "" {
		s.ai = ai.NewAI(s.log, aiConfig)
		if err := s.addShutdownStep("stop ai", s.ai.Stop); err != nil {
			return err
		}

		// Only initialize aichat service if there are personas configured
		aichatConfig := s.configManager.GetAIChatConfig()
		if len(aichatConfig.Personas) > 0 {
			s.aichat = aichat.NewAIChat(s.log, aichatConfig, s.slack, s.ai)
			if err := s.addShutdownStep("stop aichat", s.aichat.Stop); err != nil {
				return err
			}
			personaKeys := make([]string, 0, len(aichatConfig.Personas))
			for k := range aichatConfig.Personas {
				personaKeys = append(personaKeys, k)
			}
			s.log.Info("AI Chat service initialized", zap.Strings("personas", personaKeys))
		} else {
			s.log.Info("AI Chat service disabled - no personas configured")
		}

		// Only initialize showerthought if enabled and notify channel is set
		stConfig := s.configManager.GetShowerthoughtConfig()
		if stConfig.Enabled && stConfig.NotifyChannel != "" {
			s.showerThought = showerthought.New(s.log, stConfig, s.slack, s.ai)
			if err := s.addShutdownStep("stop showerthought", s.showerThought.Stop); err != nil {
				return err
			}
			s.log.Info("Shower thought service initialized",
				zap.String("channel", stConfig.NotifyChannel))
		} else if stConfig.Enabled {
			s.log.Warn("Shower thought service disabled - no notify channel configured")
		}
	} else {
		s.log.Info("AI services disabled - no OpenAI API key provided")
	}

	return nil
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

	if s.chat != nil {
		if err := s.chat.SetConfig(newConfig.Chat); err != nil {
			s.log.Error("Failed to update chat configuration", zap.Error(err))
		}
	}
	if s.vibecheck != nil {
		s.vibecheck.SetConfig(newConfig.Vibecheck)
	}

	if s.userWatch != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := s.userWatch.UpdateNotifyChannel(ctx, newConfig.User.NotifyChannel)
		cancel()
		if err != nil {
			s.log.Error("Failed to update user watcher notification channel", zap.Error(err), zap.String("channel", newConfig.User.NotifyChannel))
		}
	}

	// Note: AI services may need restart for some changes (like API keys)
	// For now, we'll just log the change
	if s.ai != nil || s.aichat != nil {
		s.log.Info("AI service configuration changed - may require restart for some changes")
	}

	s.log.Info("Service configuration update completed")
}

func (s *Bot) Run(runCtx context.Context) (runErr error) {
	defer func() {
		if runErr == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), 5*time.Second)
		defer cancel()
		if err := s.Shutdown(cleanupCtx); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("clean up failed run: %w", err))
		}
	}()

	if err := s.startService(runCtx, "slack service", s.slack.Start, s.slack.Stop); err != nil {
		return err
	}

	// ConfigManager is already running and providing live config updates

	if s.chat != nil && s.http != nil {
		s.http.RegisterEventProcessor(s.chat)
		if err := s.startService(runCtx, "chat", s.chat.Start, s.chat.Stop); err != nil {
			return err
		}
	}

	if s.vibecheck != nil && s.http != nil {
		s.http.RegisterEventProcessor(s.vibecheck)
		if err := s.startService(runCtx, "vibecheck", s.vibecheck.Start, s.vibecheck.Stop); err != nil {
			return err
		}
	}

	if s.userWatch != nil {
		if s.http != nil {
			s.http.RegisterEventProcessor(s.userWatch)
		}
		if err := s.startService(runCtx, "user watch", s.userWatch.Start, s.userWatch.Stop); err != nil {
			return err
		}
	}

	if s.ai != nil {
		if err := s.startService(runCtx, "ai", s.ai.Start, s.ai.Stop); err != nil {
			return err
		}
	}

	if s.aichat != nil {
		s.http.RegisterEventProcessor(s.aichat)
		if err := s.startService(runCtx, "aichat", s.aichat.Start, s.aichat.Stop); err != nil {
			return err
		}
	}

	if s.showerThought != nil {
		if err := s.startService(runCtx, "showerthought", s.showerThought.Start, s.showerThought.Stop); err != nil {
			return err
		}
	}

	if s.isShuttingDown() {
		return errors.New("start http server: shutdown already started")
	}
	return s.http.Run(runCtx)
}

func (s *Bot) startService(
	ctx context.Context,
	name string,
	start func(context.Context) error,
	stop func(context.Context) error,
) error {
	if s.isShuttingDown() {
		return fmt.Errorf("start %s: shutdown already started", name)
	}
	if err := start(ctx); err != nil {
		return fmt.Errorf("start %s: %w", name, err)
	}
	if !s.isShuttingDown() {
		return nil
	}

	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := stop(cleanupCtx); err != nil {
		return errors.Join(
			fmt.Errorf("start %s: shutdown started concurrently", name),
			fmt.Errorf("stop %s after concurrent shutdown: %w", name, err),
		)
	}
	return fmt.Errorf("start %s: shutdown started concurrently", name)
}

func (s *Bot) isShuttingDown() bool {
	s.lifecycleMu.Lock()
	defer s.lifecycleMu.Unlock()
	return s.shutdownStarted
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

func (s *Bot) addShutdownStep(name string, stop func(context.Context) error) error {
	s.lifecycleMu.Lock()
	if !s.shutdownStarted {
		s.shutdownSteps = append(s.shutdownSteps, shutdownStep{name: name, stop: stop})
		s.lifecycleMu.Unlock()
		return nil
	}
	s.lifecycleMu.Unlock()

	cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cleanupErr := stop(cleanupCtx)
	return errors.Join(
		fmt.Errorf("initialize %s: shutdown already started", name),
		cleanupErr,
	)
}

// Shutdown stops every initialized resource in reverse initialization order. The
// first caller performs cleanup; repeated callers receive the same result.
func (s *Bot) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	s.lifecycleMu.Lock()
	if s.shutdownStarted {
		done := s.shutdownDone
		s.lifecycleMu.Unlock()
		select {
		case <-done:
			s.lifecycleMu.Lock()
			err := s.shutdownErr
			s.lifecycleMu.Unlock()
			return err
		default:
		}
		select {
		case <-done:
			s.lifecycleMu.Lock()
			err := s.shutdownErr
			s.lifecycleMu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.shutdownStarted = true
	s.shutdownDone = make(chan struct{})
	steps := append([]shutdownStep(nil), s.shutdownSteps...)
	s.lifecycleMu.Unlock()

	var errs error
	for i := len(steps) - 1; i >= 0; i-- {
		if err := steps[i].stop(ctx); err != nil {
			errs = errors.Join(errs, fmt.Errorf("%s: %w", steps[i].name, err))
		}
	}

	s.lifecycleMu.Lock()
	s.shutdownErr = errs
	close(s.shutdownDone)
	s.lifecycleMu.Unlock()
	return errs
}

func (s *Bot) Logger() *zap.Logger {
	if s.log != nil {
		return s.log
	}
	// Return a no-op logger if not initialized
	return zap.NewNop()
}
