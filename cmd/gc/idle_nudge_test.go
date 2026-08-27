package main

import (
	"bytes"
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

const (
	testTriggerBeadIDKey       = "gc.trigger_bead_id"
	testTriggerBeadStoreRefKey = "gc.trigger_bead_store_ref"
)

func idleClaimTestCfg() *config.City {
	return &config.City{Agents: []config.Agent{{
		Name:  "agent-a",
		Nudge: "Run gc hook --claim --json now; if it returns work, execute the claimed formula immediately.",
	}}}
}

func idleClaimPoolSession() beads.Bead {
	return beads.Bead{
		ID:     "session-bead-a",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"session_name":       "session-a",
			"pool_managed":       "true",
			"template":           "agent-a",
			testTriggerBeadIDKey: "work-a",
		},
	}
}

//nolint:unparam // sessionName is always "session-a" today; kept as a param so new cases can vary it.
func runningIdleClaimFake(t *testing.T, sessionName string) *runtime.Fake {
	t.Helper()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("fake start: %v", err)
	}
	return sp
}

func mustGetTestBead(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", id, err)
	}
	return b
}

func TestNudgeStalledPoolClaims_NudgesAfterGrace(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("first tick Nudge calls = %d, want 0 inside grace", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeTriggerKey]; got != "work-a" {
		t.Fatalf("idle claim marker trigger = %q, want work-a", got)
	}

	clk.Advance(idleClaimNudgeGrace + time.Second)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 1 {
		t.Fatalf("Nudge calls = %d, want 1 after grace", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("idle claim attempt count = %q, want 1", got)
	}
	if got := session.Metadata[idleClaimNudgeAtKey]; got != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("idle claim last nudge at = %q, want %q", got, clk.Now().UTC().Format(time.RFC3339))
	}
}

// Two stores can hold beads with the same ID, so the backstop must resolve the
// slot's trigger through the store ref it was bound to. Here the rig-scoped
// copy is still open (nudge-worthy) while the city-scoped copy of the same ID
// is closed; keying on ID alone would read the wrong bead and stay silent.
func TestNudgeStalledPoolClaims_MatchesTriggerStoreRefForDuplicateIDs(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata[testTriggerBeadStoreRefKey] = "rig:fixture"
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = "0"
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	work := []beads.Bead{
		{ID: "work-a", Status: "open"},
		{ID: "work-a", Status: "closed"},
	}
	storeRefs := []string{"rig:fixture", "city:test-city"}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: base.Add(idleClaimNudgeGrace + time.Second)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, storeRefs, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 1 {
		t.Fatalf("Nudge calls = %d, want 1 for the open rig-scoped trigger", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("idle claim attempt count = %q, want 1", got)
	}
}

func TestNudgeStalledPoolClaims_NeverTouchesWorkingSlot(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = "1"
	session.Metadata[idleClaimNudgeAtKey] = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "in_progress", Assignee: "session-a"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 1, 0, 0, 0, time.UTC)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("working slot Nudge calls = %d, want 0", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeTriggerKey]; got != "" {
		t.Fatalf("idle claim marker trigger = %q, want cleared", got)
	}
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "" {
		t.Fatalf("idle claim marker count = %q, want cleared", got)
	}
	if got := session.Metadata[idleClaimNudgeAtKey]; got != "" {
		t.Fatalf("idle claim marker at = %q, want cleared", got)
	}
}

