package config

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"

	altsrc "github.com/urfave/cli-altsrc/v3"
	yaml "github.com/urfave/cli-altsrc/v3/yaml"
	"github.com/urfave/cli/v3"
	"slackbot.arpa/bot/http"
)

func Flags() []cli.Flag {
	var configFile string
	return []cli.Flag{
		&cli.StringFlag{
			Name:    "log-level",
			Usage:   "log level",
			Value:   "info",
			Sources: cli.EnvVars("LOG_LEVEL"),
			Action: func(ctx context.Context, cmd *cli.Command, v string) error {
				options := []string{"error", "warn", "info", "debug"}
				if slices.Contains(options, strings.ToLower(v)) {
					return nil
				}
				return cli.Exit(fmt.Errorf("'log-level' must be %v. Received: %v", strings.Join(options, ", "), v), 2)
			},
		},
		&cli.StringFlag{
			Name:    "env",
			Usage:   "build environment description",
			Sources: cli.EnvVars("ENVIRONMENT"),
			Action: func(ctx context.Context, cmd *cli.Command, v string) error {
				if v == "" {
					return nil
				}
				if IsEnvironment(v) {
					return nil
				}
				return cli.Exit(fmt.Errorf("'env' must be %v. Received: %v", strings.Join(EnvironmentOptions, ", "), v), 2)
			},
		},
		&cli.StringFlag{
			Name:    "data-dir",
			Usage:   "Data storage directory, may be relative or absolute",
			Value:   "./",
			Sources: cli.EnvVars("DATA_DIR"),
			Action: func(ctx context.Context, cmd *cli.Command, v string) error {
				if err := validateDirectoryInput(v, 0755); err != nil {
					return cli.Exit(fmt.Errorf("invalid data directory: %v", err), 2)
				}
				return nil
			},
		},
		&cli.StringSliceFlag{
			Name:    "features",
			Sources: cli.EnvVars("FEATURES"),
			Action: func(ctx context.Context, cmd *cli.Command, values []string) error {
				for _, v := range values {
					if !IsFeature(v) {
						return cli.Exit(fmt.Errorf("invalid feature option: %s", v), 2)
					}
				}
				return nil
			},
		},
		&cli.StringFlag{
			Name:    "server-host",
			Usage:   "Server host",
			Value:   "localhost",
			Sources: cli.EnvVars("SERVER_HOST"),
		},
		&cli.Uint32Flag{
			Name:    "server-port",
			Usage:   "Server port",
			Value:   http.DefaultServerPort,
			Sources: cli.EnvVars("SERVER_PORT"),
		},
		&cli.StringFlag{
			Name:        "config-file",
			Aliases:     []string{"config", "c"},
			Usage:       "Path to yaml or json file of chat responses definition.",
			Value:       "./config.yaml",
			Sources:     cli.EnvVars("SLACK_CONFIG_FILE"),
			Destination: &configFile,
		},
		&cli.StringSliceFlag{
			Name:  "slack-preferred-users",
			Usage: "Preference toward users.",
			Sources: cli.NewValueSourceChain(
				cli.EnvVar("SLACK_PREFERRED_USERS"),
				yaml.YAML("preferred_users", altsrc.NewStringPtrSourcer(&configFile)),
			),
		},
		&cli.StringSliceFlag{
			Name:  "slack-preferred-channels",
			Usage: "Channels to automatically join.",
			Sources: cli.NewValueSourceChain(
				cli.EnvVar("SLACK_PREFERRED_CHANNELS"),
				yaml.YAML("preferred_channels", altsrc.NewStringPtrSourcer(&configFile)),
			),
		},
		&cli.StringFlag{
			Name:  "slack-obituary-notify-channel",
			Usage: "Channel name to notify when a user is deleted from the Slack organization.",
			Sources: cli.NewValueSourceChain(
				cli.EnvVar("SLACK_OBITUARY_NOTIFY_CHANNEL"),
				yaml.YAML("obituary.notify_channel", altsrc.NewStringPtrSourcer(&configFile)),
			),
		},
		&cli.StringFlag{
			Name:  "slack-events-path",
			Usage: "HTTP path for the Slack Events API endpoint.",
			Value: "/api/slack/events",
			Sources: cli.NewValueSourceChain(
				cli.EnvVar("SLACK_EVENTS_PATH"),
				yaml.YAML("slack_events_path", altsrc.NewStringPtrSourcer(&configFile)),
			),
		},
	}
}

func MutuallyExclusiveFlags() []cli.MutuallyExclusiveFlags {
	return []cli.MutuallyExclusiveFlags{
		{
			Required: true,
			Flags: [][]cli.Flag{
				{
					&cli.StringFlag{
						Name:    "slack-token",
						Usage:   "Slack Client Secret for OAuth authentication.",
						Sources: cli.EnvVars("SLACK_TOKEN"),
					},
				},
				{
					&cli.StringFlag{
						Name:    "slack-token-file",
						Usage:   "Path to Slack Client Secret for OAuth authentication.",
						Sources: cli.EnvVars("SLACK_TOKEN_FILE"),
						Action: func(ctx context.Context, cmd *cli.Command, v string) error {
							if err := validateFileInput(v); err != nil {
								return cli.Exit(fmt.Errorf("invalid slack token file: %v", err), 2)
							}
							return nil
						},
					},
				},
			},
		},
		{
			Required: false,
			Flags: [][]cli.Flag{
				{
					&cli.StringFlag{
						Name:    "slack-signing-secret",
						Usage:   "Slack Signing Secret for verifying events.",
						Sources: cli.EnvVars("SLACK_SIGNING_SECRET"),
					},
				},
				{
					&cli.StringFlag{
						Name:    "slack-signing-secret-file",
						Usage:   "Path to Slack Signing Secret file for verifying events.",
						Sources: cli.EnvVars("SLACK_SIGNING_SECRET_FILE"),
						Action: func(ctx context.Context, cmd *cli.Command, v string) error {
							if err := validateFileInput(v); err != nil {
								return cli.Exit(fmt.Errorf("invalid slack signing secret file: %v", err), 2)
							}
							return nil
						},
					},
				},
			},
		},
	}
}

// Ensures the directory input is valid.
//
// The directory must either exist or the parent directory must exist.
// Will create if the directory doesn't exist.
func validateDirectoryInput(dir string, permissions os.FileMode) error {
	if dir == "" {
		return errors.New("directory is required")
	} else {
		parent := filepath.Dir(dir)
		_, err := os.Stat(parent)
		if err != nil {
			return err
		}
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			err := os.MkdirAll(dir, permissions)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// Ensures the file input is valid.
func validateFileInput(file string) error {
	if file == "" {
		return errors.New("file is required")
	} else {
		_, err := os.Stat(file)
		if err != nil {
			return err
		}
	}
	return nil
}

func validateURLInput(input string) error {
	if input == "" {
		return errors.New("URL is required")
	} else {
		u, err := url.ParseRequestURI(input)
		if err != nil {
			return fmt.Errorf("invalid url '%v': %v", input, err)
		}
		host, _, err := net.SplitHostPort(u.Host)
		if err != nil || host == "" {
			return fmt.Errorf("invalid url '%v': %v", input, err)
		}
		return nil
	}
}
