package config

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/urfave/cli/v3"
	"go.uber.org/zap"

	"slackbot.arpa/bot/ai"
	"slackbot.arpa/bot/aichat"
	"slackbot.arpa/bot/chat"
	"slackbot.arpa/bot/http"
	"slackbot.arpa/bot/slack"
	"slackbot.arpa/bot/user"
	"slackbot.arpa/bot/vibecheck"
)

// ConfigProvider provides access to live configuration with hot-reload support
type ConfigProvider interface {
	GetConfig() *Config
	GetAIChatConfig() aichat.Config
	GetChatConfig() chat.Config
	GetVibecheckConfig() vibecheck.Config
	GetSlackConfig() slack.Config
	GetAIConfig() ai.Config
	GetUserConfig() user.Config
	GetHTTPConfig() http.Config
	Subscribe(callback func(*Config)) func() // Returns unsubscribe function
	Close() error
}

// CLIOverrides holds values explicitly set via CLI flags
type CLIOverrides struct {
	// Global settings
	LogLevel    *string
	Environment *string
	DataDir     *string
	ConfigFile  *string

	// Server settings
	ServerPort     *uint32
	SlackEventPath *string

	// Slack settings
	SlackToken         *string
	SlackSigningSecret *string
	PreferredUsers     []string
	PreferredChannels  []string
	UserNotifyChannel  *string

	// AI settings
	OpenAIAPIKey *string

	// AI Chat settings
	PersonasConfig         *string
	PersonasStickyDuration *time.Duration
	MaxContextMessages     *int
	MaxContextAge          *time.Duration
	MaxContextTokens       *int

	// Vibecheck settings
	VibecheckBanDuration *time.Duration
}

// ConfigManager manages unified configuration with hot-reload support
type ConfigManager struct {
	log          *zap.Logger
	cliOverrides *CLIOverrides
	buildOpts    BuildOpts

	// Hot-reloadable file config
	fileConfig atomic.Pointer[FileConfig]

	// Merged config cache
	mergedConfig atomic.Pointer[Config]

	// File watching
	watcher    *fsnotify.Watcher
	configPath string

	// Subscribers for config changes
	subscribers []func(*Config)
	subsMutex   sync.RWMutex

	// Control
	ctx    context.Context
	cancel context.CancelFunc
}

// NewConfigManager creates a new configuration manager
func NewConfigManager(log *zap.Logger, buildOpts BuildOpts, cliOverrides *CLIOverrides, configPath string) (*ConfigManager, error) {
	ctx, cancel := context.WithCancel(context.Background())

	cm := &ConfigManager{
		log:          log,
		cliOverrides: cliOverrides,
		buildOpts:    buildOpts,
		configPath:   configPath,
		ctx:          ctx,
		cancel:       cancel,
	}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("failed to create file watcher: %w", err)
	}
	cm.watcher = watcher

	if err := cm.loadFileConfig(); err != nil {
		log.Warn("Failed to load initial config file, using defaults",
			zap.String("path", configPath),
			zap.Error(err))
		cm.fileConfig.Store(&FileConfig{}) // Empty config as fallback
	}

	if err := cm.rebuildMergedConfig(); err != nil {
		_ = cm.Close()
		return nil, fmt.Errorf("failed to build initial config: %w", err)
	}

	if configPath != "" {
		if err := cm.startWatching(); err != nil {
			log.Warn("Failed to watch config file",
				zap.String("path", configPath),
				zap.Error(err))
		}
	}

	return cm, nil
}

// loadFileConfig loads configuration from file
func (cm *ConfigManager) loadFileConfig() error {
	if cm.configPath == "" {
		return fmt.Errorf("no config file path specified")
	}

	var fileConfig FileConfig
	err := ReadConfig(cm.configPath, &fileConfig)
	if err != nil {
		return err
	}

	cm.fileConfig.Store(&fileConfig)
	cm.log.Debug("Loaded configuration from file", zap.String("path", cm.configPath))
	return nil
}

