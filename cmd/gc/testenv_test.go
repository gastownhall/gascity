package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gcEnvVars lists the GC_* identity and session-routing variables that
// tests should clear to isolate from host session state (e.g., running
// inside a gc-managed tmux session).
var gcEnvVars = []string{
	"GC_ALIAS",
	"GC_AGENT",
	"GC_SESSION_ID",
	"GC_SESSION_NAME",
	"GC_SHARED_SKILL_CATALOG_SNAPSHOT",
	"GC_TMUX_SESSION",
	"GC_CITY",
}

// clearGCEnv clears GC_* identity and session-routing variables for the
// duration of the test, preventing host session state from leaking into
// tests. Uses t.Setenv so values are automatically restored.
func clearGCEnv(t *testing.T) {
	t.Helper()
	for _, k := range gcEnvVars {
		t.Setenv(k, "")
	}
}

var testProviderStubCommands = []string{
	"claude",
	"codex",
	"gemini",
	"cursor",
	"copilot",
	"amp",
	"opencode",
	"auggie",
	"pi",
	"omp",
}

func installTestProviderStubs() (string, error) {
	dir, err := os.MkdirTemp("", "gascity-provider-stubs-*")
	if err != nil {
		return "", err
	}
	for _, name := range testProviderStubCommands {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			_ = os.RemoveAll(dir)
			return "", err
		}
	}
	return dir, nil
}

func writeTestGitIdentity(homeDir string) error {
	gitConfig := filepath.Join(homeDir, ".gitconfig")
	data := []byte("[user]\n\tname = gc-test\n\temail = gc-test@test.local\n[beads]\n\trole = maintainer\n")
	return os.WriteFile(gitConfig, data, 0o644)
}

// gcBeadsBdTestHomeEnv creates a temp HOME with a .gitconfig containing user
// identity and beads.role = maintainer, then returns extra env entries suitable
// for appending to sanitizedBaseEnv. Use this for any test that runs the real
// gc-beads-bd.sh op_init, which calls ensure_beads_role and requires a writable
// global git config.
func gcBeadsBdTestHomeEnv(t *testing.T) []string {
	t.Helper()
	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(beads-bd test home): %v", err)
	}
	if err := writeTestGitIdentity(homeDir); err != nil {
		t.Fatalf("write test git identity for beads-bd: %v", err)
	}
	return []string{
		"HOME=" + homeDir,
		"GIT_CONFIG_GLOBAL=" + filepath.Join(homeDir, ".gitconfig"),
	}
}

func writeTestDoltIdentity(homeDir string) error {
	doltDir := filepath.Join(homeDir, ".dolt")
	if err := os.MkdirAll(doltDir, 0o755); err != nil {
		return err
	}
	data := []byte(`{"user.name":"gc-test","user.email":"gc-test@test.local"}`)
	return os.WriteFile(filepath.Join(doltDir, "config_global.json"), data, 0o644)
}

func configureTestDoltIdentityEnv(t *testing.T) {
	t.Helper()

	homeDir := filepath.Join(t.TempDir(), "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(test home): %v", err)
	}
	if err := writeTestGitIdentity(homeDir); err != nil {
		t.Fatalf("write test git identity: %v", err)
	}
	if err := writeTestDoltIdentity(homeDir); err != nil {
		t.Fatalf("write test dolt identity: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(homeDir, ".gitconfig"))
	t.Setenv("DOLT_ROOT_PATH", homeDir)
}

func configureRealBdAndDoltPath(t *testing.T) {
	t.Helper()

	bdPath := waitTestRealBDPath(t)
	doltPath, err := exec.LookPath("dolt")
	if err != nil {
		t.Skip("dolt not installed")
	}
	t.Setenv("PATH", strings.Join([]string{
		filepath.Dir(bdPath),
		filepath.Dir(doltPath),
		os.Getenv("PATH"),
	}, string(os.PathListSeparator)))
}
