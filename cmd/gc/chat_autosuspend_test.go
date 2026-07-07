package main

import (
	"bytes"
	"context"
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
	autoSuspendChatSessions(store, sp, 30*time.Minute, clk, &stdout, &stderr)

	// s1 should be suspended (idle 2h > 30m timeout).
	got1, err := mgr.Get(s1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got1.State != session.StateSuspended {
		t.Errorf("s1 state = %q, want suspended", got1.State)
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
	autoSuspendChatSessions(store, sp, 30*time.Minute, clk, &stdout, &stderr)

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
	autoSuspendChatSessions(nil, sp, 30*time.Minute, clk, &stdout, &stderr)
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("unexpected output with nil store: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}
