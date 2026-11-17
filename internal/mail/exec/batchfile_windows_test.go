//go:build windows

package exec //nolint:revive // internal package, always imported with alias

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBatchFileScriptNeverReceivesCallerTextCmdExeReads pins that an exec:
// mail provider whose script is a batch file refuses, before the script runs,
// an operation whose message ID or recipient holds a character cmd.exe acts
// on. Windows starts a batch file through cmd.exe, which runs "m-1&rem" as two
// commands and expands %USERNAME%.
func TestBatchFileScriptNeverReceivesCallerTextCmdExeReads(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "mail.cmd")
	ran := filepath.Join(dir, "ran.txt")
	if err := os.WriteFile(script, []byte("@echo off\r\necho %*>>\"%~dp0ran.txt\"\r\nexit /b 1\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := NewProvider(script)
	seen := func() []string {
		data, err := os.ReadFile(ran)
		if err != nil {
			return nil
		}
		return strings.Fields(strings.ReplaceAll(string(data), "\r\n", "\n"))
	}
	for _, hostile := range []string{"m-1&rem", "m-%USERNAME%"} {
		for _, op := range []struct {
			name string
			call func() error
		}{
			{"Get", func() error { _, err := p.Get(hostile); return err }},
			{"Inbox", func() error { _, err := p.Inbox(hostile); return err }},
			{"Send", func() error { _, err := p.Send("from", hostile, "subject", "body"); return err }},
			{"Archive", func() error { return p.Archive(hostile) }},
			{"Thread", func() error { _, err := p.Thread(hostile); return err }},
			{"Count", func() error { _, _, err := p.Count(hostile); return err }},
		} {
			err := op.call()
			if err == nil || !strings.Contains(err.Error(), "batch file") {
				t.Errorf("%s(%q) = %v, want a batch-file refusal", op.name, hostile, err)
			}
			// ensure-running carries no caller text and may run.
			for _, word := range seen() {
				if word != "ensure-running" {
					t.Fatalf("%s(%q) ran the batch file, which saw %q", op.name, hostile, seen())
				}
			}
		}
	}

	// A plain ID still reaches a batch-file script.
	if _, err := p.Get("m-1"); err == nil || strings.Contains(err.Error(), "batch file") {
		t.Fatalf("Get(m-1) = %v, want the script's own failure", err)
	}
	if got := seen(); len(got) < 2 || got[len(got)-2] != "get" || got[len(got)-1] != "m-1" {
		t.Fatalf("script saw %q, want it to end with get m-1", got)
	}
}
