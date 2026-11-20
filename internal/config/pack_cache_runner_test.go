package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunRepoCacheGitPutsConfigArgsBeforeCommand pins the exported runner's
// argv contract: the caller's `-c` flags ride between `git` and the
// subcommand, and stdout comes back with trailing newlines trimmed only.
func TestRunRepoCacheGitPutsConfigArgsBeforeCommand(t *testing.T) {
	binDir := t.TempDir()
	script := `#!/bin/sh
for arg in "$@"; do
	printf '%s\n' "$arg"
done
`
	if err := os.WriteFile(filepath.Join(binDir, "git"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	out, err := RunRepoCacheGit(t.TempDir(), []string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.untrackedCache=false",
	}, "status", "--porcelain")
	if err != nil {
		t.Fatalf("RunRepoCacheGit: %v", err)
	}
	want := strings.Join([]string{
		"-c", "core.fsmonitor=false",
		"-c", "core.hooksPath=/dev/null",
		"-c", "core.untrackedCache=false",
		"status", "--porcelain",
	}, "\n")
	if out != want {
		t.Fatalf("RunRepoCacheGit output = %q, want %q", out, want)
	}
}