// rebuildMergedConfig merges CLI overrides with file config
func (cm *ConfigManager) rebuildMergedConfig() error {
	fileConfig := cm.fileConfig.Load()
	if fileConfig == nil {
		fileConfig = &FileConfig{}
	}

	opts := cm.mergeConfigs(fileConfig)

	config, err := newConfig(opts)
	if err != nil {
		return fmt.Errorf("failed to build merged config: %w", err)
	}

	cm.mergedConfig.Store(&config)
	cm.log.Debug("Rebuilt merged configuration")
	return nil
}

// mergeConfigs merges file config with CLI overrides, giving precedence to CLI
func (cm *ConfigManager) mergeConfigs(fileConfig *FileConfig) configOpts {
	opts := configOpts{
		Version:     cm.buildOpts.BuildVersion,
		BuildTime:   cm.buildOpts.BuildTime,
		Environment: cm.buildOpts.BuildEnvironment,
	}

	opts.LogLevel = stringWithOverride("info", cm.cliOverrides.LogLevel)
	opts.Environment = stringWithOverride(cm.buildOpts.BuildEnvironment, cm.cliOverrides.Environment)
	opts.DataDir = stringWithOverride("./", cm.cliOverrides.DataDir)
	opts.ConfigFile = stringWithOverride("./config.yaml", cm.cliOverrides.ConfigFile)
	opts.ServerPort = uint32WithOverride(4200, cm.cliOverrides.ServerPort)
	opts.SlackEventsPath = stringWithOverride("/api/slack/events", cm.cliOverrides.SlackEventPath)

	opts.SlackToken = stringWithOverride("", cm.cliOverrides.SlackToken)
	opts.SlackSigningSecret = stringWithOverride("", cm.cliOverrides.SlackSigningSecret)
	opts.PreferredUsers = cm.cliOverrides.PreferredUsers
	opts.PreferredChannels = cm.cliOverrides.PreferredChannels
	opts.UserNotifyChannel = stringWithOverride("", cm.cliOverrides.UserNotifyChannel)

	opts.OpenAIAPIKey = stringWithOverride("", cm.cliOverrides.OpenAIAPIKey)

	userConfig := fileConfig.User
	if userConfig.NotifyChannel != nil && cm.cliOverrides.UserNotifyChannel == nil {
		opts.UserNotifyChannel = *userConfig.NotifyChannel
	}

	aichatConfig := fileConfig.AIChat
	opts.PersonasConfig = stringWithOverride(serializePersonas(aichatConfig.Personas), cm.cliOverrides.PersonasConfig)
	opts.PersonasStickyDuration = durationWithFileAndOverride(
		aichatConfig.StickyDuration, 30*time.Minute, cm.cliOverrides.PersonasStickyDuration)
	opts.AIChatMaxContextMessages = intWithFileAndOverride(
		aichatConfig.MaxContextMessages, 10, cm.cliOverrides.MaxContextMessages)
	opts.AIChatMaxContextAge = durationWithFileAndOverride(
		aichatConfig.MaxContextAge, 24*time.Hour, cm.cliOverrides.MaxContextAge)
	opts.AIChatMaxContextTokens = intWithFileAndOverride(
		aichatConfig.MaxContextTokens, 2000, cm.cliOverrides.MaxContextTokens)

	vibecheckConfig := fileConfig.Vibecheck
	opts.VibecheckBanDuration = durationWithFileAndOverride(
		vibecheckConfig.BanDuration, 5*time.Minute, cm.cliOverrides.VibecheckBanDuration)

	chatConfig := fileConfig.Chat
	opts.ChatResponses = chatConfig.Responses

	return opts
}

// startWatching starts watching the config file for changes
func (cm *ConfigManager) startWatching() error {
	if cm.configPath == "" {
		return nil
	}

	// Watch the directory containing the config file
	configDir := filepath.Dir(cm.configPath)
	if err := cm.watcher.Add(configDir); err != nil {
		return fmt.Errorf("failed to watch config directory: %w", err)
	}

	// Start the watcher goroutine
	go cm.watchLoop()

	cm.log.Info("Started watching config file", zap.String("path", cm.configPath))
	return nil
}

