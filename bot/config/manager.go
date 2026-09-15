package config

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
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
	botmetrics "slackbot.arpa/bot/metrics"
	"slackbot.arpa/bot/showerthought"
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
	GetShowerthoughtConfig() showerthought.Config
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
	ServerPort                    *uint32
	SlackEventPath                *string
	SlackEventDeduplicationWindow *time.Duration
	MetricsEnabled                *bool
	MetricsPath                   *string

	// Slack settings
	SlackToken         *string
	SlackSigningSecret *string
	PreferredUsers     []string
	PreferredChannels  []string
	UserNotifyChannel  *string

	// AI settings
	OpenAIAPIKey          *string
	OpenAIModel           *string
	OpenAIReasoningEffort *string

	// AI Chat settings
	PersonasConfig         *string
	PersonasStickyDuration *time.Duration
	MaxContextMessages     *int
	MaxContextAge          *time.Duration
	MaxContextTokens       *int
	AIChatRateLimitEnabled *bool

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
	if err := validateFileConfig(fileConfig); err != nil {
		return err
	}
	config, err := cm.buildMergedConfig(fileConfig)
	if err != nil {
		return err
	}
	cm.mergedConfig.Store(config)
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
	opts.DataDir = stringWithOverride("./tmp", cm.cliOverrides.DataDir)
	opts.ConfigFile = stringWithOverride("./config.yaml", cm.cliOverrides.ConfigFile)
	opts.ServerPort = uint32WithOverride(4200, cm.cliOverrides.ServerPort)
	opts.SlackEventsPath = stringWithOverride("/api/slack/events", cm.cliOverrides.SlackEventPath)
	opts.SlackEventDeduplicationWindow = durationWithFileAndOverride(
		fileConfig.SlackEventDeduplicationWindow,
		http.DefaultSlackEventDeduplicationWindow,
		cm.cliOverrides.SlackEventDeduplicationWindow)
	opts.MetricsEnabled = boolWithFileAndOverride(
		fileConfig.Metrics.Enabled, false, cm.cliOverrides.MetricsEnabled)
	opts.MetricsPath = stringWithFileAndOverride(
		fileConfig.Metrics.Path, botmetrics.DefaultPath, cm.cliOverrides.MetricsPath)

	opts.SlackToken = stringWithOverride("", cm.cliOverrides.SlackToken)
	opts.SlackSigningSecret = stringWithOverride("", cm.cliOverrides.SlackSigningSecret)
	opts.PreferredUsers = cm.cliOverrides.PreferredUsers
	opts.PreferredChannels = cm.cliOverrides.PreferredChannels
	opts.UserNotifyChannel = stringWithOverride("", cm.cliOverrides.UserNotifyChannel)

	opts.OpenAIAPIKey = stringWithOverride("", cm.cliOverrides.OpenAIAPIKey)
	opts.OpenAIModel = stringWithOverride(ai.DefaultModel, cm.cliOverrides.OpenAIModel)
	opts.OpenAIReasoningEffort = stringWithOverride(
		ai.DefaultReasoningEffort, cm.cliOverrides.OpenAIReasoningEffort)

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
		aichatConfig.MaxContextAge, 2*time.Hour, cm.cliOverrides.MaxContextAge)
	opts.AIChatMaxContextTokens = intWithFileAndOverride(
		aichatConfig.MaxContextTokens, 2000, cm.cliOverrides.MaxContextTokens)
	opts.AIChatRateLimitEnabled = boolWithFileAndOverride(
		aichatConfig.RateLimitEnabled, true, cm.cliOverrides.AIChatRateLimitEnabled)

	vibecheckConfig := fileConfig.Vibecheck
	opts.VibecheckBanDuration = durationWithFileAndOverride(
		vibecheckConfig.BanDuration, 5*time.Minute, cm.cliOverrides.VibecheckBanDuration)
	opts.VibecheckGoodReactions = vibecheckConfig.GoodReactions
	opts.VibecheckGoodText = vibecheckConfig.GoodText
	opts.VibecheckBadReactions = vibecheckConfig.BadReactions
	opts.VibecheckBadText = vibecheckConfig.BadText
	opts.VibecheckEnabled = len(vibecheckConfig.GoodReactions) > 0 ||
		len(vibecheckConfig.GoodText) > 0 ||
		len(vibecheckConfig.BadReactions) > 0 ||
		len(vibecheckConfig.BadText) > 0

	chatConfig := fileConfig.Chat
	opts.ChatResponses = chatConfig.Responses

	showerthoughtConfig := fileConfig.ShowerThought
	if showerthoughtConfig.Enabled != nil {
		opts.ShowerthoughtEnabled = *showerthoughtConfig.Enabled
	}
	opts.ShowerthoughtBusinessHoursStart = intWithFileAndOverride(
		showerthoughtConfig.BusinessHoursStart, 9, nil)
	opts.ShowerthoughtBusinessHoursEnd = intWithFileAndOverride(
		showerthoughtConfig.BusinessHoursEnd, 17, nil)

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

			// Handle in-place writes and atomic replacement by editors/deploy tools.
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
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

	var fileConfig FileConfig
	if err := ReadConfig(cm.configPath, &fileConfig); err != nil {
		cm.log.Error("Failed to reload config file",
			zap.String("path", cm.configPath),
			zap.Error(err))
		return
	}
	if err := validateFileConfig(&fileConfig); err != nil {
		cm.log.Error("Invalid configuration update; keeping last valid configuration",
			zap.String("path", cm.configPath),
			zap.Error(err))
		return
	}

	merged, err := cm.buildMergedConfig(&fileConfig)
	if err != nil {
		cm.log.Error("Failed to rebuild merged config; keeping last valid configuration", zap.Error(err))
		return
	}

	previous := cm.mergedConfig.Load()
	cm.fileConfig.Store(&fileConfig)
	cm.mergedConfig.Store(merged)
	if changed := restartRequiredChanges(previous, merged); len(changed) > 0 {
		cm.log.Warn("Configuration changes require a restart to take effect",
			zap.Strings("settings", changed))
	}
	cm.notifySubscribers(merged)
	cm.log.Info("Configuration reloaded successfully")
}

