package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/urfave/cli/v3"
	"slackbot.arpa/bot/ai"
	"slackbot.arpa/bot/aichat"
	"slackbot.arpa/bot/chat"
	"slackbot.arpa/bot/http"
	"slackbot.arpa/bot/slack"
	"slackbot.arpa/bot/user"
	"slackbot.arpa/bot/vibecheck"
)

type Environment string

const (
	EnvironmentDevelopment Environment = "development"
	EnvironmentProduction  Environment = "production"
)

var Environments = []string{EnvironmentDevelopment.String(), EnvironmentProduction.String()}

func IsEnvironment(v string) bool {
	return slices.Contains(Environments, strings.ToLower(v))
}

func (e Environment) String() string {
	return string(e)
}

func environmentFromString(s string) Environment {
	switch s {
	case EnvironmentDevelopment.String():
		return EnvironmentDevelopment
	case EnvironmentProduction.String():
		return EnvironmentProduction
	default:
		return ""
	}
}

// From LDFLAGS
type BuildOpts struct {
	BuildVersion     string
	BuildTime        string
	BuildEnvironment string
}

func (l BuildOpts) MakeConfig(cmd *cli.Command) (Config, error) {
	if l.BuildVersion == "" {
		l.BuildVersion = "dev"
	}
	if l.BuildTime == "" {
		l.BuildTime = "unknown"
	}
	environment := cmd.String("env")
	if environment == "" {
		environment = l.BuildEnvironment
	}
	if environment == "" || !IsEnvironment(environment) {
		environment = EnvironmentProduction.String()
	}
	opts := configOpts{
		Version:              l.BuildVersion,
		BuildTime:            l.BuildTime,
		LogLevel:             cmd.String("log-level"),
		Environment:          environment,
		DataDir:              cmd.String("data-dir"),
		ServerPort:           cmd.Uint32("server-port"),
		SlackToken:           cmd.String("slack-token"),
		SlackSigningSecret:   cmd.String("slack-signing-secret"),
		OpenAIAPIKey:         cmd.String("openai-api-key"),
		PreferredUsers:       cmd.StringSlice("slack-preferred-user"),
		PreferredChannels:    cmd.StringSlice("slack-preferred-channels"),
		UserNotifyChannel:    cmd.String("slack-user-notify-channel"),
		SlackEventsPath:        cmd.String("slack-events-path"),
		ConfigFile:             cmd.String("config-file"),
		PersonasConfig:         cmd.String("personas-config"),
		PersonasStickyDuration: cmd.Duration("personas-sticky-duration"),
		VibecheckBanDuration:   cmd.Duration("vibecheck-ban-duration"),
	}

	return newConfig(opts)
}

type configOpts struct {
	Version            string
	BuildTime          string
	LogLevel           string
	Environment        string
	DataDir            string
	ServerPort         uint32
	SlackToken         string
	SlackSigningSecret string
	OpenAIAPIKey       string
	PreferredUsers     []string
	PreferredChannels  []string
	UserNotifyChannel  string
	SlackEventsPath    string
	ConfigFile         string
	// AI Chat Personas Configuration
	PersonasConfig         string
	PersonasStickyDuration time.Duration
	// AI Chat Context Limits
	AIChatMaxContextMessages int
	AIChatMaxContextAge      time.Duration
	AIChatMaxContextTokens   int
	// Vibecheck ban duration
	VibecheckBanDuration time.Duration
	// Chat responses
	ChatResponses []chat.Response
}

type Config struct {
	Version     string
	BuildTime   string
	LogLevel    string
	Environment Environment
	DataDir     string
	ConfigFile  string
	Server      http.Config
	Slack       slack.Config
	User        user.Config
	Chat        chat.Config
	Vibecheck   vibecheck.Config
	AI          ai.Config
	AIChat      aichat.Config
}

func newConfig(opts configOpts) (Config, error) {
	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = "./tmp"
	} else {
		dataDir, _ = relativeToAbsolutePath(dataDir)
	}

	// Parse personas configuration
	personas := make(map[string]string)
	if opts.PersonasConfig != "" {
		// Check if we received a Go map string representation (from CLI YAML parsing)
		if strings.HasPrefix(opts.PersonasConfig, "map[") {
			// The CLI library already parsed the YAML and gave us a string representation
			// This mainly happens during testing. In production, personas are typically
			// provided via environment variables as JSON or YAML strings.
			personas = make(map[string]string)
		} else {
			// Try parsing as YAML first, then JSON
			var personasData map[string]interface{}
			err := yaml.Unmarshal([]byte(opts.PersonasConfig), &personasData)
			if err != nil {
				// If YAML fails, try JSON
				err = json.Unmarshal([]byte(opts.PersonasConfig), &personasData)
				if err != nil {
					return Config{}, fmt.Errorf("failed to parse personas config: %w", err)
				}
			}
			
			// Convert to string map
			for name, prompt := range personasData {
				if promptStr, ok := prompt.(string); ok {
					personas[name] = promptStr
				}
			}
		}
	}

	return Config{
		Version:     opts.Version,
		BuildTime:   opts.BuildTime,
		LogLevel:    opts.LogLevel,
		Environment: environmentFromString(opts.Environment),
		DataDir:     dataDir,
		ConfigFile:  opts.ConfigFile,
		Server: http.Config{
			ServerPort:     opts.ServerPort,
			SlackEventPath: opts.SlackEventsPath,
		},
		Slack: slack.Config{
			Token:             opts.SlackToken,
			SigningSecret:     opts.SlackSigningSecret,
			Debug:             false,
			PreferredChannels: opts.PreferredChannels,
		},
		User: user.Config{
			NotifyChannel: opts.UserNotifyChannel,
			DataDir:       dataDir,
		},
		Chat: chat.Config{
			PreferredUsers: opts.PreferredUsers,
			Responses:      opts.ChatResponses,
		},
		Vibecheck: vibecheck.Config{
			PreferredUsers: opts.PreferredUsers,
			DataDir:        dataDir,
			BanDuration:    opts.VibecheckBanDuration,
		},
		AI: ai.Config{
			OpenAIAPIKey: opts.OpenAIAPIKey,
		},
		AIChat: aichat.Config{
			DataDir:             dataDir,
			Personas:            personas,
			StickyDuration:      opts.PersonasStickyDuration,
			MaxContextMessages:  opts.AIChatMaxContextMessages,
			MaxContextAge:       opts.AIChatMaxContextAge,
			MaxContextTokens:    opts.AIChatMaxContextTokens,
		},
	}, nil
}

// Relative path from the executable directory.
// Returns the input if it's already absolute.
func relativeToAbsolutePath(input string) (string, error) {
	if path.IsAbs(input) {
		return input, nil
	}
	cwd, err := currentExecutableDirectory()
	if err != nil {
		return input, err
	}
	_, _ = filepath.Abs(input)
	return path.Clean(path.Join(cwd, input)), nil
}

// Returns the directory of the current executable.
// Not the same as the CWD, this depends on where the executable is instead.
func currentExecutableDirectory() (string, error) {
	ex, err := os.Executable()
	if err != nil {
		return "", err
	}
	return path.Dir(ex), nil
}

func Default[T comparable](val T, defaultVal T) T {
	var zero T
	if val == zero {
		return defaultVal
	}
	return val
}
