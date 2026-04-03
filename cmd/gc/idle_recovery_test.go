package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestIdleRecovery_NudgesSessionWithWork(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "claude-mc-abc", runtime.Config{})
	// Simulate idle prompt in pane
	sp.SetPeekOutput("claude-mc-abc", "❯ \n  ⏵⏵ bypass permissions on (shift+tab to cycle) · esc to interrupt")
	// Wait, that has "esc to interrupt" — that means busy. Fix:
	sp.SetPeekOutput("claude-mc-abc", "\n\n❯ \n")

	session := beads.Bead{
		ID:     "b1",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "claude-mc-abc",
			"pool_managed": "true",
		},
	}
	workBead := beads.Bead{
		ID:       "w1",
		Status:   "in_progress",
		Assignee: "claude-mc-abc",
	}

	ir := newIdleRecovery(0) // no grace for test
	var stdout bytes.Buffer

	ir.recoverIdleSessions(sp, []beads.Bead{session}, []beads.Bead{workBead}, time.Now(), &stdout)

	// First call records first-seen-idle, doesn't act yet.
	if strings.Contains(stdout.String(), "nudged") {
		t.Fatal("should not nudge on first observation")
	}

	// Second call — past grace (0) — should nudge.
	stdout.Reset()
	ir.recoverIdleSessions(sp, []beads.Bead{session}, []beads.Bead{workBead}, time.Now(), &stdout)

	if !strings.Contains(stdout.String(), "nudged claude-mc-abc") {
		t.Errorf("expected nudge, got: %s", stdout.String())
	}

	// Should have called Nudge, not SetMeta(GC_DRAIN_ACK).
	for _, c := range sp.Calls {
		if c.Method == "SetMeta" && strings.Contains(c.Name, "GC_DRAIN_ACK") {
			t.Error("should nudge, not drain, when session has work")
		}
	}
}

func TestIdleRecovery_DrainsSessionWithoutWork(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "claude-mc-xyz", runtime.Config{})
	sp.SetPeekOutput("claude-mc-xyz", "\n\n❯ \n")

	session := beads.Bead{
		ID:     "b2",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "claude-mc-xyz",
			"pool_managed": "true",
		},
	}

	ir := newIdleRecovery(0)
	var stdout bytes.Buffer

	// First call records idle.
	ir.recoverIdleSessions(sp, []beads.Bead{session}, nil, time.Now(), &stdout)
	// Second call — should drain.
	stdout.Reset()
	ir.recoverIdleSessions(sp, []beads.Bead{session}, nil, time.Now(), &stdout)

	if !strings.Contains(stdout.String(), "GC_DRAIN_ACK") {
		t.Errorf("expected drain, got: %s", stdout.String())
	}

	ack, _ := sp.GetMeta("claude-mc-xyz", "GC_DRAIN_ACK")
	if ack != "1" {
		t.Errorf("GC_DRAIN_ACK = %q, want \"1\"", ack)
	}
}

func TestIdleRecovery_SkipsBusySessions(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "claude-mc-busy", runtime.Config{})
	// Has busy indicator
	sp.SetPeekOutput("claude-mc-busy", "● Working on something...\n  ⏵⏵ bypass permissions on · esc to interrupt\n❯ ")

	session := beads.Bead{
		ID:     "b3",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "claude-mc-busy",
			"pool_managed": "true",
		},
	}

	ir := newIdleRecovery(0)
	var stdout bytes.Buffer

	ir.recoverIdleSessions(sp, []beads.Bead{session}, nil, time.Now(), &stdout)
	ir.recoverIdleSessions(sp, []beads.Bead{session}, nil, time.Now(), &stdout)

	if stdout.String() != "" {
		t.Errorf("should not act on busy session, got: %s", stdout.String())
	}
}

func TestIdleRecovery_RespectsGracePeriod(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "claude-mc-grace", runtime.Config{})
	sp.SetPeekOutput("claude-mc-grace", "\n❯ \n")

	session := beads.Bead{
		ID:     "b4",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "claude-mc-grace",
			"pool_managed": "true",
		},
	}

	ir := newIdleRecovery(5 * time.Minute)
	var stdout bytes.Buffer
	now := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)

	// First observation.
	ir.recoverIdleSessions(sp, []beads.Bead{session}, nil, now, &stdout)
	// 1 minute later — still within grace.
	ir.recoverIdleSessions(sp, []beads.Bead{session}, nil, now.Add(1*time.Minute), &stdout)
	if stdout.String() != "" {
		t.Errorf("should not act within grace period, got: %s", stdout.String())
	}

	// 6 minutes later — past grace.
	ir.recoverIdleSessions(sp, []beads.Bead{session}, nil, now.Add(6*time.Minute), &stdout)
	if !strings.Contains(stdout.String(), "GC_DRAIN_ACK") {
		t.Errorf("should drain after grace period, got: %s", stdout.String())
	}
}

func TestIdleRecovery_SkipsNonPoolSessions(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "mayor", runtime.Config{})
	sp.SetPeekOutput("mayor", "\n❯ \n")

	session := beads.Bead{
		ID:     "b5",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "mayor",
			// no pool_managed
		},
	}

	ir := newIdleRecovery(0)
	var stdout bytes.Buffer

	ir.recoverIdleSessions(sp, []beads.Bead{session}, nil, time.Now(), &stdout)
	ir.recoverIdleSessions(sp, []beads.Bead{session}, nil, time.Now(), &stdout)

	if stdout.String() != "" {
		t.Errorf("should skip non-pool sessions, got: %s", stdout.String())
	}
}

func TestIdleRecovery_ClearsTimerWhenNoLongerIdle(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "claude-mc-flip", runtime.Config{})
	sp.SetPeekOutput("claude-mc-flip", "\n❯ \n")

	session := beads.Bead{
		ID:     "b6",
		Status: "open",
		Metadata: map[string]string{
			"session_name": "claude-mc-flip",
			"pool_managed": "true",
		},
	}

	ir := newIdleRecovery(5 * time.Minute)
	var stdout bytes.Buffer
	now := time.Date(2026, 4, 3, 12, 0, 0, 0, time.UTC)

	// First observation — records idle.
	ir.recoverIdleSessions(sp, []beads.Bead{session}, nil, now, &stdout)
	if _, ok := ir.firstIdle["claude-mc-flip"]; !ok {
		t.Fatal("should track idle time")
	}

	// Session becomes busy — timer should clear.
	sp.SetPeekOutput("claude-mc-flip", "● Running tool...\n  ⏵⏵ esc to interrupt\n")
	ir.recoverIdleSessions(sp, []beads.Bead{session}, nil, now.Add(1*time.Minute), &stdout)
	if _, ok := ir.firstIdle["claude-mc-flip"]; ok {
		t.Error("idle timer should be cleared when session is no longer idle")
	}
}
