package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

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
			Validator: func(v string) error {
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
			Validator: func(v string) error {
				if v == "" {
					return nil
				}
				if IsEnvironment(v) {
					return nil
				}
				return cli.Exit(fmt.Errorf("'env' must be %v. Received: %v", strings.Join(Environments, ", "), v), 2)
			},
		},
		&cli.StringFlag{
			Name:    "data-dir",
			Usage:   "Data storage directory, may be relative or absolute",
			Value:   "./",
			Sources: cli.EnvVars("DATA_DIR"),
			Validator: func(v string) error {
				if err := validateDirectoryInput(v, 0755); err != nil {
					return cli.Exit(fmt.Errorf("invalid data directory: %v", err), 2)
				}
				return nil
			},
		},
		&cli.Uint32Flag{
			Name:    "server-port",
			Usage:   "Server port",
			Value:   http.DefaultServerPort,
			Sources: cli.EnvVars("SERVER_PORT"),
		},
		&cli.StringFlag{
			Name:    "config-file",
			Aliases: []string{"config", "c"},
			Usage:   "Path to yaml or json file of chat responses definition.",
			Value:   "../cmd/bot/config.yaml",
			Sources: cli.EnvVars("CONFIG_FILE"),
			Validator: func(v string) error {
				if v == "" {
					return nil
				}
				if err := validateFileInput(v); err != nil {
					return cli.Exit(fmt.Errorf("invalid config file '%s': %w", v, err), 2)
				}
				return nil
			},
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
			Name:  "slack-user-notify-channel",
			Usage: "Channel name to notify when a user is added or removed from the Slack organization.",
			Sources: cli.NewValueSourceChain(
				cli.EnvVar("SLACK_OBITUARY_NOTIFY_CHANNEL"),
				cli.EnvVar("SLACK_USER_NOTIFY_CHANNEL"),
				yaml.YAML("user.notify_channel", altsrc.NewStringPtrSourcer(&configFile)),
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
		&cli.StringFlag{
			Name:     "slack-token",
			Usage:    "Slack Client Secret for OAuth authentication.",
			Required: true,
			Sources: cli.NewValueSourceChain(
				cli.EnvVar("SLACK_TOKEN"),
				cli.File("/run/secrets/slack_token"),
			),
		},
		&cli.StringFlag{
			Name:  "slack-signing-secret",
			Usage: "Slack Signing Secret for verifying events.",
			Sources: cli.NewValueSourceChain(
				cli.EnvVar("SLACK_SIGNING_SECRET"),
				cli.File("/run/secrets/slack_signing_secret"),
			),
		},
		&cli.StringFlag{
			Name:  "openai-api-key",
			Usage: "OpenAPI API key for AI conversations.",
			Sources: cli.NewValueSourceChain(
				cli.EnvVar("OPENAI_API_KEY"),
				cli.File("/run/secrets/openai_api_key"),
			),
		},
		&cli.StringFlag{
			Name:  "personas-config",
			Usage: "JSON or YAML string defining AI Chat personas as name:prompt pairs.",
			Sources: cli.NewValueSourceChain(
				cli.EnvVar("AI_PERSONAS_CONFIG"),
				yaml.YAML("personas", altsrc.NewStringPtrSourcer(&configFile)),
			),
		},
		&cli.DurationFlag{
			Name:  "vibecheck-ban-duration",
			Usage: "Duration to ban users for when they fail a vibecheck.",
			Value: 5 * time.Minute,
			Sources: cli.NewValueSourceChain(
				cli.EnvVar("VIBECHECK_BAN_DURATION"),
				yaml.YAML("vibecheck.ban_duration", altsrc.NewStringPtrSourcer(&configFile)),
			),
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
