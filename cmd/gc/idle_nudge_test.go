package main

import (
	"bytes"
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

func idleClaimTestCfg() *config.City {
	return &config.City{Agents: []config.Agent{{
		Name:  "polecat",
		Nudge: "Run gc hook --claim --json now; if it returns work, execute the claimed formula immediately.",
	}}}
}

func idleClaimPoolSession() beads.Bead {
	return beads.Bead{
		ID:     "s-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"session_name":                    "worker-1",
			"pool_managed":                    "true",
			"template":                        "polecat",
			beadmeta.TriggerBeadIDMetadataKey: "w-1",
		},
	}
}

func runningFake(t *testing.T) *runtime.Fake {
	t.Helper()
	sp := runtime.NewFake()
	if err := sp.Start(context.TODO(), "worker-1", runtime.Config{}); err != nil {
		t.Fatalf("fake start: %v", err)
	}
	return sp
}

// A slot handed work it never claimed (trigger bead still open) is observed on
// the first tick (grace), then nudged once the grace elapses.
func TestNudgeStalledPoolClaims_NudgesAfterGrace(t *testing.T) {
	sp := runningFake(t)
	cfg := idleClaimTestCfg()
	sessions := []beads.Bead{idleClaimPoolSession()}
	work := []beads.Bead{{ID: "w-1", Status: "open"}} // unclaimed
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var out bytes.Buffer

	// First tick: observe only — start the grace clock, no nudge.
	nudgeStalledPoolClaims(sp, cfg, store, sessions, work, base, &out)
	if out.Len() != 0 {
		t.Fatalf("first tick should not nudge (grace): %q", out.String())
	}
	if got := sessions[0].Metadata[idleClaimNudgeTriggerKey]; got != "w-1" {
		t.Fatalf("expected marker trigger w-1, got %q", got)
	}

	// Past grace: nudge, and bump the attempt count.
	nudgeStalledPoolClaims(sp, cfg, store, sessions, work, base.Add(idleClaimNudgeGrace+time.Second), &out)
	if !bytes.Contains(out.Bytes(), []byte("nudged worker-1 to claim w-1")) {
		t.Fatalf("expected nudge past grace, got: %q", out.String())
	}
	if got := sessions[0].Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("expected attempt count 1, got %q", got)
	}
}

// The instant a slot claims (trigger bead flips to in_progress) it must never be
// touched — this is the inversion that the reverted #312 nudger got wrong.
func TestNudgeStalledPoolClaims_NeverTouchesWorkingSlot(t *testing.T) {
	sp := runningFake(t)
	cfg := idleClaimTestCfg()
	sessions := []beads.Bead{idleClaimPoolSession()}
	work := []beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: "worker-1"}}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, sessions, work, base, &out)
	nudgeStalledPoolClaims(sp, cfg, store, sessions, work, base.Add(time.Hour), &out)
	if out.Len() != 0 {
		t.Fatalf("must not nudge a working slot: %q", out.String())
	}
	if got := sessions[0].Metadata[idleClaimNudgeTriggerKey]; got != "" {
		t.Fatalf("marker should stay clear for a claimed bead, got %q", got)
	}
}

// After the attempt cap is reached the backstop gives up — bounded, never an
// every-tick loop.
func TestNudgeStalledPoolClaims_GivesUpAtCap(t *testing.T) {
	sp := runningFake(t)
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := idleClaimPoolSession()
	s.Metadata[idleClaimNudgeTriggerKey] = "w-1"
	s.Metadata[idleClaimNudgeCountKey] = strconv.Itoa(idleClaimNudgeMaxAttempts)
	s.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	sessions := []beads.Bead{s}
	work := []beads.Bead{{ID: "w-1", Status: "open"}}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, sessions, work, base.Add(time.Hour), &out)
	if out.Len() != 0 {
		t.Fatalf("must not nudge past the attempt cap: %q", out.String())
	}
}

// A non-pool session is ignored entirely.
func TestNudgeStalledPoolClaims_SkipsNonPool(t *testing.T) {
	sp := runningFake(t)
	cfg := idleClaimTestCfg()
	s := idleClaimPoolSession()
	delete(s.Metadata, "pool_managed")
	sessions := []beads.Bead{s}
	work := []beads.Bead{{ID: "w-1", Status: "open"}}
	store := beads.SessionStore{Store: beads.NewMemStoreFrom(0, sessions, nil)}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, sessions, work, base, &out)
	nudgeStalledPoolClaims(sp, cfg, store, sessions, work, base.Add(time.Hour), &out)
	if out.Len() != 0 {
		t.Fatalf("must not touch a non-pool session: %q", out.String())
	}
}
