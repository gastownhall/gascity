//go:build windows

package herdr

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBatchFileHerdrNeverReceivesTextCmdExeReads pins that a herdr reached
// through a batch file is refused, before it runs, any argument holding a
// character cmd.exe acts on. Windows starts a batch file through cmd.exe, which
// runs "a&rem" as two commands and expands %USERNAME% even inside quotes, and
// herdr carries nudge and startup-prompt text on its command line.
func TestBatchFileHerdrNeverReceivesTextCmdExeReads(t *testing.T) {
	dir := t.TempDir()
	shim := filepath.Join(dir, "herdr.cmd")
	ran := filepath.Join(dir, "ran.txt")
	if err := os.WriteFile(shim, []byte("@echo off\r\necho %*>>\"%~dp0ran.txt\"\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &client{session: "gc-test", bin: shim}
	ctx := context.Background()
	for _, text := range []string{"a&rem", "please review a&rem", "expand %USERNAME% here", "line one\nline two"} {
		for _, op := range []struct {
			name string
			call func() error
		}{
			{"deliverNudge", func() error { return c.deliverNudge(ctx, "p1", text) }},
			{"paneRunCommand", func() error { return c.paneRunCommand(ctx, "p1", text) }},
			{"paneRead", func() error { _, err := c.paneRead(ctx, text, "visible", 0); return err }},
		} {
			err := op.call()
			if err == nil || !strings.Contains(err.Error(), "batch file") {
				t.Errorf("%s(%q) = %v, want a batch-file refusal", op.name, text, err)
			}
			if got, readErr := os.ReadFile(ran); readErr == nil {
				t.Fatalf("%s(%q) ran herdr.cmd, which saw %q", op.name, text, got)
			}
		}
	}

	// Plain text still reaches a batch-file herdr.
	if err := c.agentPrompt(ctx, "p1", "plain words"); err != nil {
		t.Fatalf("agentPrompt(plain words) = %v, want nil", err)
	}
	if got, err := os.ReadFile(ran); err != nil || !strings.Contains(string(got), `agent prompt p1 "plain words"`) {
		t.Fatalf("herdr.cmd saw %q (%v), want the plain prompt", got, err)
	}
}
