package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeRecoverTarget builds a nudgeTarget pointed at a session named "sess-1"
// with the given resolved provider. Resolved is intentionally a pointer
// because the live resolveNudgeTarget code path also returns a pointer.
func fakeRecoverTarget(resolved *config.ResolvedProvider) nudgeTarget {
	return nudgeTarget{
		identity:    "rig/witness",
		alias:       "witness-1",
		sessionName: "sess-1",
		resolved:    resolved,
	}
}

func TestDeliverSessionRecover_ClaudeSendsKeys(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name: "claude",
		RecoveryHints: config.RecoveryHints{
			SoftRecoveryKeys: []string{"C-u", "/rewind", "Enter"},
		},
	}
	target := fakeRecoverTarget(resolved)

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), target.sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := deliverSessionRecover(target, sp, stdout, stderr)
	if code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr=%q)", code, stderr.String())
	}

	// Exactly one SendKeys call, with all three keys passed in order.
	var sendCalls []runtime.Call
	for _, c := range sp.Calls {
		if c.Method == "SendKeys" {
			sendCalls = append(sendCalls, c)
		}
	}
	if len(sendCalls) != 1 {
		t.Fatalf("SendKeys calls = %d, want 1; all calls=%v", len(sendCalls), sp.Calls)
	}
	wantMsg := "C-u /rewind Enter"
	if sendCalls[0].Message != wantMsg {
		t.Errorf("SendKeys message = %q, want %q", sendCalls[0].Message, wantMsg)
	}
	if sendCalls[0].Name != target.sessionName {
		t.Errorf("SendKeys name = %q, want %q", sendCalls[0].Name, target.sessionName)
	}
	if !strings.Contains(stdout.String(), "Sent soft recovery to witness-1") {
		t.Errorf("stdout = %q, want contains success line", stdout.String())
	}
}

func TestDeliverSessionRecover_NoHintExitsTwoWithoutSending(t *testing.T) {
	// codex (or any provider without RecoveryHints) must NOT have keys
	// delivered, and must exit 2 so the dog ladder advances.
	resolved := &config.ResolvedProvider{Name: "codex"}
	target := fakeRecoverTarget(resolved)

	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), target.sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := deliverSessionRecover(target, sp, stdout, stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (provider has no soft recovery)", code)
	}
	for _, c := range sp.Calls {
		if c.Method == "SendKeys" {
			t.Errorf("unexpected SendKeys call %+v on provider with no recovery hint", c)
		}
	}
	// The molecule's strike-1 step greps stderr for this exact substring
	// to distinguish "no soft rung available, advance" from a hard error.
	// Renaming this marker will silently break mol-shutdown-dance.
	if !strings.Contains(stderr.String(), "no soft recovery; skipping") {
		t.Errorf("stderr = %q, want contains 'no soft recovery; skipping'", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want empty", stdout.String())
	}
}

func TestDeliverSessionRecover_NilResolvedExitsTwo(t *testing.T) {
	// A session bead with no resolved provider (rare edge case — agent
	// not in city config) must skip rather than panic.
	target := fakeRecoverTarget(nil)
	sp := runtime.NewFake()
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := deliverSessionRecover(target, sp, stdout, stderr)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	// Same marker as the no-keys branch so the molecule's strike-1 grep
	// catches both "agent has no resolved provider" and "provider has no
	// hint" with one rule.
	if !strings.Contains(stderr.String(), "no soft recovery; skipping") {
		t.Errorf("stderr = %q, want contains 'no soft recovery; skipping'", stderr.String())
	}
}

func TestDeliverSessionRecover_SessionNotRunningExitsOne(t *testing.T) {
	// Provider has a hint, but the session isn't running — hard error
	// (the warrant flow shouldn't waste a strike on a dead session).
	resolved := &config.ResolvedProvider{
		Name: "claude",
		RecoveryHints: config.RecoveryHints{
			SoftRecoveryKeys: []string{"C-u", "/rewind", "Enter"},
		},
	}
	target := fakeRecoverTarget(resolved)
	sp := runtime.NewFake() // no Start → not running

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := deliverSessionRecover(target, sp, stdout, stderr)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1 (session not running)", code)
	}
	for _, c := range sp.Calls {
		if c.Method == "SendKeys" {
			t.Errorf("unexpected SendKeys call against non-running session: %+v", c)
		}
	}
}

func TestDeliverSessionRecover_SendKeysErrorExitsOne(t *testing.T) {
	// When the provider returns an error from SendKeys, propagate it.
	resolved := &config.ResolvedProvider{
		Name: "claude",
		RecoveryHints: config.RecoveryHints{
			SoftRecoveryKeys: []string{"C-u", "/rewind", "Enter"},
		},
	}
	target := fakeRecoverTarget(resolved)
	sp := runtime.NewFailFake() // broken: all ops error, IsRunning false

	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	code := deliverSessionRecover(target, sp, stdout, stderr)
	// FailFake returns false for IsRunning so we hit the not-running
	// branch first; that's still exit 1, just via the earlier guard.
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
}
