package config

import (
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/urfave/cli/v3"
)

func TestExtractCLIOverrides_EnvironmentVariables(t *testing.T) {
	// Test that environment variables are properly captured even when not set via CLI
	tests := []struct {
		name    string
		envKey  string
		envVal  string
		flagVal string
		verify  func(*CLIOverrides) error
	}{
		{
			name:   "openai api key from environment",
			envKey: "OPENAI_API_KEY",
			envVal: "sk-test-key-from-env",
			verify: func(overrides *CLIOverrides) error {
				if overrides.OpenAIAPIKey == nil {
					t.Error("OpenAIAPIKey should not be nil when environment variable is set")
					return nil
				}
				if *overrides.OpenAIAPIKey != "sk-test-key-from-env" {
					t.Errorf("OpenAIAPIKey = %v, want %v", *overrides.OpenAIAPIKey, "sk-test-key-from-env")
				}
				return nil
			},
		},
		{
			name:   "slack token from environment",
			envKey: "SLACK_TOKEN",
			envVal: "xoxb-test-token-from-env",
			verify: func(overrides *CLIOverrides) error {
				if overrides.SlackToken == nil {
					t.Error("SlackToken should not be nil when environment variable is set")
					return nil
				}
				if *overrides.SlackToken != "xoxb-test-token-from-env" {
					t.Errorf("SlackToken = %v, want %v", *overrides.SlackToken, "xoxb-test-token-from-env")
				}
				return nil
			},
		},
		{
			name:   "slack signing secret from environment",
			envKey: "SLACK_SIGNING_SECRET",
			envVal: "test-signing-secret-from-env",
			verify: func(overrides *CLIOverrides) error {
				if overrides.SlackSigningSecret == nil {
					t.Error("SlackSigningSecret should not be nil when environment variable is set")
					return nil
				}
				if *overrides.SlackSigningSecret != "test-signing-secret-from-env" {
					t.Errorf("SlackSigningSecret = %v, want %v", *overrides.SlackSigningSecret, "test-signing-secret-from-env")
				}
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Set up environment variable
			oldVal := os.Getenv(tt.envKey)
			defer func() {
				if oldVal == "" {
					require.NoError(t, os.Unsetenv(tt.envKey))
				} else {
					require.NoError(t, os.Setenv(tt.envKey, oldVal))
				}
			}()
			require.NoError(t, os.Setenv(tt.envKey, tt.envVal))

			// Create CLI command with environment variable sources
			cmd := &cli.Command{
				Name: "test",
				Flags: []cli.Flag{
					&cli.StringFlag{
						Name: "openai-api-key",
						Sources: cli.NewValueSourceChain(
							cli.EnvVar("OPENAI_API_KEY"),
							cli.File("/run/secrets/openai_api_key"),
						),
					},
					&cli.StringFlag{
						Name: "slack-token",
						Sources: cli.NewValueSourceChain(
							cli.EnvVar("SLACK_TOKEN"),
							cli.File("/run/secrets/slack_token"),
						),
					},
					&cli.StringFlag{
						Name: "slack-signing-secret",
						Sources: cli.NewValueSourceChain(
							cli.EnvVar("SLACK_SIGNING_SECRET"),
							cli.File("/run/secrets/slack_signing_secret"),
						),
					},
				},
			}

			// Parse the command to process environment variables
			err := cmd.Run(context.Background(), []string{"test"})
			if err != nil {
				t.Logf("Expected command run error (this is normal for test setup): %v", err)
			}

			// Extract CLI overrides
			overrides := ExtractCLIOverrides(cmd)

			// Verify the result
			if err := tt.verify(overrides); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestExtractCLIOverrides_EmptyEnvironment(t *testing.T) {
	// Test that empty environment variables don't create overrides
	envKeys := []string{"OPENAI_API_KEY", "SLACK_TOKEN", "SLACK_SIGNING_SECRET"}

	// Save and clear environment variables
	oldValues := make(map[string]string)
	for _, key := range envKeys {
		oldValues[key] = os.Getenv(key)
		require.NoError(t, os.Unsetenv(key))
	}
	defer func() {
		for key, val := range oldValues {
			if val != "" {
				require.NoError(t, os.Setenv(key, val))
			}
		}
	}()

	cmd := &cli.Command{
		Name: "test",
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name: "openai-api-key",
				Sources: cli.NewValueSourceChain(
					cli.EnvVar("OPENAI_API_KEY"),
				),
			},
			&cli.StringFlag{
				Name: "slack-token",
				Sources: cli.NewValueSourceChain(
					cli.EnvVar("SLACK_TOKEN"),
				),
			},
			&cli.StringFlag{
				Name: "slack-signing-secret",
				Sources: cli.NewValueSourceChain(
					cli.EnvVar("SLACK_SIGNING_SECRET"),
				),
			},
		},
	}

	// Parse the command
	err := cmd.Run(context.Background(), []string{"test"})
	if err != nil {
		t.Logf("Expected command run error (this is normal for test setup): %v", err)
	}

	// Extract CLI overrides
	overrides := ExtractCLIOverrides(cmd)

	// Verify that no overrides are set when environment variables are empty
	if overrides.OpenAIAPIKey != nil {
		t.Error("OpenAIAPIKey should be nil when environment variable is empty")
	}
	if overrides.SlackToken != nil {
		t.Error("SlackToken should be nil when environment variable is empty")
	}
	if overrides.SlackSigningSecret != nil {
		t.Error("SlackSigningSecret should be nil when environment variable is empty")
	}
}
