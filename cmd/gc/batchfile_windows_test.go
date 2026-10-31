//go:build windows

package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// batchFileFixture writes a do-nothing batch file and returns its path.
func batchFileFixture(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("@echo off\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestPromptSynthKeepsThePromptFromABatchFileProvider pins that prompt text
// never reaches cmd.exe through a batch-file provider command: cmd.exe would
// run its & and cut it at the first line break.
func TestPromptSynthKeepsThePromptFromABatchFileProvider(t *testing.T) {
	workDir := t.TempDir()
	provider := &config.ResolvedProvider{Command: batchFileFixture(t, "agent.cmd"), PrintArgs: []string{"-p"}}
	_, err := defaultPromptSynthRunner(context.Background(), provider, "line one\na&echo>pwned", workDir)
	if err == nil || !strings.Contains(err.Error(), "batch file") {
		t.Errorf("defaultPromptSynthRunner = %v, want the batch-file provider refused", err)
	}
	if _, statErr := os.Stat(filepath.Join(workDir, "pwned")); statErr == nil {
		t.Error("cmd.exe ran the prompt: pwned exists in the working directory")
	}
}

// TestAgentScriptCommandKeepsDataFromABatchFile pins that an agent-script
// action's values, which can carry bead text, never reach cmd.exe through a
// batch-file bd or gc.
func TestAgentScriptCommandKeepsDataFromABatchFile(t *testing.T) {
	dir := t.TempDir()
	bd := batchFileFixture(t, "bd.cmd")
	err := runAgentScriptCommandInStore(io.Discard, io.Discard, dir, nil, bd, "update", "gc-1", "--append-notes", "a&echo>pwned")
	if err == nil || !strings.Contains(err.Error(), "batch file") {
		t.Errorf("runAgentScriptCommandInStore = %v, want the batch file refused", err)
	}
	if _, statErr := os.Stat(filepath.Join(dir, "pwned")); statErr == nil {
		t.Error("cmd.exe ran the action's notes: pwned exists in the store directory")
	}
}
