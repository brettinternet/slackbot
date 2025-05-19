package config

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/urfave/cli/v3"
)

func Flags() []cli.Flag {
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
			Value:   "development",
			Sources: cli.EnvVars("ENVIRONMENT"),
			Action: func(ctx context.Context, cmd *cli.Command, v string) error {
				options := []string{EnvironmentDevelopment.String(), EnvironmentProduction.String()}
				if slices.Contains(options, strings.ToLower(v)) {
					return nil
				}
				return cli.Exit(fmt.Errorf("'env' must be %v. Received: %v", strings.Join(options, ", "), v), 2)
			},
		},
		&cli.StringFlag{
			Name:     "slack-api-key",
			Usage:    "Slack API key.",
			Sources:  cli.EnvVars("SLACK_API_KEY"),
			Required: true,
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
