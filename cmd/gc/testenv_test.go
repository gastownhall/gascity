package main

import (
	"os"
	"path/filepath"
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
