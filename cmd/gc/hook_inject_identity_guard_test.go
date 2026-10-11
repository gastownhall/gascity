package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

// The `--inject` forms of `gc mail check` and `gc nudge drain` emit
// <system-reminder> blocks straight into a provider's system prompt. Nine
// provider overlays call them on every turn, and gc stages those overlays into
// the session work directory — commonly a city or rig root — so a human who
// opens the same provider in that directory gets them too.
//
// For mail that is worse than noise: the recipient falls back through
// GC_SESSION_ID, GC_ALIAS, GC_AGENT and then to "human" (cmd_mail.go), so an
// unmanaged session resolves to the operator's own inbox and injects it as an
// instruction. The agent then goes and acts on the human's mail instead of
// answering what the human typed.
//
// Same rule as `gc prime --hook`: no gc identity means this did not come from a
// session gc started, so the injection form stays silent. An explicitly named
// target is left alone — that is a deliberate request, not the implicit
// fallback that causes the hijack — and the plain non-inject forms are
// untouched because those are the human-facing commands.

func unmanagedInjectEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GC_CITY", filepath.Join(t.TempDir(), "missing-city"))
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_SESSION_NAME", "")
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_TEMPLATE", "")
	t.Setenv(managedSessionHookEnv, "")
}

// TestUnmanagedMailInjectWouldTargetTheOperatorInbox documents the vector the
// guard closes: with no gc identity the mailbox resolution falls all the way
// back to "human", so an unguarded --inject would read the operator's own inbox
// and inject it as an instruction.
func TestUnmanagedMailInjectWouldTargetTheOperatorInbox(t *testing.T) {
	unmanagedInjectEnv(t)

	got := defaultMailIdentityCandidates()
	if len(got) != 1 || got[0] != "human" {
		t.Fatalf("defaultMailIdentityCandidates() = %v, want [human]", got)
	}
}

// TestMailCheckInjectSilentWithoutManagedIdentity is the regression guard for
// the operator's own inbox being injected into a session they started.
func TestMailCheckInjectSilentWithoutManagedIdentity(t *testing.T) {
	unmanagedInjectEnv(t)

	var stdout, stderr bytes.Buffer
	code := cmdMailCheckWithFormat(nil, true, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdMailCheckWithFormat = %d, want 0; stderr=%q", code, stderr.String())
	}
	if out := stdout.String(); out != "" {
		t.Fatalf("unmanaged --inject leaked mail into the system prompt: %q", out)
	}
}

// TestNudgeDrainInjectSilentWithoutManagedIdentity is the sibling guard.
func TestNudgeDrainInjectSilentWithoutManagedIdentity(t *testing.T) {
	unmanagedInjectEnv(t)

	var stdout, stderr bytes.Buffer
	code := cmdNudgeDrainWithFormat(nil, true, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdNudgeDrainWithFormat = %d, want 0; stderr=%q", code, stderr.String())
	}
	if out := stdout.String(); out != "" {
		t.Fatalf("unmanaged --inject emitted hook context: %q", out)
	}
}

// TestMailCheckInjectHonorsAnExplicitTarget proves the guard keys on the
// implicit "human" fallback, not on the flag: naming a mailbox is a deliberate
// request and must still be served.
func TestMailCheckInjectHonorsAnExplicitTarget(t *testing.T) {
	unmanagedInjectEnv(t)

	var stdout, stderr bytes.Buffer
	_ = cmdMailCheckWithFormat([]string{"mayor"}, true, "", &stdout, &stderr)
	if stdout.String() == "" && stderr.String() == "" {
		t.Fatal("an explicitly named target must not be silenced by the identity guard")
	}
}

// TestMailCheckWithoutInjectUnaffectedByGuard pins that the human-facing form
// still runs: the guard is scoped to the hook-injection flag only.
func TestMailCheckWithoutInjectUnaffectedByGuard(t *testing.T) {
	unmanagedInjectEnv(t)

	var stdout, stderr bytes.Buffer
	_ = cmdMailCheckWithFormat(nil, false, "", &stdout, &stderr)
	if stdout.String() == "" && stderr.String() == "" {
		t.Fatal("plain `gc mail check` must not be silenced by the identity guard")
	}
}

// TestInjectGuardLetsManagedSessionsThrough is the counterweight: a real
// managed session must still receive its nudges and mail.
func TestInjectGuardLetsManagedSessionsThrough(t *testing.T) {
	unmanagedInjectEnv(t)
	t.Setenv("GC_AGENT", "worker")

	var stdout, stderr bytes.Buffer
	_ = cmdNudgeDrainWithFormat(nil, true, "", &stdout, &stderr)
	if stdout.String() == "" && stderr.String() == "" {
		t.Fatal("a managed session must still get hook context; the guard over-suppressed")
	}
	if strings.TrimSpace(stdout.String()) == "" && strings.TrimSpace(stderr.String()) == "" {
		t.Fatal("expected some output for a managed identity")
	}
}
