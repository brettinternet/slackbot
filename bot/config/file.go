// Package config provides configuration management for the bot
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"maps"

	"github.com/fsnotify/fsnotify"
	"github.com/goccy/go-yaml"
	"go.uber.org/zap"
	"slackbot.arpa/bot/aichat"
	"slackbot.arpa/bot/chat"
	"slackbot.arpa/bot/user"
	"slackbot.arpa/bot/vibecheck"
)

// FileConfig represents the entire configuration file structure
type FileConfig struct {
	User      user.FileConfig      `json:"user" yaml:"user"`
	Chat      chat.FileConfig      `json:"chat" yaml:"chat"`
	Vibecheck vibecheck.FileConfig `json:"vibecheck" yaml:"vibecheck"`
	AIChat    aichat.FileConfig    `json:"aichat" yaml:"aichat"`
}

// ConfigWatcher watches a configuration file for changes and parses its content
type ConfigWatcher struct {
	log            *zap.Logger
	filePath       string
	lastModTime    time.Time
	watcher        *fsnotify.Watcher
	pollingTicker  *time.Ticker
	stopCh         chan struct{}
	callbacks      map[string]func(FileConfig)
	mu             sync.RWMutex
	config         FileConfig // Current parsed configuration
	isConfigLoaded atomic.Bool
}

// NewConfigWatcher creates a new configuration file watcher
func NewConfigWatcher(log *zap.Logger, filePath string) (*ConfigWatcher, error) {
	absPath, err := filepath.Abs(filePath)
	if err != nil {
		return nil, fmt.Errorf("get absolute path: %w", err)
	}

	// Get initial file stats
	fileInfo, err := os.Stat(absPath)
	if err != nil {
		return nil, fmt.Errorf("stat config file: %w", err)
	}

	// Initialize file watcher
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("create file watcher: %w", err)
	}

	return &ConfigWatcher{
		log:         log,
		filePath:    absPath,
		lastModTime: fileInfo.ModTime(),
		watcher:     watcher,
		stopCh:      make(chan struct{}),
		callbacks:   make(map[string]func(FileConfig)),
	}, nil
}

// Start begins watching the configuration file for changes
func (w *ConfigWatcher) Start(ctx context.Context) error {
	// Load initial configuration
	if err := w.loadConfig(); err != nil {
		return fmt.Errorf("load initial config: %w", err)
	}

	// Add file to watch
	if err := w.watcher.Add(w.filePath); err != nil {
		w.log.Warn("Could not watch config file, falling back to polling only",
			zap.String("file", w.filePath),
			zap.Error(err),
		)
	}

	// Create polling ticker (check every 30 seconds) as fallback mechanism
	w.pollingTicker = time.NewTicker(30 * time.Second)

	// Start watching for file changes
	go w.watchConfigFile(ctx)
	w.log.Debug("Config watcher started successfully.",
		zap.String("config_file", w.filePath),
	)

	w.notifyCallbacks() // Notify all callbacks with the initial configuration
	return nil
}

// Stop stops the watcher
func (w *ConfigWatcher) Stop() {
	close(w.stopCh)
	if w.pollingTicker != nil {
		w.pollingTicker.Stop()
	}
	_ = w.watcher.Close()
}

// AddCallback registers a function to be called when the config file changes
func (w *ConfigWatcher) AddCallback(name string, callback func(FileConfig)) {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.callbacks[name] = callback

	// If we already have a configuration loaded, call the callback immediately
	if w.isConfigLoaded.Load() {
		callback(w.config)
	}
}

// GetConfig returns the current parsed configuration
func (w *ConfigWatcher) GetConfig() FileConfig {
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.config
}

// loadConfig reads and parses the configuration file
func (w *ConfigWatcher) loadConfig() error {
	var config FileConfig
	if err := ReadConfig(w.filePath, &config); err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	w.mu.Lock()
	w.config = config
	w.isConfigLoaded.Store(true)
	w.mu.Unlock()

	return nil
}

// watchConfigFile monitors the config file for changes
func (w *ConfigWatcher) watchConfigFile(ctx context.Context) {
	for {
		select {
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		case <-w.pollingTicker.C:
			// Regular polling fallback
			w.checkFileModification()
		case event, ok := <-w.watcher.Events:
			if !ok {
				return
			}

			// Check if this is a write or create event
			if event.Op&(fsnotify.Write|fsnotify.Create) != 0 {
				w.checkFileModification()
			}
		case err, ok := <-w.watcher.Errors:
			if !ok {
				return
			}
			w.log.Error("File watcher error", zap.Error(err))
		}
	}
}

// checkFileModification checks if the file has been modified and notifies callbacks
func (w *ConfigWatcher) checkFileModification() {
	// Get file info to check modification time
	fileInfo, err := os.Stat(w.filePath)
	if err != nil {
		w.log.Error("Failed to stat config file",
			zap.String("file", w.filePath),
			zap.Error(err),
		)
		return
	}

	// Only notify if the file was actually modified
	if fileInfo.ModTime().After(w.lastModTime) {
		w.lastModTime = fileInfo.ModTime()
		w.log.Info("Config file changed, reloading configuration",
			zap.String("file", w.filePath),
		)

		if err := w.loadConfig(); err != nil {
			w.log.Error("Failed to reload config", zap.Error(err))
			return
		}

		w.notifyCallbacks()
	}
}

// notifyCallbacks calls all registered callbacks with the current configuration
func (w *ConfigWatcher) notifyCallbacks() {
	w.mu.RLock()
	config := w.config
	callbacks := make(map[string]func(FileConfig))
	maps.Copy(callbacks, w.callbacks)
	w.mu.RUnlock()

	for name, callback := range callbacks {
		w.log.Debug("Notifying config change callback", zap.String("callback", name))
		callback(config)
	}
}

// ReadConfig reads and parses a config file into the provided struct
func ReadConfig(filePath string, v any) error {
	ext := filepath.Ext(filePath)

	content, err := os.ReadFile(filePath) // #nosec G304 -- filePath is controlled by configuration
	if err != nil {
		return fmt.Errorf("read config file: %w", err)
	}

	switch ext {
	case ".json":
		if err := json.Unmarshal(content, v); err != nil {
			return fmt.Errorf("unmarshal json: %w", err)
		}
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(content, v); err != nil {
			return fmt.Errorf("unmarshal yaml: %w", err)
		}
	default:
		return fmt.Errorf("unsupported config file format: %s", ext)
	}

	return nil
}
