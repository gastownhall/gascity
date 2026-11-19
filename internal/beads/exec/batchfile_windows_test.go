//go:build windows

package exec //nolint:revive // internal package, always imported with alias

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBatchFileScriptNeverReceivesCallerTextCmdExeReads pins that an exec:
// store whose script is a batch file refuses, before the script runs, an
// operation whose ID or filter holds a character cmd.exe acts on. Windows
// starts a batch file through cmd.exe, which runs "gc-1&rem" as two commands
// and expands %USERNAME%; IDs and filters arrive from API paths and queries.
func TestBatchFileScriptNeverReceivesCallerTextCmdExeReads(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "store.cmd")
	ran := filepath.Join(dir, "ran.txt")
	if err := os.WriteFile(script, []byte("@echo off\r\necho %*>>\"%~dp0ran.txt\"\r\nexit /b 1\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewStore(script)
	for _, hostile := range []string{"gc-1&rem", "gc-%USERNAME%"} {
		for _, op := range []struct {
			name string
			call func() error
		}{
			{"Get", func() error { _, err := s.Get(hostile); return err }},
			{"List by assignee", func() error { _, err := s.List(beads.ListQuery{Assignee: hostile}); return err }},
			{"List by label", func() error { _, err := s.List(beads.ListQuery{Label: hostile}); return err }},
			{"List children", func() error { _, err := s.List(beads.ListQuery{ParentID: hostile}); return err }},
			{"SetMetadata key", func() error { return s.SetMetadata("gc-1", hostile, "v") }},
			{"DepList", func() error { _, err := s.DepList(hostile, "down"); return err }},
			{"Close", func() error { return s.Close(hostile) }},
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

	// A plain ID still reaches a batch-file script.
	if _, err := s.Get("gc-1"); err == nil || strings.Contains(err.Error(), "batch file") {
		t.Fatalf("Get(gc-1) = %v, want the script's own failure", err)
	}
	if got, err := os.ReadFile(ran); err != nil || strings.TrimSpace(string(got)) != "get gc-1" {
		t.Fatalf("script saw %q (%v), want \"get gc-1\"", got, err)
	}
}
