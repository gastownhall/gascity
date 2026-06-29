package runproj

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestEnrichRunSummaryGolden pins the Go EnrichRunSummary port to the TypeScript
// oracle: it folds the shared bead fixture into a bead-derived summary, advances
// the cold marks, enriches it against the shared sessions fixture at the same
// fixed generation time the generator used, and asserts the canonical JSON
// matches runsummary_enriched_golden.json byte-for-byte.
func TestEnrichRunSummaryGolden(t *testing.T) {
	beadList := loadFixtureBeads(t)
	sessions := loadFixtureSessions(t)

	base := BuildRunSummary(beadList)

	inFlight := make([]RunLane, 0, len(base.Lanes)+len(base.BlockedLanes))
	inFlight = append(inFlight, base.Lanes...)
	inFlight = append(inFlight, base.BlockedLanes...)
	marks := AdvanceProgressMarks(nil, inFlight)

	// Must equal ENRICHED_GENERATION_MS in gen-run-goldens.mts.
	nowMs := mustMillis(t, "2026-06-09T00:00:00Z")
	enriched := EnrichRunSummary(base, sessions, true, nowMs, marks)

	got, err := canonicalJSON(enriched)
	if err != nil {
		t.Fatalf("marshal enriched summary: %v", err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "runsummary_enriched_golden.json"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("enriched run summary does not match golden:\n%s", unifiedDiff(string(want), string(got)))
	}
}

// TestDeriveRunHealthSessionUnavailability ports health.test.ts (gascity-dashboard
// 0gww): without the session list every lane's health collapses to unavailable;
// with it available, health derives.
func TestDeriveRunHealthSessionUnavailability(t *testing.T) {
	mk := func(id string) RunLane {
		return RunLane{
			ID:                   id,
			Phase:                "implementation",
			ActiveAssignees:      []string{"app/codex"},
			UpdatedAt:            RunLaneUpdatedAt{Status: "available", At: "2026-06-08T00:00:00Z"},
			Progress:             RunLaneProgress{Status: "unavailable", Error: "run progress unavailable"},
			FormulaStageResolved: false,
			Health:               runHealthUnavailable(),
		}
	}

	t.Run("unavailable session list ⇒ every lane health unavailable", func(t *testing.T) {
		lanes := deriveRunHealthLanes([]RunLane{mk("run-a"), mk("run-b")}, nil, false, nil)
		for _, l := range lanes {
			if l.Health.Status != "unavailable" {
				t.Errorf("lane %s: health.status = %q, want unavailable", l.ID, l.Health.Status)
			}
			if l.Health.Error != "run session list unavailable" {
				t.Errorf("lane %s: health.error = %q", l.ID, l.Health.Error)
			}
		}
	})

	t.Run("available session list ⇒ health derives", func(t *testing.T) {
		lanes := deriveRunHealthLanes([]RunLane{mk("run-a")}, nil, true, nil)
		if lanes[0].Health.Status != "available" {
			t.Errorf("health.status = %q, want available", lanes[0].Health.Status)
		}
	})
}

// TestIsStaleSessionlessLatch ports liveness.test.ts (gascity-dashboard-s4rp):
// the sharp session-less demotion predicate.
func TestIsStaleSessionlessLatch(t *testing.T) {
	nowMs := mustMillis(t, "2026-06-07T00:00:00Z")

	at := func(deltaMs int64) RunLaneUpdatedAt {
		return RunLaneUpdatedAt{Status: "available", At: time.UnixMilli(nowMs - deltaMs).UTC().Format(time.RFC3339)}
	}
	health := func(session string) RunLaneHealthState {
		sess := RunLaneSessionState{Status: "unresolved", Error: "run session unresolved"}
		if session == "resolved" {
			sess = RunLaneSessionState{
				Status:     "resolved",
				LastActive: RunLaneSessionLastActive{Status: "available", At: "2026-06-07T00:00:00Z"},
				Running:    RunLaneSessionRunning{Status: "available", Value: true},
				Activity:   RunLaneSessionActivity{Status: "available", Value: "working"},
			}
		}
		return RunLaneHealthState{Status: "available", Data: RunLaneHealth{
			PhaseConfidence:   "inferred",
			StuckNode:         RunLaneStuckNode{Status: "unavailable", Error: "active run step unavailable"},
			ThrashingDetected: false,
			Session:           sess,
		}}
	}
	// The gc-1920 baseline: approval-gate latch, unresolved session, no active
	// step, ~4 days stale.
	base := RunLane{
		ID:              "gc-1920",
		Phase:           "approval",
		ActiveAssignees: []string{},
		UpdatedAt:       at(4 * 24 * 60 * 60 * 1000),
		Progress:        RunLaneProgress{Status: "unavailable", Error: "run progress unavailable"},
		Health:          health("unresolved"),
	}
	withProgress := base
	withProgress.Progress = RunLaneProgress{
		Status:  "active_step",
		StepID:  "implementation.patch",
		Stage:   RunLaneStagePosition{Status: "unavailable", Error: "active run stage unavailable"},
		Attempt: RunLaneStepAttempt{Status: "unavailable", Error: "run step attempt unavailable"},
	}

	clone := func(mut func(*RunLane)) RunLane {
		l := base
		mut(&l)
		return l
	}

	cases := []struct {
		name              string
		lane              RunLane
		sessionsAvailable bool
		want              bool
	}{
		{"demotes stale gc-1920 latch", base, true, true},
		{"keeps freshly-queued recent run", clone(func(l *RunLane) { l.Phase = "intake"; l.UpdatedAt = at(60_000) }), true, false},
		{"keeps recent approval gate", clone(func(l *RunLane) { l.UpdatedAt = at(30 * 60_000) }), true, false},
		{"keeps stale run with resolved session", clone(func(l *RunLane) { l.Health = health("resolved") }), true, false},
		{"keeps stale run with in_progress step", withProgress, true, false},
		{"no demotion when session list unavailable", base, false, false},
		{"never demotes complete", clone(func(l *RunLane) { l.Phase = "complete" }), true, false},
		{"never demotes blocked", clone(func(l *RunLane) { l.Phase = "blocked" }), true, false},
		{"no demotion without known age", clone(func(l *RunLane) {
			l.UpdatedAt = RunLaneUpdatedAt{Status: "unavailable", Error: "run update time unavailable"}
		}), true, false},
		{"boundary just under floor stays", clone(func(l *RunLane) { l.UpdatedAt = at(staleLatchAfterMs - 1_000) }), true, false},
		{"boundary at floor demotes", clone(func(l *RunLane) { l.UpdatedAt = at(staleLatchAfterMs) }), true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isStaleSessionlessLatch(tc.lane, nowMs, tc.sessionsAvailable); got != tc.want {
				t.Errorf("isStaleSessionlessLatch = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestAdvanceProgressMarksThrash exercises the cross-generation monotonicity the
// golden cannot reach (one snapshot ⇒ all streaks 0): a lane whose graph position
// stays flat while the active step's attempt climbs accrues a thrash streak, and
// the census counts it as thrashing only once the streak crosses the threshold
// AND the lane is phaseConfidence 'known'.
func TestAdvanceProgressMarksThrash(t *testing.T) {
	mkLane := func(attempt int) RunLane {
		return RunLane{
			ID:                   "run-thrash",
			Phase:                "review",
			ActiveAssignees:      []string{"pool-x"},
			FormulaStageResolved: true,
			UpdatedAt:            RunLaneUpdatedAt{Status: "available", At: "2026-06-08T00:00:00Z"},
			Progress: RunLaneProgress{
				Status:  "active_step",
				StepID:  "review-loop",
				Stage:   RunLaneStagePosition{Status: "available", Index: 1, Key: "review", Label: "Review"},
				Attempt: RunLaneStepAttempt{Status: "available", Value: attempt},
			},
			Health: runHealthUnavailable(),
		}
	}
	session := DashboardSession{ID: "s1", SessionName: "x", State: "active", Provider: "claude", Running: true}
	session.Alias = ptr("pool-x")
	sessions := []DashboardSession{session}

	// Generation 1: attempt 1, no prior marks ⇒ streak 0.
	g1 := []RunLane{mkLane(1)}
	marks := AdvanceProgressMarks(nil, g1)
	if marks["run-thrash"].ThrashStreak != 0 {
		t.Fatalf("gen1 streak = %d, want 0", marks["run-thrash"].ThrashStreak)
	}

	// Generation 2: same stage/step, attempt climbed to 2 ⇒ streak 1.
	g2 := []RunLane{mkLane(2)}
	marks = AdvanceProgressMarks(marks, g2)
	if marks["run-thrash"].ThrashStreak != 1 {
		t.Fatalf("gen2 streak = %d, want 1", marks["run-thrash"].ThrashStreak)
	}

	// Generation 3: attempt climbed to 3 ⇒ streak 2 ⇒ thrashingDetected.
	g3 := []RunLane{mkLane(3)}
	marks = AdvanceProgressMarks(marks, g3)
	if marks["run-thrash"].ThrashStreak != 2 {
		t.Fatalf("gen3 streak = %d, want 2", marks["run-thrash"].ThrashStreak)
	}

	lanes := deriveRunHealthLanes(g3, sessions, true, marks)
	h := lanes[0].Health
	if h.Status != "available" || !h.Data.ThrashingDetected {
		t.Fatalf("expected thrashingDetected with known confidence, got %+v", h)
	}
	if h.Data.PhaseConfidence != "known" {
		t.Fatalf("phaseConfidence = %q, want known", h.Data.PhaseConfidence)
	}
	census := buildCensus(lanes)
	if census.Thrashing != 1 || census.KnownDenominator != 1 {
		t.Fatalf("census thrashing=%d knownDenominator=%d, want 1/1", census.Thrashing, census.KnownDenominator)
	}

	// A flat attempt resets the streak to 0 (progress stalled, not thrashing).
	marks = AdvanceProgressMarks(marks, []RunLane{mkLane(3)})
	if marks["run-thrash"].ThrashStreak != 0 {
		t.Fatalf("flat-attempt streak = %d, want 0", marks["run-thrash"].ThrashStreak)
	}
}

func loadFixtureBeads(t *testing.T) []beads.Bead {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "beads_fixture.json"))
	if err != nil {
		t.Fatalf("read bead fixture: %v", err)
	}
	var beadList []beads.Bead
	if err := json.Unmarshal(raw, &beadList); err != nil {
		t.Fatalf("unmarshal bead fixture: %v", err)
	}
	return beadList
}

func loadFixtureSessions(t *testing.T) []DashboardSession {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "sessions_fixture.json"))
	if err != nil {
		t.Fatalf("read sessions fixture: %v", err)
	}
	var sessions []DashboardSession
	if err := json.Unmarshal(raw, &sessions); err != nil {
		t.Fatalf("unmarshal sessions fixture: %v", err)
	}
	return sessions
}

func mustMillis(t *testing.T, value string) int64 {
	t.Helper()
	ms, ok := millisFromTimestamp(value)
	if !ok {
		t.Fatalf("parse %q failed", value)
	}
	return ms
}

func ptr(s string) *string { return &s }
