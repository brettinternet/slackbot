package main

import "testing"

func TestIsRuntimeArtifact(t *testing.T) {
	t.Parallel()

	tests := map[string]bool{
		"bin/bot":                         true,
		"cmd/bot/bot":                     true,
		"cmd/bot/build/main":              true,
		"cmd/bot/tmp/users.json":          true,
		"cmd/worker/data/state.json":      true,
		"docker/bot/data/context.db":      true,
		"tmp/aichat_context.db":           true,
		"tmp/aichat_context.db-shm":       true,
		"tmp/aichat_context.db-wal":       true,
		"tmp/users.json":                  true,
		"coverage.out":                    true,
		"integration.test":                true,
		"slackbot.exe":                    true,
		"bot/aichat/context.go":           false,
		"bot/aichat/migrations_test.go":   false,
		"docker/bot/prod.Dockerfile":      false,
		"tools/random/testdata/sample.db": true,
	}

	for path, want := range tests {
		path, want := path, want
		t.Run(path, func(t *testing.T) {
			t.Parallel()
			if got := isRuntimeArtifact(path); got != want {
				t.Fatalf("isRuntimeArtifact(%q) = %t, want %t", path, got, want)
			}
		})
	}
}
