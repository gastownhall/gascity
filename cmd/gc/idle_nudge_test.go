package main

import (
	"bytes"
	"context"
	"strconv"
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

func TestNudgeStalledPoolClaims_UsesClaimFallbackWhenNudgeIsBlank(t *testing.T) {
	for _, tc := range []struct {
		name  string
		nudge string
	}{
		{name: "empty", nudge: ""},
		{name: "whitespace", nudge: " \t "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := runningIdleClaimFake(t, "session-a")
			cfg := idleClaimTestCfg()
			cfg.Agents[0].Nudge = tc.nudge
			session := idleClaimPoolSession()
			work := []beads.Bead{{ID: "work-a", Status: "open"}}
			store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
			base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var out bytes.Buffer

			nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, base, &out)
			nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, base.Add(idleClaimNudgeGrace+time.Second), &out)

			for _, call := range sp.SnapshotCalls() {
				if call.Method == "Nudge" && call.Name == "session-a" {
					if got, want := call.Message, defaultPoolClaimNudge; got != want {
						t.Fatalf("fallback nudge = %q, want %q", got, want)
					}
					return
				}
			}
			t.Fatal("fallback nudge was not delivered")
		})
	}
}

func TestPoolClaimBackstopContent_PreservesExplicitNudge(t *testing.T) {
	cfg := idleClaimTestCfg()
	cfg.Agents[0].Nudge = "  Use the configured wake text exactly.  "

	if got, want := (poolClaimBackstop{cfg: cfg}).content(idleClaimPoolSession()), "Use the configured wake text exactly."; got != want {
		t.Fatalf("claim nudge = %q, want normalized configured value %q", got, want)
	}
}

func TestPoolClaimBackstopContent_FailsClosedForUnknownTemplate(t *testing.T) {
	cfg := idleClaimTestCfg()
	session := idleClaimPoolSession()
	session.Metadata["template"] = "unknown-agent"

	if got := (poolClaimBackstop{cfg: cfg}).content(session); got != "" {
		t.Fatalf("claim nudge for unknown template = %q, want empty", got)
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

// A seat that let its claim window lapse is latched by incarnation: once the
// last nudge has had its full backoff and the trigger is still unclaimed, the
// backstop records the incarnation's instance token, which pool accounting reads
// to stop counting the seat as capacity for new demand (ga-2m9dwp).
func TestNudgeStalledPoolClaims_LatchesTheIncarnationThatNeverClaimed(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata["instance_token"] = "tok-1"
	session.Metadata["creation_complete_at"] = base.Add(-time.Hour).Format(time.RFC3339)
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = strconv.Itoa(idleClaimNudgeMaxAttempts)
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	var out bytes.Buffer

	// The last nudge is still inside its backoff: it may yet land.
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, base.Add(idleClaimNudgeBackoff-time.Second), &out)
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeLapsedTokenKey]; got != "" {
		t.Fatalf("lapsed latch inside the last backoff = %q, want empty", got)
	}

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, base.Add(idleClaimNudgeBackoff), &out)
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeLapsedTokenKey]; got != "tok-1" {
		t.Fatalf("lapsed latch = %q, want the incarnation's instance token tok-1", got)
	}
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("Nudge calls past the cap = %d, want 0", got)
	}
	if !poolSeatClaimLapsedInfo(sessionInfosFromBeads([]beads.Bead{session})[0]) {
		t.Fatal("latched incarnation does not read as a lapsed seat")
	}

	// A later incarnation of the same seat starts with a clean slate: the
	// latch names a token it does not hold.
	session.Metadata["instance_token"] = "tok-2"
	if poolSeatClaimLapsedInfo(sessionInfosFromBeads([]beads.Bead{session})[0]) {
		t.Fatal("a new incarnation inherited the previous incarnation's lapse")
	}
}

// The latch is evidence against one incarnation nudged to the cap: a seat with
// no instance token or start stamp has no incarnation to latch, and a seat the
// engine gave up on without nudging (no nudge text for its template) was never
// handed the claim. Either keeps counting as capacity, today's behavior.
func TestNudgeStalledPoolClaims_LapseNeedsAnIncarnationNudgedToTheCap(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	started := base.Add(-time.Hour).Format(time.RFC3339)
	for _, tc := range []struct {
		name     string
		template string
		count    int
		meta     map[string]string
	}{
		{name: "no instance token", template: "agent-a", count: idleClaimNudgeMaxAttempts, meta: map[string]string{"creation_complete_at": started}},
		{name: "no start stamp", template: "agent-a", count: idleClaimNudgeMaxAttempts, meta: map[string]string{"instance_token": "tok-1"}},
		{name: "never nudged", template: "agent-unknown", count: 0, meta: map[string]string{"instance_token": "tok-1", "creation_complete_at": started}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := runningIdleClaimFake(t, "session-a")
			cfg := idleClaimTestCfg()
			session := idleClaimPoolSession()
			session.Metadata["template"] = tc.template
			session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
			session.Metadata[idleClaimNudgeCountKey] = strconv.Itoa(tc.count)
			session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
			for k, v := range tc.meta {
				session.Metadata[k] = v
			}
			work := []beads.Bead{{ID: "work-a", Status: "open"}}
			store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
			var out bytes.Buffer

			nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, base.Add(time.Hour), &out)
			session = mustGetTestBead(t, store, session.ID)
			if got := session.Metadata[idleClaimNudgeLapsedTokenKey]; got != "" {
				t.Fatalf("lapsed latch = %q, want empty", got)
			}
		})
	}
}

// The attempt markers outlive a restart. A seat restarted onto the same
// unclaimed trigger is observed afresh, before its start even commits (the wake
// stamps last_woke_at with the new token; creation_complete_at still names the
// predecessor's start): it is neither latched by its predecessor's spent cap nor
// left without nudges, and it gets its own grace, nudges and give-up.
func TestNudgeStalledPoolClaims_RestartedSeatGetsItsOwnClaimWindow(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-a")
	cfg := idleClaimTestCfg()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	session.Metadata[idleClaimNudgeCountKey] = strconv.Itoa(idleClaimNudgeMaxAttempts)
	session.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	session.Metadata["creation_complete_at"] = base.Add(-time.Hour).Format(time.RFC3339)
	session.Metadata["instance_token"] = "tok-2"
	session.Metadata["last_woke_at"] = base.Add(5 * time.Minute).Format(time.RFC3339)
	work := []beads.Bead{{ID: "work-a", Status: "open"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	var out bytes.Buffer

	observed := base.Add(10 * time.Minute)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, observed, &out)
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeLapsedTokenKey]; got != "" {
		t.Fatalf("restarted seat latched by its predecessor's attempts: %q", got)
	}
	if got := session.Metadata[idleClaimNudgeCountKey]; got != "0" {
		t.Fatalf("restarted seat attempt count = %q, want the cycle re-observed at 0", got)
	}
	if got := session.Metadata[idleClaimNudgeAtKey]; got != observed.Format(time.RFC3339) {
		t.Fatalf("restarted seat observed at %q, want %q", got, observed.Format(time.RFC3339))
	}
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("Nudge calls inside the restarted seat's grace = %d, want 0", got)
	}

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, observed.Add(idleClaimNudgeGrace+time.Second), &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 1 {
		t.Fatalf("Nudge calls after the restarted seat's grace = %d, want 1", got)
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
