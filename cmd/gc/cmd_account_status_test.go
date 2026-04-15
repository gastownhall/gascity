package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/account"
)

// TestAccountStatusCmd_ShowsSessions verifies that doAccountStatus lists
// sessions with their mapped account handles. Two panes with different
// CLAUDE_CONFIG_DIR values should each resolve to the correct handle.
func TestAccountStatusCmd_ShowsSessions(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
		account.Account{Handle: "work2", ConfigDir: "/config/work2"},
	)

	panes := map[string]*FakePane{
		"session-a": {Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"}},
		"session-b": {Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work2"}},
	}
	ops := FakeTmuxOps(panes)

	var stdout, stderr bytes.Buffer
	code := doAccountStatus(ops, reg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAccountStatus exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	// Both handles should appear in the output.
	if !strings.Contains(out, "work1") {
		t.Errorf("output missing handle 'work1'; got:\n%s", out)
	}
	if !strings.Contains(out, "work2") {
		t.Errorf("output missing handle 'work2'; got:\n%s", out)
	}
	// Both session names should appear.
	if !strings.Contains(out, "session-a") {
		t.Errorf("output missing session name 'session-a'; got:\n%s", out)
	}
	if !strings.Contains(out, "session-b") {
		t.Errorf("output missing session name 'session-b'; got:\n%s", out)
	}
}

// TestAccountStatusCmd_NoTmux verifies that doAccountStatus returns an error
// when tmux is not running. The PRD-specified message is:
// "error: tmux is not running. gc account status requires an active tmux server."
func TestAccountStatusCmd_NoTmux(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)

	// Empty panes map → FakeTmuxOps.IsRunning() returns false.
	ops := FakeTmuxOps(map[string]*FakePane{})

	var stdout, stderr bytes.Buffer
	code := doAccountStatus(ops, reg, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doAccountStatus exit code = %d, want 1", code)
	}

	errMsg := stderr.String()
	want := "tmux is not running"
	if !strings.Contains(errMsg, want) {
		t.Errorf("stderr = %q, want it to contain %q", errMsg, want)
	}
}

// TestAccountStatusCmd_UnmappedSession verifies that sessions with a
// CLAUDE_CONFIG_DIR that doesn't match any registered account show
// "(no account)" in the handle column.
func TestAccountStatusCmd_UnmappedSession(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)

	panes := map[string]*FakePane{
		"session-a": {Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"}},
		"session-b": {Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/unknown"}},
	}
	ops := FakeTmuxOps(panes)

	var stdout, stderr bytes.Buffer
	code := doAccountStatus(ops, reg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAccountStatus exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	// The mapped session should show its handle.
	if !strings.Contains(out, "work1") {
		t.Errorf("output missing handle 'work1'; got:\n%s", out)
	}
	// The unmapped session should show "(no account)".
	if !strings.Contains(out, "(no account)") {
		t.Errorf("output missing '(no account)' for unmapped session; got:\n%s", out)
	}
}

// TestAccountStatusCmd_NoSessions verifies that doAccountStatus displays
// an appropriate message when tmux is running but there are no active panes.
// We simulate this with a TmuxOps where IsRunning returns true but ListPanes
// returns an empty list.
func TestAccountStatusCmd_NoSessions(t *testing.T) {
	reg := account.TestRegistry(t,
		account.Account{Handle: "work1", ConfigDir: "/config/work1"},
	)

	// Custom TmuxOps: IsRunning true, but ListPanes returns empty.
	ops := TmuxOps{
		IsRunning: func() bool { return true },
		ListPanes: func() ([]PaneInfo, error) { return nil, nil },
		ShowEnv: func(_, _ string) (string, error) {
			return "", nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := doAccountStatus(ops, reg, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAccountStatus exit code = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "no active sessions") {
		t.Errorf("output should contain 'no active sessions'; got:\n%s", out)
	}
}

// TestAccountStatusCmd_EmptyRegistry verifies that doAccountStatus returns
// exit code 1 and prompts the operator to run "gc account add" when the
// registry has no accounts registered. The guard should fire before any
// tmux inspection occurs.
//
// PRD Scenario #41: When gc account status is run with no accounts, the
// command exits with a non-zero status and prompts the operator to run
// gc account add.
//
// Audit GAP-8b fix.
func TestAccountStatusCmd_EmptyRegistry(t *testing.T) {
	// Empty registry — no accounts.
	reg := account.Registry{}

	// Provide tmux ops that would succeed — but they should never be reached
	// because the empty-registry guard fires first.
	ops := FakeTmuxOps(map[string]*FakePane{
		"session-a": {Env: map[string]string{"CLAUDE_CONFIG_DIR": "/config/work1"}},
	})

	var stdout, stderr bytes.Buffer
	code := doAccountStatus(ops, reg, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doAccountStatus exit code = %d, want 1", code)
	}

	errOut := stderr.String()
	if !strings.Contains(errOut, "no accounts registered") {
		t.Errorf("stderr should contain %q, got: %s", "no accounts registered", errOut)
	}
	if !strings.Contains(errOut, "gc account add") {
		t.Errorf("stderr should contain %q, got: %s", "gc account add", errOut)
	}
}
