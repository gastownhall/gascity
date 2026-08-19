package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInternalEnsureWorktreeCmd(t *testing.T) {
	repo := t.TempDir()
	runGitInitForEnsure(t, repo)
	wt := filepath.Join(t.TempDir(), "slot-1")

	var stdout, stderr bytes.Buffer
	cmd := newInternalEnsureWorktreeCmd(&stdout, &stderr)
	cmd.SetArgs([]string{repo, wt, "claude-1"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("execute: %v\nstderr=%s", err, stderr.String())
	}
	if _, err := os.Stat(filepath.Join(wt, ".git")); err != nil {
		t.Fatalf("expected worktree at %s: %v; stdout=%s", wt, err, stdout.String())
	}
}

func runGitInitForEnsure(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"init"},
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "Test"},
		{"commit", "--allow-empty", "-m", "init"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dir
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %s: %v", strings.Join(args, " "), out, err)
		}
	}
}
