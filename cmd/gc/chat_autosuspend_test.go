package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestAutoSuspendChatSessions(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := session.NewManagerWithOptions(store, sp)
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	// Create two sessions.
	s1, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "default", Title: "S1", Command: "echo s1", WorkDir: "/tmp", Provider: "test", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	s2, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "default", Title: "S2", Command: "echo s2", WorkDir: "/tmp", Provider: "test", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatal(err)
	}

	// Set activity times: s1 was active 2 hours ago, s2 was active 1 minute ago.
	sp.SetActivity(s1.SessionName, now.Add(-2*time.Hour))
	sp.SetActivity(s2.SessionName, now.Add(-1*time.Minute))

	// Neither is attached.
	sp.SetAttached(s1.SessionName, false)
	sp.SetAttached(s2.SessionName, false)

	var stdout, stderr bytes.Buffer
	autoSuspendChatSessions("", nil, store, sp, 30*time.Minute, clk, &stdout, &stderr)

	// s1 should be suspended (idle 2h > 30m timeout).
	got1, err := mgr.Get(s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got1.State != session.StateSuspended {
		t.Errorf("s1 state = %q, want suspended", got1.State)
	}
	// The idle auto-suspend is not an operator's: it writes no hold, so the
	// session's next wake reason resumes it (mc-esqo7).
	if raw, _ := store.Get(s1.ID); raw.Metadata["held_until"] != "" || raw.Metadata["sleep_intent"] != "" {
		t.Errorf("s1 held_until=%q sleep_intent=%q, want no hold", raw.Metadata["held_until"], raw.Metadata["sleep_intent"])
	}

	// s2 should still be active (idle 1m < 30m timeout).
	got2, err := mgr.Get(s2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got2.State != session.StateActive {
		t.Errorf("s2 state = %q, want active", got2.State)
	}

	// Verify stdout mentions the suspended session.
	if !strings.Contains(stdout.String(), s1.ID) {
		t.Errorf("stdout should mention suspended session ID %s, got: %s", s1.ID, stdout.String())
	}
	if stderr.Len() != 0 {
		t.Errorf("unexpected stderr: %s", stderr.String())
	}
}

// TestAutoSuspendSuspendsLabelLostActiveSession pins the deliberate union-feed
// upgrade reaching autoSuspendChatSessions: catalog.List now routes through
// Manager.List's type+label union (was label-only ListFull), so an active
// session bead that LOST its gc:session label after a crash — invisible to the
// old label-only listing and therefore never auto-suspended — is now surfaced by
// the union's type leg and correctly suspended. This is the intended fix, not a
// regression: the previously-stranded session gets reaped.
func TestAutoSuspendSuspendsLabelLostActiveSession(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := session.NewManagerWithOptions(store, sp)
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	s1, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "default", Title: "LabelLost", Command: "echo s1", WorkDir: "/tmp", Provider: "test", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	// Strip the gc:session label but keep Type=session: the union's type leg still
	// finds it, the retired label-only ListFull would not.
	if err := store.Update(s1.ID, beads.UpdateOpts{RemoveLabels: []string{session.LabelSession}}); err != nil {
		t.Fatalf("stripping label: %v", err)
	}

	sp.SetActivity(s1.SessionName, now.Add(-2*time.Hour))
	sp.SetAttached(s1.SessionName, false)

	var stdout, stderr bytes.Buffer
	autoSuspendChatSessions("", nil, store, sp, 30*time.Minute, clk, &stdout, &stderr)

	got, err := mgr.Get(s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != session.StateSuspended {
		t.Errorf("label-lost session state = %q, want suspended (union feed must surface it)", got.State)
	}
	if !strings.Contains(stdout.String(), s1.ID) {
		t.Errorf("stdout should mention suspended session ID %s, got: %s", s1.ID, stdout.String())
	}
}

func TestAutoSuspendSkipsAttachedSessions(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := session.NewManagerWithOptions(store, sp)
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	s1, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "default", Title: "Attached", Command: "echo a", WorkDir: "/tmp", Provider: "test", Env: nil, Resume: session.ProviderResume{}, Hints: runtime.Config{}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatal(err)
	}

	// Old activity but attached — should NOT be suspended.
	sp.SetActivity(s1.SessionName, now.Add(-2*time.Hour))
	sp.SetAttached(s1.SessionName, true)

	var stdout, stderr bytes.Buffer
	autoSuspendChatSessions("", nil, store, sp, 30*time.Minute, clk, &stdout, &stderr)

	got, err := mgr.Get(s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != session.StateActive {
		t.Errorf("attached session state = %q, want active", got.State)
	}
}

func TestAutoSuspendNilStore(t *testing.T) {
	sp := runtime.NewFake()
	clk := &clock.Fake{Time: time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)}
	var stdout, stderr bytes.Buffer
	// Should not panic with nil store.
	autoSuspendChatSessions("", nil, nil, sp, 30*time.Minute, clk, &stdout, &stderr)
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("unexpected output with nil store: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

// The catalog's Attached is a bool that reads "detached" when the probe fails;
// an attachment probe that cannot tell must hold the suspend.
func TestAutoSuspendHoldsOnAttachProbeError(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	mgr := session.NewManagerWithOptions(store, sp)
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}

	s1, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "default", Title: "Probe fails", Command: "echo a", WorkDir: "/tmp", Provider: "test", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatal(err)
	}
	sp.SetActivity(s1.SessionName, now.Add(-2*time.Hour))
	sp.AttachedErrors[s1.SessionName] = fmt.Errorf("attach probe timed out: %w", runtime.ErrRuntimeUnavailable)

	var stdout, stderr bytes.Buffer
	autoSuspendChatSessions("", nil, store, sp, 30*time.Minute, clk, &stdout, &stderr)

	got, err := mgr.Get(s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != session.StateActive {
		t.Errorf("session state = %q, want active while attachment is unknown", got.State)
	}
	if n := sp.CountCalls("Stop", s1.SessionName); n != 0 {
		t.Errorf("Stop calls = %d, want 0", n)
	}
}