func (cm *ConfigManager) buildMergedConfig(fileConfig *FileConfig) (*Config, error) {
	opts := cm.mergeConfigs(fileConfig)
	merged, err := newConfig(opts)
	if err != nil {
		return nil, fmt.Errorf("failed to build merged config: %w", err)
	}
	return &merged, nil
}

func validateFileConfig(fileConfig *FileConfig) error {
	aiChat := fileConfig.AIChat
	if aiChat.StickyDuration != nil && *aiChat.StickyDuration < 0 {
		return errors.New("aichat.sticky_duration must not be negative")
	}
	if aiChat.MaxContextMessages != nil && *aiChat.MaxContextMessages <= 0 {
		return errors.New("aichat.max_context_messages must be positive")
	}
	if aiChat.MaxContextAge != nil && *aiChat.MaxContextAge <= 0 {
		return errors.New("aichat.max_context_age must be positive")
	}
	if aiChat.MaxContextTokens != nil && *aiChat.MaxContextTokens <= 0 {
		return errors.New("aichat.max_context_tokens must be positive")
	}
	start := intWithFileAndOverride(fileConfig.ShowerThought.BusinessHoursStart, 9, nil)
	end := intWithFileAndOverride(fileConfig.ShowerThought.BusinessHoursEnd, 17, nil)
	if start < 0 || start > 23 || end < 1 || end > 24 || start >= end {
		return errors.New("showerthought business hours must satisfy 0 <= start < end <= 24")
	}
	if fileConfig.SlackEventDeduplicationWindow != nil && *fileConfig.SlackEventDeduplicationWindow <= 0 {
		return errors.New("slack_event_deduplication_window must be positive")
	}
	if fileConfig.Metrics.Path != nil && (!strings.HasPrefix(*fileConfig.Metrics.Path, "/") || *fileConfig.Metrics.Path == "/") {
		return errors.New("metrics.path must be an absolute non-root path")
	}
	return nil
}