func TestNudgeStalledPoolClaims_GivesUpAtCap(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = strconv.Itoa(idleClaimNudgeMaxAttempts)
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: base.Add(time.Hour)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("Nudge calls past cap = %d, want 0", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != strconv.Itoa(idleClaimNudgeMaxAttempts) {
		t.Fatalf("idle claim attempt count = %q, want cap preserved", got)
	}
}

// The attempt is reserved on the session bead BEFORE delivery, so a nudge the
// provider fails to deliver still consumes one of the bounded attempts. That is
// what stops a slot whose provider is wedged from being re-nudged on every tick
// forever; the cost is that transient delivery failures burn the cap. The
// failing-provider fixture is continuationFailingNudgeProvider
// (continuation_nudge_test.go), shared across both backstop lanes.
func TestNudgeStalledPoolClaims_DeliveryFailureConsumesAttempt(t *testing.T) {
	sp := &continuationFailingNudgeProvider{Provider: runningIdleClaimFake(t, "session-a")}
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = "0"
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: base.Add(idleClaimNudgeGrace + time.Second)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
	if sp.nudgeCalls != 1 {
		t.Fatalf("delivery calls = %d, want 1 failed attempt", sp.nudgeCalls)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("persisted attempt count = %q, want 1 despite delivery failure", got)
	}
	if got := session.Metadata[idleClaimNudgeAtKey]; got != clk.Now().UTC().Format(time.RFC3339) {
		t.Fatalf("persisted attempt time = %q, want %q", got, clk.Now().UTC().Format(time.RFC3339))
	}

	// The reservation paces the next retry exactly as a delivered nudge would:
	// nothing more is attempted until the backoff elapses.
	clk.Advance(idleClaimNudgeBackoff - time.Second)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
	if sp.nudgeCalls != 1 {
		t.Fatalf("inside-backoff delivery calls = %d, want unchanged 1", sp.nudgeCalls)
	}

	for want := 2; want <= idleClaimNudgeMaxAttempts; want++ {
		session = mustGetTestBead(t, store, session.ID)
		clk.Advance(idleClaimNudgeBackoff + time.Second)
		nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
		if sp.nudgeCalls != want {
			t.Fatalf("attempt %d delivery calls = %d, want %d", want, sp.nudgeCalls, want)
		}
	}

	// Every attempt failed, so exhausted() is reached without the trigger ever
	// being claimed: the lane stops attempting and leaves the cap in place.
	session = mustGetTestBead(t, store, session.ID)
	clk.Advance(time.Hour)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
	if sp.nudgeCalls != idleClaimNudgeMaxAttempts {
		t.Fatalf("past-cap delivery calls = %d, want %d", sp.nudgeCalls, idleClaimNudgeMaxAttempts)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != strconv.Itoa(idleClaimNudgeMaxAttempts) {
		t.Fatalf("persisted attempt count = %q, want cap %d preserved", got, idleClaimNudgeMaxAttempts)
	}
}

// fencedNudgeProvider refuses every delivery with runtime.ErrInputFenced, the
// contract a runtime uses to say "I proved your input never reached the pane".
type fencedNudgeProvider struct {
	runtime.Provider
	nudgeCalls int
}

func (p *fencedNudgeProvider) Nudge(string, []runtime.ContentBlock) error {
	p.nudgeCalls++
	return fmt.Errorf("%w: tmux session is parked in copy mode", runtime.ErrInputFenced)
}

// A fenced refusal is proof that NO input reached the session, so it must not
// burn one of the bounded attempts — otherwise a session parked in copy mode
// (or behind any other input fence) silently exhausts its backstop in three
// ticks and is never nudged again once the fence clears. The queued-nudge lane
// already treats runtime.ErrInputFenced this way (cmd_nudge.go releases the
// claim instead of recording a failure); this pins the same rule for the
// backstop engine. Its control is
// TestNudgeStalledPoolClaims_DeliveryFailureConsumesAttempt: a plain delivery
// error, which may have partially landed, still consumes an attempt and still
// reaches the cap.
func TestNudgeStalledPoolClaims_FencedRefusalNeverConsumesAttempt(t *testing.T) {
	sp := &fencedNudgeProvider{Provider: runningIdleClaimFake(t, "session-a")}
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = "0"
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: base.Add(idleClaimNudgeGrace + time.Second)}
	var out bytes.Buffer

	// Refuse far more times than the cap: a fence that never consumes an
	// attempt can never exhaust the backstop.
	rounds := idleClaimNudgeMaxAttempts + 3
	for round := 1; round <= rounds; round++ {
		nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
		if sp.nudgeCalls != round {
			t.Fatalf("round %d delivery calls = %d, want %d: the fence exhausted the backstop", round, sp.nudgeCalls, round)
		}
		session = mustGetTestBead(t, store, session.ID)
		if got := session.Metadata[idleClaimNudgeCountKey]; got != "0" {
			t.Fatalf("round %d persisted attempt count = %q, want 0: a fenced refusal consumed an attempt", round, got)
		}
		if got := session.Metadata[idleClaimNudgeAtKey]; got != clk.Now().UTC().Format(time.RFC3339) {
			t.Fatalf("round %d persisted attempt time = %q, want %q so refusals stay paced", round, got, clk.Now().UTC().Format(time.RFC3339))
		}
		// Pacing survives the rollback: nothing is retried until the clock moves.
		nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
		if sp.nudgeCalls != round {
			t.Fatalf("round %d re-tick delivery calls = %d, want unchanged %d", round, sp.nudgeCalls, round)
		}
		clk.Advance(idleClaimNudgeGrace + time.Second)
		session = mustGetTestBead(t, store, session.ID)
	}

	// A parked session must be diagnosable from the controller log by a named
	// reason, not just "failed".
	log := out.String()
	if !strings.Contains(log, "input fenced") || !strings.Contains(log, "attempt not consumed") {
		t.Fatalf("controller log = %q, want a distinct fenced-refusal reason", log)
	}
	if !strings.Contains(log, "copy mode") {
		t.Fatalf("controller log = %q, want the runtime's named fence reason carried through", log)
	}

	// Once the fence clears, the very next delivery is attempt 1 of 3 — the
	// backstop still has its full budget.
	live := runningIdleClaimFake(t, "session-a")
	nudgeStalledPoolClaims(live, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
	if got := live.CountCalls("Nudge", "session-a"); got != 1 {
		t.Fatalf("post-fence Nudge calls = %d, want 1", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "1" {
		t.Fatalf("post-fence attempt count = %q, want 1", got)
	}
}

func TestNudgeStalledPoolClaims_SkipsNonPool(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	delete(session.Metadata, "pool_managed")
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	clk := &clock.Fake{Time: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	var out bytes.Buffer

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
	clk.Advance(time.Hour)
	session = mustGetTestBead(t, store, session.ID)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("non-pool Nudge calls = %d, want 0", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeTriggerKey]; got != "" {
		t.Fatalf("non-pool marker trigger = %q, want empty", got)
	}
}