// watchLoop processes file system events
func (cm *ConfigManager) watchLoop() {
	for {
		select {
		case <-cm.ctx.Done():
			return

		case event, ok := <-cm.watcher.Events:
			if !ok {
				return
			}

			// Only process events for our config file
			if event.Name != cm.configPath {
				continue
			}

			// Handle write events (file modified)
			if event.Op&fsnotify.Write == fsnotify.Write {
				cm.handleConfigChange()
			}

		case err, ok := <-cm.watcher.Errors:
			if !ok {
				return
			}
			cm.log.Error("Config file watcher error", zap.Error(err))
		}
	}
}

// handleConfigChange reloads config when file changes
func (cm *ConfigManager) handleConfigChange() {
	cm.log.Info("Config file changed, reloading", zap.String("path", cm.configPath))

	// Small delay to avoid partial write issues
	time.Sleep(100 * time.Millisecond)

	// Reload file config
	if err := cm.loadFileConfig(); err != nil {
		cm.log.Error("Failed to reload config file",
			zap.String("path", cm.configPath),
			zap.Error(err))
		return
	}

	// Rebuild merged config
	if err := cm.rebuildMergedConfig(); err != nil {
		cm.log.Error("Failed to rebuild merged config", zap.Error(err))
		return
	}

	// Notify subscribers
	config := cm.mergedConfig.Load()
	if config != nil {
		cm.notifySubscribers(config)
	}

	cm.log.Info("Configuration reloaded successfully")
}

// notifySubscribers notifies all subscribers of config changes
func (cm *ConfigManager) notifySubscribers(config *Config) {
	cm.subsMutex.RLock()
	defer cm.subsMutex.RUnlock()

	for _, callback := range cm.subscribers {
		// Run callbacks in goroutines to avoid blocking
		go func(cb func(*Config)) {
			defer func() {
				if r := recover(); r != nil {
					cm.log.Error("Config subscriber callback panicked",
						zap.Any("panic", r))
				}
			}()
			cb(config)
		}(callback)
	}
}

// ConfigProvider interface implementations

func (cm *ConfigManager) GetConfig() *Config {
	return cm.mergedConfig.Load()
}

func (cm *ConfigManager) GetAIChatConfig() aichat.Config {
	config := cm.GetConfig()
	if config == nil {
		return aichat.Config{}
	}
	return config.AIChat
}

func (cm *ConfigManager) GetChatConfig() chat.Config {
	config := cm.GetConfig()
	if config == nil {
		return chat.Config{}
	}
	return config.Chat
}

func (cm *ConfigManager) GetVibecheckConfig() vibecheck.Config {
	config := cm.GetConfig()
	if config == nil {
		return vibecheck.Config{}
	}
	return config.Vibecheck
}

func (cm *ConfigManager) GetSlackConfig() slack.Config {
	config := cm.GetConfig()
	if config == nil {
		return slack.Config{}
	}
	return config.Slack
}

func (cm *ConfigManager) GetAIConfig() ai.Config {
	config := cm.GetConfig()
	if config == nil {
		return ai.Config{}
	}
	return config.AI
}

func (cm *ConfigManager) GetUserConfig() user.Config {
	config := cm.GetConfig()
	if config == nil {
		return user.Config{}
	}
	return config.User
}

func (cm *ConfigManager) GetHTTPConfig() http.Config {
	config := cm.GetConfig()
	if config == nil {
		return http.Config{}
	}
	return config.Server
}

func (cm *ConfigManager) Subscribe(callback func(*Config)) func() {
	cm.subsMutex.Lock()
	defer cm.subsMutex.Unlock()

	cm.subscribers = append(cm.subscribers, callback)
	index := len(cm.subscribers) - 1

	// Return unsubscribe function
	return func() {
		cm.subsMutex.Lock()
		defer cm.subsMutex.Unlock()

		// Remove callback by setting to nil (avoid slice reshuffling)
		if index < len(cm.subscribers) {
			cm.subscribers[index] = nil
		}
	}
}

func (cm *ConfigManager) Close() error {
	cm.cancel()

	if cm.watcher != nil {
		return cm.watcher.Close()
	}
	return nil
}

