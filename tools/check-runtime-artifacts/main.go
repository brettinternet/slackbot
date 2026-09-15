// Command check-runtime-artifacts fails when generated runtime artifacts are tracked by Git.
package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

func main() {
	output, err := exec.Command("git", "ls-files", "-z").Output()
	if err != nil {
		fmt.Fprintf(os.Stderr, "list tracked files: %v\n", err)
		os.Exit(1)
	}

	var artifacts []string
	for _, path := range bytes.Split(output, []byte{0}) {
		if len(path) > 0 && isRuntimeArtifact(string(path)) {
			artifacts = append(artifacts, string(path))
		}
	}
	if len(artifacts) == 0 {
		return
	}

	sort.Strings(artifacts)
	fmt.Fprintln(os.Stderr, "generated runtime artifacts must not be committed:")
	for _, path := range artifacts {
		fmt.Fprintf(os.Stderr, "  %s\n", path)
	}
	os.Exit(1)
}

func isRuntimeArtifact(path string) bool {
	if path == "coverage.out" || path == "slackbot" || path == "bin/bot" || path == "cmd/bot/bot" ||
		strings.HasPrefix(path, "tmp/") {
		return true
	}

	if hasPathSegment(path, "build", "cmd") || hasPathSegment(path, "tmp", "cmd") ||
		hasPathSegment(path, "data", "cmd") || hasPathSegment(path, "data", "docker") {
		return true
	}

	for _, suffix := range []string{".db", ".db-shm", ".db-wal", ".test", ".exe"} {
		if strings.HasSuffix(path, suffix) {
			return true
		}
	}
	return false
}

func hasPathSegment(path, segment, root string) bool {
	parts := strings.Split(path, "/")
	if len(parts) < 3 || parts[0] != root {
		return false
	}
	for _, part := range parts[2:] {
		if part == segment {
			return true
		}
	}
	return false
}
