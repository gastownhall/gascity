//go:build windows

package exec //nolint:revive // internal package, always imported with alias

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBatchFileScriptNeverReceivesCallerTextCmdExeReads pins that an exec:
// runtime whose script is a batch file refuses, before the script runs, an
// operation whose argument holds a character cmd.exe acts on. Windows starts a
// batch file through cmd.exe, which runs "x&rem" as two commands and expands
// %USERNAME%.
func TestBatchFileScriptNeverReceivesCallerTextCmdExeReads(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "runtime.cmd")
	ran := filepath.Join(dir, "ran.txt")
	if err := os.WriteFile(script, []byte("@echo off\r\necho %*>>\"%~dp0ran.txt\"\r\nexit /b 1\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewProvider(script)
	for _, hostile := range []string{"x&rem", "x-%USERNAME%"} {
		for _, op := range []struct {
			name string
			call func() error
		}{
			{"Stop", func() error { return p.Stop(hostile) }},
			{"GetMeta", func() error { _, err := p.GetMeta("s1", hostile); return err }},
			{"RemoveMeta", func() error { return p.RemoveMeta("s1", hostile) }},
			{"ListRunning", func() error { _, err := p.ListRunning(hostile); return err }},
			{"CheckImage", func() error { return p.CheckImage(hostile) }},
			{"CopyTo", func() error { return p.CopyTo("s1", hostile, "dst") }},
		} {
			err := op.call()
			if err == nil || !strings.Contains(err.Error(), "batch file") {
				t.Errorf("%s(%q) = %v, want a batch-file refusal", op.name, hostile, err)
			}
			if got, readErr := os.ReadFile(ran); readErr == nil {
				t.Fatalf("%s(%q) ran the batch file, which saw %q", op.name, hostile, got)
			}
		}
	}

	// Plain arguments still reach a batch-file script.
	if _, err := p.GetMeta("s1", "k"); err == nil || strings.Contains(err.Error(), "batch file") {
		t.Fatalf("GetMeta(s1, k) = %v, want the script's own failure", err)
	}
	if got, err := os.ReadFile(ran); err != nil || strings.TrimSpace(string(got)) != "get-meta s1 k" {
		t.Fatalf("script saw %q (%v), want \"get-meta s1 k\"", got, err)
	}
}
