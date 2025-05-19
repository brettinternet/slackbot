package config

import (
	"os"
	"path"
	"path/filepath"
	"slices"

	"github.com/urfave/cli/v3"
	"slackbot.arpa/bot/obituary"
)

type Feature string
type Environment string

const (
	FeatureObituary Feature = "obituary"

	EnvironmentDevelopment Environment = "development"
	EnvironmentProduction  Environment = "production"
)

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
	Features = []Feature{FeatureObituary}
)

func IsFeature(f string) bool {
	return slices.Contains(Features, Feature(f))
}

func HasFeature(features []Feature, f Feature) bool {
	return slices.Contains(features, f)
}

// From LDFLAGS
type BuildOpts struct {
	BuildVersion string
	BuildTime    string
}

func (l BuildOpts) MakeConfig(cmd *cli.Command) Config {
	if l.BuildVersion == "" {
		l.BuildVersion = "dev"
	}
	if l.BuildTime == "" {
		l.BuildTime = "unknown"
	}
	opts := configOpts{
		Version:               l.BuildVersion,
		BuildTime:             l.BuildTime,
		LogLevel:              cmd.String("log-level"),
		Environment:           cmd.String("env"),
		DataDir:               cmd.String("data-dir"),
		Features:              cmd.StringSlice("features"),
		SlackAPIKey:           cmd.String("slack-api-key"),
		ObituaryNotifyChannel: cmd.String("obituary-notify-channel"),
	}

	return newConfig(opts)
}

type configOpts struct {
	Version               string
	BuildTime             string
	LogLevel              string
	Environment           string
	DataDir               string
	Features              []string
	SlackAPIKey           string
	ObituaryNotifyChannel string
}

type Config struct {
	Version     string
	BuildTime   string
	LogLevel    string
	Environment Environment
	DataDir     string
	Features    []Feature // Feature flags
	SlackAPIKey string
	Obituary    obituary.Config
}

func newConfig(opts configOpts) Config {
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

	return Config{
		Version:     opts.Version,
		BuildTime:   opts.BuildTime,
		LogLevel:    opts.LogLevel,
		Environment: environmentFromString(opts.Environment),
		Features:    features,
		DataDir:     dataDir,
		SlackAPIKey: opts.SlackAPIKey,
		Obituary: obituary.Config{
			NotifyChannel: opts.ObituaryNotifyChannel,
			DataDir:       dataDir,
		},
	}
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
