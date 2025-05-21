package config

import (
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"github.com/urfave/cli/v3"
	"slackbot.arpa/bot/chat"
	"slackbot.arpa/bot/http"
	"slackbot.arpa/bot/obituary"
	"slackbot.arpa/bot/slack"
	"slackbot.arpa/bot/vibecheck"
)

type Feature string
type Environment string

const (
	FeatureObituary  Feature = "obituary"
	FeatureChat      Feature = "chat"
	FeatureVibecheck Feature = "vibecheck"

	EnvironmentDevelopment Environment = "development"
	EnvironmentProduction  Environment = "production"
)

var EnvironmentOptions = []string{EnvironmentDevelopment.String(), EnvironmentProduction.String()}

func IsEnvironment(v string) bool {
	return slices.Contains(EnvironmentOptions, strings.ToLower(v))
}

func (f Feature) String() string {
	return string(f)
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

var (
	Features = []Feature{FeatureObituary, FeatureChat, FeatureVibecheck}
)

func IsFeature(f string) bool {
	return slices.Contains(Features, Feature(f))
}

func HasFeature(features []Feature, f Feature) bool {
	return slices.Contains(features, f)
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
		Version:                l.BuildVersion,
		BuildTime:              l.BuildTime,
		LogLevel:               cmd.String("log-level"),
		Environment:            environment,
		DataDir:                cmd.String("data-dir"),
		Features:               cmd.StringSlice("features"),
		ServerHost:             cmd.String("server-host"),
		ServerPort:             cmd.Uint32("server-port"),
		SlackToken:             cmd.String("slack-token"),
		SlackTokenFile:         cmd.String("slack-token-file"),
		SlackSigningSecret:     cmd.String("slack-signing-secret"),
		SlackSigningSecretFile: cmd.String("slack-signing-secret-file"),
		PreferredUsers:         cmd.StringSlice("slack-preferred-users"),
		PreferredChannels:      cmd.StringSlice("slack-preferred-channels"),
		ObituaryNotifyChannel:  cmd.String("slack-obituary-notify-channel"),
		SlackEventsPath:        cmd.String("slack-events-path"),
		ConfigFile:             cmd.String("config-file"),
	}

	return newConfig(opts)
}

type configOpts struct {
	Version                string
	BuildTime              string
	LogLevel               string
	Environment            string
	DataDir                string
	Features               []string
	ServerHost             string
	ServerPort             uint32
	SlackToken             string
	SlackTokenFile         string
	SlackSigningSecret     string
	SlackSigningSecretFile string
	PreferredUsers         []string
	PreferredChannels      []string
	ObituaryNotifyChannel  string
	SlackEventsPath        string
	ConfigFile             string
}

type Config struct {
	Version     string
	BuildTime   string
	LogLevel    string
	Environment Environment
	DataDir     string
	ConfigFile  string
	Features    []Feature // Feature flags
	Server      http.Config
	Slack       slack.Config
	Obituary    obituary.Config
	Chat        chat.Config
	Vibecheck   vibecheck.Config
}

func newConfig(opts configOpts) (Config, error) {
	var features []Feature
	for _, f := range opts.Features {
		if IsFeature(f) {
			features = append(features, Feature(f))
		}
	}

	dataDir := opts.DataDir
	if dataDir == "" {
		dataDir = "./tmp"
	} else {
		dataDir, _ = relativeToAbsolutePath(dataDir)
	}

	slackToken := opts.SlackToken
	if opts.SlackTokenFile != "" {
		tokenBytes, err := os.ReadFile(opts.SlackTokenFile)
		if err == nil {
			slackToken = string(tokenBytes)
		}
	}

	slackSigningSecret := opts.SlackSigningSecret
	if opts.SlackSigningSecretFile != "" {
		secretBytes, err := os.ReadFile(opts.SlackSigningSecretFile)
		if err == nil {
			slackSigningSecret = string(secretBytes)
		}
	}

	return Config{
		Version:     opts.Version,
		BuildTime:   opts.BuildTime,
		LogLevel:    opts.LogLevel,
		Environment: environmentFromString(opts.Environment),
		DataDir:     dataDir,
		Features:    features,
		ConfigFile:  opts.ConfigFile,
		Server: http.Config{
			ServerHost:     opts.ServerHost,
			ServerPort:     opts.ServerPort,
			SlackEventPath: opts.SlackEventsPath,
		},
		Slack: slack.Config{
			Token:             slackToken,
			SigningSecret:     slackSigningSecret,
			Debug:             false,
			PreferredChannels: opts.PreferredChannels,
		},
		Obituary: obituary.Config{
			NotifyChannel: opts.ObituaryNotifyChannel,
			DataDir:       dataDir,
		},
		Chat: chat.Config{
			PreferredUsers: opts.PreferredUsers,
		},
		Vibecheck: vibecheck.Config{
			PreferredUsers: opts.PreferredUsers,
			DataDir:        dataDir,
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
	filepath.Abs(input)
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