func restartRequiredChanges(oldConfig, newConfig *Config) []string {
	if oldConfig == nil || newConfig == nil {
		return nil
	}
	var changed []string
	if oldConfig.LogLevel != newConfig.LogLevel {
		changed = append(changed, "log_level")
	}
	if oldConfig.Environment != newConfig.Environment {
		changed = append(changed, "environment")
	}
	if oldConfig.DataDir != newConfig.DataDir {
		changed = append(changed, "data_dir")
	}
	if oldConfig.Server.ServerPort != newConfig.Server.ServerPort {
		changed = append(changed, "server.port")
	}
	if oldConfig.Server.SlackEventPath != newConfig.Server.SlackEventPath {
		changed = append(changed, "server.slack_events_path")
	}
	if oldConfig.Server.SlackEventDeduplicationWindow != newConfig.Server.SlackEventDeduplicationWindow {
		changed = append(changed, "slack_event_deduplication_window")
	}
	if oldConfig.Server.Metrics != newConfig.Server.Metrics {
		changed = append(changed, "metrics")
	}
	if !reflect.DeepEqual(oldConfig.Slack, newConfig.Slack) {
		changed = append(changed, "slack")
	}
	if !reflect.DeepEqual(oldConfig.AI, newConfig.AI) {
		changed = append(changed, "openai")
	}
	return changed
}

// notifySubscribers notifies all subscribers of config changes
func (cm *ConfigManager) notifySubscribers(config *Config) {
	cm.subsMutex.RLock()
	callbacks := append([]func(*Config){}, cm.subscribers...)
	cm.subsMutex.RUnlock()

	// Dispatch synchronously so callbacks observe the same order as config
	// reloads. In particular, a slower callback must not apply an older config
	// after a newer one.
	for _, callback := range callbacks {
		func() {
			defer func() {
				if r := recover(); r != nil {
					cm.log.Error("Config subscriber callback panicked",
						zap.Any("panic", r))
				}
			}()
			callback(config)
		}()
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

func (cm *ConfigManager) GetShowerthoughtConfig() showerthought.Config {
	config := cm.GetConfig()
	if config == nil {
		return showerthought.Config{}
	}
	return config.ShowerThought
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
	if cmd.IsSet("slack-event-deduplication-window") {
		val := cmd.Duration("slack-event-deduplication-window")
		overrides.SlackEventDeduplicationWindow = &val
	}
	if cmd.IsSet("metrics-enabled") {
		val := cmd.Bool("metrics-enabled")
		overrides.MetricsEnabled = &val
	}
	if cmd.IsSet("metrics-path") {
		val := cmd.String("metrics-path")
		overrides.MetricsPath = &val
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
	if cmd.IsSet("openai-model") {
		val := cmd.String("openai-model")
		overrides.OpenAIModel = &val
	}
	if cmd.IsSet("openai-reasoning-effort") {
		val := cmd.String("openai-reasoning-effort")
		overrides.OpenAIReasoningEffort = &val
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
	if cmd.IsSet("aichat-rate-limit-enabled") {
		val := cmd.Bool("aichat-rate-limit-enabled")
		overrides.AIChatRateLimitEnabled = &val
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

func stringWithFileAndOverride(fileValue *string, defaultValue string, override *string) string {
	if override != nil {
		return *override
	}
	if fileValue != nil {
		return *fileValue
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

func boolWithFileAndOverride(fileValue *bool, defaultValue bool, override *bool) bool {
	if override != nil {
		return *override
	}
	if fileValue != nil {
		return *fileValue
	}
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