// ExtractCLIOverrides extracts CLI overrides from urfave/cli command
func ExtractCLIOverrides(cmd *cli.Command) *CLIOverrides {
	overrides := &CLIOverrides{}

	// Extract values only if they were explicitly set (not just defaults)
	if cmd.IsSet("log-level") {
		val := cmd.String("log-level")
		overrides.LogLevel = &val
	}
	if cmd.IsSet("env") {
		val := cmd.String("env")
		overrides.Environment = &val
	}
	if cmd.IsSet("data-dir") {
		val := cmd.String("data-dir")
		overrides.DataDir = &val
	}
	if cmd.IsSet("config-file") {
		val := cmd.String("config-file")
		overrides.ConfigFile = &val
	}
	if cmd.IsSet("server-port") {
		port := cmd.Uint("server-port")
		if port > 65535 { // Check for valid port range
			port = 65535
		}
		val := uint32(port) // #nosec G115 -- port range validation above
		overrides.ServerPort = &val
	}
	if cmd.IsSet("slack-events-path") {
		val := cmd.String("slack-events-path")
		overrides.SlackEventPath = &val
	}
	if cmd.IsSet("slack-token") || cmd.String("slack-token") != "" {
		val := cmd.String("slack-token")
		overrides.SlackToken = &val
	}
	if cmd.IsSet("slack-signing-secret") || cmd.String("slack-signing-secret") != "" {
		val := cmd.String("slack-signing-secret")
		overrides.SlackSigningSecret = &val
	}
	if cmd.IsSet("slack-preferred-users") {
		overrides.PreferredUsers = cmd.StringSlice("slack-preferred-users")
	}
	if cmd.IsSet("slack-preferred-channels") {
		overrides.PreferredChannels = cmd.StringSlice("slack-preferred-channels")
	}
	if cmd.IsSet("slack-user-notify-channel") {
		val := cmd.String("slack-user-notify-channel")
		overrides.UserNotifyChannel = &val
	}
	if cmd.IsSet("openai-api-key") || cmd.String("openai-api-key") != "" {
		val := cmd.String("openai-api-key")
		overrides.OpenAIAPIKey = &val
	}
	if cmd.IsSet("personas-config") {
		val := cmd.String("personas-config")
		overrides.PersonasConfig = &val
	}
	if cmd.IsSet("personas-sticky-duration") {
		val := cmd.Duration("personas-sticky-duration")
		overrides.PersonasStickyDuration = &val
	}
	if cmd.IsSet("aichat-max-context-messages") {
		val := cmd.Int("aichat-max-context-messages")
		overrides.MaxContextMessages = &val
	}
	if cmd.IsSet("aichat-max-context-age") {
		val := cmd.Duration("aichat-max-context-age")
		overrides.MaxContextAge = &val
	}
	if cmd.IsSet("aichat-max-context-tokens") {
		val := cmd.Int("aichat-max-context-tokens")
		overrides.MaxContextTokens = &val
	}
	if cmd.IsSet("vibecheck-ban-duration") {
		val := cmd.Duration("vibecheck-ban-duration")
		overrides.VibecheckBanDuration = &val
	}

	return overrides
}

// Helper functions for configuration merging

func stringWithOverride(defaultValue string, override *string) string {
	if override != nil {
		return *override
	}
	return defaultValue
}

func uint32WithOverride(defaultValue uint32, override *uint32) uint32 {
	if override != nil {
		return *override
	}
	return defaultValue
}

func durationWithFileAndOverride(fileValue *time.Duration, defaultValue time.Duration, override *time.Duration) time.Duration {
	// CLI override takes highest precedence
	if override != nil {
		return *override
	}
	// File value takes precedence over default
	if fileValue != nil {
		return *fileValue
	}
	// Use default value
	return defaultValue
}

func intWithFileAndOverride(fileValue *int, defaultValue int, override *int) int {
	// CLI override takes highest precedence
	if override != nil {
		return *override
	}
	// File value takes precedence over default
	if fileValue != nil {
		return *fileValue
	}
	// Use default value
	return defaultValue
}

func serializePersonas(personas map[string]string) string {
	if len(personas) == 0 {
		return ""
	}
	// Convert map to YAML string for compatibility with existing parsing
	// This is a simple implementation - in practice you might want proper YAML marshaling
	result := ""
	for name, prompt := range personas {
		result += fmt.Sprintf("%s: |\n  %s\n", name, prompt)
	}
	return result
}
