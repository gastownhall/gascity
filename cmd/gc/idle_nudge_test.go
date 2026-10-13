package main

import (
	"bytes"
	"context"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
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

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), poolClaimDrain{}, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("first tick Nudge calls = %d, want 0 inside grace", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeTriggerKey]; got != "work-a" {
		t.Fatalf("idle claim marker trigger = %q, want work-a", got)
	}

	clk.Advance(idleClaimNudgeGrace + time.Second)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), poolClaimDrain{}, &out)
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

			nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, base, poolClaimDrain{}, &out)
			nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, base.Add(idleClaimNudgeGrace+time.Second), poolClaimDrain{}, &out)

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

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, storeRefs, clk.Now(), poolClaimDrain{}, &out)
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

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), poolClaimDrain{}, &out)
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

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), poolClaimDrain{}, &out)
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

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), poolClaimDrain{}, &out)
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
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), poolClaimDrain{}, &out)
	if sp.nudgeCalls != 1 {
		t.Fatalf("inside-backoff delivery calls = %d, want unchanged 1", sp.nudgeCalls)
	}

	for want := 2; want <= idleClaimNudgeMaxAttempts; want++ {
		session = mustGetTestBead(t, store, session.ID)
		clk.Advance(idleClaimNudgeBackoff + time.Second)
		nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), poolClaimDrain{}, &out)
		if sp.nudgeCalls != want {
			t.Fatalf("attempt %d delivery calls = %d, want %d", want, sp.nudgeCalls, want)
		}
	}

	// Every attempt failed, so exhausted() is reached without the trigger ever
	// being claimed: the lane stops attempting and leaves the cap in place.
	session = mustGetTestBead(t, store, session.ID)
	clk.Advance(time.Hour)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), poolClaimDrain{}, &out)
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

	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), poolClaimDrain{}, &out)
	clk.Advance(time.Hour)
	session = mustGetTestBead(t, store, session.ID)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, work, nil, clk.Now(), poolClaimDrain{}, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("non-pool Nudge calls = %d, want 0", got)
	}
	session = mustGetTestBead(t, store, session.ID)
	if got := session.Metadata[idleClaimNudgeTriggerKey]; got != "" {
		t.Fatalf("non-pool marker trigger = %q, want empty", got)
	}
}

// drainRecorder records the drain requests the claim backstop makes.
type drainRecorder struct{ ids []string }

func (d *drainRecorder) request(b beads.Bead) error {
	d.ids = append(d.ids, b.ID)
	return nil
}

// liveClaimDrain is a terminal drain that reads triggers live from store and
// requests through request.
func liveClaimDrain(store beads.Store, request func(beads.Bead) error) poolClaimDrain {
	return poolClaimDrain{
		readTrigger: func(id string) (beads.Bead, bool, error) {
			b, err := store.Get(id)
			if err != nil {
				return beads.Bead{}, false, err
			}
			return b, true, nil
		},
		request: request,
	}
}

// quietClaimFake is the running slot session-a, its agent quiet for an hour
// before at.
func quietClaimFake(t *testing.T, at time.Time) *runtime.Fake {
	t.Helper()
	sp := runningIdleClaimFake(t, "session-a")
	sp.SetActivity("session-a", at.Add(-time.Hour))
	return sp
}

// atCapSession is the pool slot at the nudge cap for work-a, at incarnation
// token.
func atCapSession(base time.Time, token string) beads.Bead {
	s := idleClaimPoolSession()
	s.Metadata[idleClaimNudgeTriggerKey] = "work-a"
	s.Metadata[idleClaimNudgeCountKey] = strconv.Itoa(idleClaimNudgeMaxAttempts)
	s.Metadata[idleClaimNudgeAtKey] = base.Format(time.RFC3339)
	s.Metadata[idleClaimNudgeTokenKey] = token
	s.Metadata["instance_token"] = token
	return s
}

// Kills a wedged slot holding its pool seat forever (mc-zndi7.80): once the
// bounded nudges are spent, the slot is quiet, and its own trigger, re-read
// live, is still open and unassigned, the backstop requests the slot's drain
// once for its incarnation, delivering no more nudges; the latch holds on the
// next ticks and re-arms after the retry window.
func TestNudgeStalledPoolClaims_DrainsAWedgedSlotAtCap(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(idleClaimNudgeBackoff + time.Second)
	sp := quietClaimFake(t, now)
	session := atCapSession(base, "tok-1")
	store := beads.NewMemStoreFrom(0, []beads.Bead{session, {ID: "work-a", Status: "open"}}, nil)
	open := []beads.Bead{{ID: "work-a", Status: "open"}}
	drains := &drainRecorder{}
	var out bytes.Buffer
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{session}, open, nil, now, liveClaimDrain(store, drains.request), &out)
	session = mustGetTestBead(t, store, session.ID)
	if len(drains.ids) != 1 || drains.ids[0] != session.ID || sp.CountCalls("Nudge", "session-a") != 0 {
		t.Fatalf("drains %v nudges %d, want one drain and no nudge", drains.ids, sp.CountCalls("Nudge", "session-a"))
	}
	if session.Metadata[idleClaimNudgeStalledTokenKey] != "tok-1" || session.Metadata[idleClaimNudgeStalledKey] != now.UTC().Format(time.RFC3339) ||
		session.Metadata[idleClaimNudgeCountKey] != strconv.Itoa(idleClaimNudgeMaxAttempts) || !strings.Contains(out.String(), "draining (execution-stalled)") {
		t.Fatalf("metadata %v log %q, want the latch for tok-1, the cap kept, and the drain logged", session.Metadata, out.String())
	}
	sp.SetActivity("session-a", now)
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{session}, open, nil, now.Add(5*time.Minute), liveClaimDrain(store, drains.request), &out)
	if len(drains.ids) != 1 {
		t.Fatalf("drains %v, want the latch to hold", drains.ids)
	}
	later := now.Add(executionStalledLatchRetryAfter + time.Second)
	sp.SetActivity("session-a", later.Add(-time.Hour))
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{mustGetTestBead(t, store, session.ID)}, open, nil, later, liveClaimDrain(store, drains.request), &out)
	if len(drains.ids) != 2 {
		t.Fatalf("drains %v, want a retry past the window", drains.ids)
	}
}

// Kills a re-woken incarnation drained on sight (#7752 review, probe P1): a
// slot restarted at a new incarnation for the same trigger, its marker at the
// cap and the old latch on it, gets its own grace and nudges before its own
// drain.
func TestNudgeStalledPoolClaims_NewIncarnationGetsItsOwnBudget(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := atCapSession(base, "tok-2") // the drained slot, woken again: PreWake minted tok-2
	s.Metadata[idleClaimNudgeStalledKey] = base.Add(time.Minute).Format(time.RFC3339)
	s.Metadata[idleClaimNudgeStalledTokenKey] = "tok-1"
	store := beads.NewMemStoreFrom(0, []beads.Bead{s, {ID: "work-a", Status: "open"}}, nil)
	open := []beads.Bead{{ID: "work-a", Status: "open"}}
	drains := &drainRecorder{}
	var out bytes.Buffer
	now := base.Add(10 * time.Minute)
	sp := quietClaimFake(t, now)
	tick := func(at time.Time) {
		sp.SetActivity("session-a", at.Add(-time.Hour))
		nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{mustGetTestBead(t, store, s.ID)}, open, nil, at, liveClaimDrain(store, drains.request), &out)
	}
	tick(now)
	if got := mustGetTestBead(t, store, s.ID); len(drains.ids) != 0 || got.Metadata[idleClaimNudgeCountKey] != "0" || got.Metadata[idleClaimNudgeStalledTokenKey] != "" {
		t.Fatalf("first tick: drains %v metadata %v, want a fresh observation with the old latch cleared", drains.ids, got.Metadata)
	}
	at := now.Add(idleClaimNudgeGrace + time.Second)
	for want := 1; want <= idleClaimNudgeMaxAttempts; want++ {
		tick(at)
		if got := sp.CountCalls("Nudge", "session-a"); got != want || len(drains.ids) != 0 {
			t.Fatalf("nudge %d: nudges %d drains %v, want its nudges before any drain", want, got, drains.ids)
		}
		at = at.Add(idleClaimNudgeBackoff + time.Second)
	}
	tick(at)
	if len(drains.ids) != 1 || mustGetTestBead(t, store, s.ID).Metadata[idleClaimNudgeStalledTokenKey] != "tok-2" {
		t.Fatalf("drains %v, want this incarnation drained once its own nudges were spent", drains.ids)
	}
}

// Kills a drain on a trigger the slot itself holds (#7752 review, probe P2):
// a continuation step pinned to the slot by its bead ID, or a trigger another
// session holds, is claimed, so the slot is neither nudged nor drained.
func TestNudgeStalledPoolClaims_AssignedTriggerIsClaimed(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(idleClaimNudgeBackoff + time.Second)
	for _, assignee := range []string{"session-bead-a", "session-a", "someone-else"} {
		s := atCapSession(base, "tok-1")
		pinned := beads.Bead{ID: "work-a", Status: "open", Assignee: assignee, Metadata: map[string]string{"gc.session_affinity": "require", "gc.continuation_group": "main"}}
		store := beads.NewMemStoreFrom(0, []beads.Bead{s, pinned}, nil)
		sp := quietClaimFake(t, now)
		drains := &drainRecorder{}
		var out bytes.Buffer
		nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{s}, []beads.Bead{pinned}, nil, now, liveClaimDrain(store, drains.request), &out)
		if len(drains.ids) != 0 || sp.CountCalls("Nudge", "session-a") != 0 {
			t.Fatalf("assigned to %q: drains %v nudges %d, want neither", assignee, drains.ids, sp.CountCalls("Nudge", "session-a"))
		}
	}
}

// Kills a drain on stale evidence (#7752 review, probe P3): a trigger the
// slot claimed after the tick's snapshot, read live, stops the drain; so
// does a trigger the live read cannot find.
func TestNudgeStalledPoolClaims_ClaimedSinceTheSnapshotIsNotDrained(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(idleClaimNudgeBackoff + time.Second)
	snapshot := []beads.Bead{{ID: "work-a", Status: "open"}}
	for name, live := range map[string][]beads.Bead{
		"claimed since":  {{ID: "work-a", Status: "in_progress", Assignee: "session-a"}},
		"no longer read": nil,
	} {
		s := atCapSession(base, "tok-1")
		store := beads.NewMemStoreFrom(0, append([]beads.Bead{s}, live...), nil)
		sp := quietClaimFake(t, now)
		drains := &drainRecorder{}
		var out bytes.Buffer
		nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{s}, snapshot, nil, now, liveClaimDrain(store, drains.request), &out)
		if len(drains.ids) != 0 || mustGetTestBead(t, store, s.ID).Metadata[idleClaimNudgeStalledKey] != "" {
			t.Fatalf("%s: drains %v, want no drain and no latch", name, drains.ids)
		}
	}
}

// Kills draining a busy agent (#7752 review): a slot at the cap whose agent
// is producing output, or whose activity cannot be read, is not drained.
func TestNudgeStalledPoolClaims_BusySlotIsNotDrained(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := base.Add(idleClaimNudgeBackoff + time.Second)
	for name, activity := range map[string]time.Time{"active a minute ago": now.Add(-time.Minute), "activity unread": {}} {
		s := atCapSession(base, "tok-1")
		store := beads.NewMemStoreFrom(0, []beads.Bead{s, {ID: "work-a", Status: "open"}}, nil)
		sp := runningIdleClaimFake(t, "session-a")
		if !activity.IsZero() {
			sp.SetActivity("session-a", activity)
		}
		drains := &drainRecorder{}
		var out bytes.Buffer
		nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{s}, []beads.Bead{{ID: "work-a", Status: "open"}}, nil, now, liveClaimDrain(store, drains.request), &out)
		if len(drains.ids) != 0 {
			t.Fatalf("%s: drains %v, want none for a slot not proven quiet", name, drains.ids)
		}
	}
}

// Kills a stale latch outliving its trigger (#7752 review): clearing the
// marker, once nothing is outstanding, clears the drain latch too.
func TestNudgeStalledPoolClaims_ClearRemovesTheLatch(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := atCapSession(base, "tok-1")
	s.Metadata[idleClaimNudgeStalledKey] = base.Format(time.RFC3339)
	s.Metadata[idleClaimNudgeStalledTokenKey] = "tok-1"
	store := beads.NewMemStoreFrom(0, []beads.Bead{s}, nil)
	var out bytes.Buffer
	nudgeStalledPoolClaims(runningIdleClaimFake(t, "session-a"), idleClaimTestCfg(), store, []beads.Bead{s}, nil, nil, base.Add(time.Hour), poolClaimDrain{}, &out)
	if got := mustGetTestBead(t, store, s.ID); got.Metadata[idleClaimNudgeStalledKey] != "" || got.Metadata[idleClaimNudgeStalledTokenKey] != "" || got.Metadata[idleClaimNudgeTriggerKey] != "" {
		t.Fatalf("metadata %v, want the marker and the latch cleared", got.Metadata)
	}
}

// Kills a slot whose template resolves to no agent parking silently forever
// (mc-zndi7.80): with no nudge to send it is drained once the grace passes.
func TestNudgeStalledPoolClaims_UnresolvedTemplateDrainsAfterGrace(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	session := idleClaimPoolSession()
	session.Metadata["template"] = "gone"
	session.Metadata["instance_token"] = "tok-1"
	store := beads.NewMemStoreFrom(0, []beads.Bead{session, {ID: "work-a", Status: "open"}}, nil)
	open := []beads.Bead{{ID: "work-a", Status: "open"}}
	drains := &drainRecorder{}
	var out bytes.Buffer
	at := base.Add(idleClaimNudgeGrace + time.Second)
	sp := quietClaimFake(t, at)
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{session}, open, nil, base, liveClaimDrain(store, drains.request), &out)
	if len(drains.ids) != 0 {
		t.Fatalf("drains %v at first sight, want the grace first", drains.ids)
	}
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{mustGetTestBead(t, store, session.ID)}, open, nil, at, liveClaimDrain(store, drains.request), &out)
	if len(drains.ids) != 1 || sp.CountCalls("Nudge", "session-a") != 0 {
		t.Fatalf("drains %v nudges %d, want a drain after the grace and no nudge", drains.ids, sp.CountCalls("Nudge", "session-a"))
	}
}

// Kills the production-dead backstop (mc-zndi7.80: build 79bd3633a1 nudged
// only agents that configure a nudge): a rig-scoped pool shaped like
// beads/claude-max-gw, no nudge configured, is nudged with the default
// claim nudge after the grace, its trigger a routed step in the city store.
func TestNudgeStalledPoolClaims_RigPoolWithoutNudgeGetsTheDefault(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "c"},
		Rigs:      []config.Rig{{Name: "beads", Path: t.TempDir()}},
		Agents:    []config.Agent{{Name: "claude-max-gw", Dir: "beads", MaxActiveSessions: intPtr(100)}},
	}
	sp := runningIdleClaimFake(t, "beads--claude-max-gw-2-pool")
	session := beads.Bead{ID: "gcg-session-2", Status: "open", Type: "session", Metadata: map[string]string{
		"session_name": "beads--claude-max-gw-2-pool", "pool_managed": "true", "pool_slot": "2", "template": "beads/claude-max-gw",
		"agent_name": "beads/claude-max-gw-2", testTriggerBeadIDKey: "gcg-step", beadmeta.TriggerBeadStoreRefMetadataKey: "city:c",
	}}
	step := beads.Bead{ID: "gcg-step", Status: "open", Metadata: map[string]string{
		"gc.routed_to": "beads/claude-max-gw", "gc.session_affinity": "require", "gc.continuation_group": "main",
	}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{session}, nil)
	var out bytes.Buffer
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{session}, []beads.Bead{step}, []string{"city:c"}, base, poolClaimDrain{}, &out)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{mustGetTestBead(t, store, session.ID)}, []beads.Bead{step}, []string{"city:c"}, base.Add(idleClaimNudgeGrace+time.Second), poolClaimDrain{}, &out)
	if got := sp.CountCalls("Nudge", "beads--claude-max-gw-2-pool"); got != 1 {
		t.Fatalf("nudges = %d (log %q), want the default claim nudge delivered", got, out.String())
	}
	if !strings.Contains(out.String(), "nudged beads--claude-max-gw-2-pool for gcg-step") {
		t.Fatalf("log %q, want the nudge logged", out.String())
	}
}

// wedgedSlotEnv is a pool of one whose live slot worker is bound to routed
// work it never claims, its agent quiet: the 2026-10-05 shape.
func wedgedSlotEnv(t *testing.T, assignee string) (*reconcilerTestEnv, beads.Bead, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1), StartCommand: "true"}}}
	env.addDesired("worker", "worker", false)
	session := env.createSessionBead("worker", "worker")
	if assignee == "self" {
		assignee = session.ID
	}
	work, err := env.store.Create(beads.Bead{Title: "step", Type: "task", Assignee: assignee, Metadata: map[string]string{
		"gc.routed_to": "worker", "gc.session_affinity": "require", "gc.continuation_group": "main",
	}})
	if err != nil {
		t.Fatal(err)
	}
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{"pool_managed": "true", testTriggerBeadIDKey: work.ID, "instance_token": "tok-1"})
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	return env, session, work
}

// runWedgedSlot runs the claim backstop and the legacy reconciler for ticks
// one-minute ticks, the agent quiet throughout, beginning each requested
// drain on the test clock; it reports the drains requested.
func runWedgedSlot(t *testing.T, env *reconcilerTestEnv, work beads.Bead, ticks int) int {
	t.Helper()
	drains := 0
	request := func(b beads.Bead) error {
		drains++
		beginSessionDrainInfo(sessiontest.SeedBead(t, b), env.sp, env.dt, executionStalledDrainReason, env.clk, defaultDrainTimeout)
		return nil
	}
	var out bytes.Buffer
	for tick := 0; tick < ticks; tick++ {
		env.sp.SetActivity("worker", env.clk.Now().Add(-time.Hour))
		raw, err := loadSessionBeads(env.store)
		if err != nil {
			t.Fatal(err)
		}
		w, _ := env.store.Get(work.ID)
		nudgeStalledPoolClaims(env.sp, env.cfg, beads.SessionStore{Store: env.store}, raw, []beads.Bead{w}, []string{"city"}, env.clk.Now(), liveClaimDrain(env.store, request), &out)
		raw, _ = loadSessionBeads(env.store)
		env.reconcile(raw)
		env.clk.Advance(time.Minute)
	}
	return drains
}

// Kills a wedged pool slot that is never recycled, or recycled in a loop
// (mc-zndi7.80, #7752 review probe P4c): a live slot bound to routed work
// whose agent never answers gets its three nudges, is drained once, and its
// runtime stops; it is not drained again.
func TestWedgedPoolSlotIsRecycledWhileRoutedWorkWaits(t *testing.T) {
	env, session, work := wedgedSlotEnv(t, "")
	drains := runWedgedSlot(t, env, work, 60)
	if got := env.sp.CountCalls("Nudge", "worker"); got != idleClaimNudgeMaxAttempts {
		t.Fatalf("nudges = %d, want %d before the slot is given up on", got, idleClaimNudgeMaxAttempts)
	}
	if drains != 1 || env.sp.IsRunning("worker") {
		cur, _ := env.store.Get(session.ID)
		t.Fatalf("drains %d, running %v (state %s/%s), want one drain and the runtime stopped", drains, env.sp.IsRunning("worker"), cur.Metadata["state"], cur.Metadata["sleep_reason"])
	}
}

// Kills the restart-and-drain loop (#7752 review probe P5): a slot whose
// trigger is pinned to it (a continuation step assigned to its bead ID) is
// never drained by the claim backstop; the pinned step is claimed, the
// continuation backstop's to pace.
func TestPinnedTriggerSlotIsNotDrainedInALoop(t *testing.T) {
	env, _, work := wedgedSlotEnv(t, "self")
	if drains := runWedgedSlot(t, env, work, 60); drains != 0 {
		t.Fatalf("drains %d, want none for a pinned trigger", drains)
	}
}

// Kills the legacy tick wiring the claim backstop's terminal drain to
// nothing (mc-zndi7.80): the controller's bead reconcile tick runs the
// backstop with its real drain request and live read, so a quiet live pool
// slot at the cap whose routed trigger is still open is draining
// (execution-stalled) after one tick.
func TestBeadReconcileTickDrainsAWedgedPoolSlot(t *testing.T) {
	store := beads.NewMemStore()
	work, err := store.Create(beads.Bead{Title: "step", Type: "task", Metadata: map[string]string{"gc.routed_to": "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: map[string]string{
		"session_name": "worker-1", "template": "worker", "agent_name": "worker-1", "pool_managed": "true", "pool_slot": "1", "state": "awake",
		"instance_token": "tok-1", "generation": "1", testTriggerBeadIDKey: work.ID,
		idleClaimNudgeTriggerKey: work.ID, idleClaimNudgeCountKey: strconv.Itoa(idleClaimNudgeMaxAttempts),
		idleClaimNudgeAtKey: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339), idleClaimNudgeTokenKey: "tok-1",
	}})
	if err != nil {
		t.Fatal(err)
	}
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker-1", runtime.Config{Command: "true"}); err != nil {
		t.Fatal(err)
	}
	sp.SetActivity("worker-1", time.Now().Add(-time.Hour))
	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(1), StartCommand: "true"}}}
	cr := &CityRuntime{
		cityPath: t.TempDir(), cityName: "test-city", cfg: cfg, sp: sp, standaloneCityStore: store,
		sessionDrains: newDrainTracker(), rec: events.Discard, stdout: io.Discard, stderr: io.Discard,
	}
	result := DesiredStateResult{
		State:                              map[string]TemplateParams{"worker-1": {TemplateName: "worker", SessionName: "worker-1", Command: "true"}},
		PoolDesiredCounts:                  map[string]int{"worker": 1}, // the routed step's demand keeps the slot awake, as in production
		ReadyUnassignedRoutedWorkBeads:     []beads.Bead{work},
		ReadyUnassignedRoutedWorkStoreRefs: []string{"city"},
	}
	cr.beadReconcileTick(context.Background(), newSeatWork(cr.workLegs()), result, newSessionBeadSnapshot([]beads.Bead{session}), nil, false)
	if ds := cr.sessionDrains.get(session.ID); ds == nil || ds.reason != executionStalledDrainReason {
		t.Fatalf("drain %+v, want the wedged slot draining for %s", ds, executionStalledDrainReason)
	}
}

// Kills the claim drain's live re-read missing the trigger by residency
// (#7752 review r2, ga-bmfmt; reviewer probe R6): on a single-store and a split
// city alike (maintainer-city's graph = "infra"), a routed graph step, which
// a split city keeps in the class binding, is found; on the split city the
// binding's copy wins over a stale pre-migration copy co-resident in the work
// ledger; and an unknown id is a clean miss.
func TestWorkLegsReadLiveFindsTheTriggerByResidency(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		step := mintDurableGraphBead(t, e, "routed step", "executor")
		if e.split {
			// The binding's copy has moved on; the ledger keeps the copy
			// `gc storage migrate` left behind.
			if err := e.class.Update(step.ID, beads.UpdateOpts{Status: ptrString("in_progress"), Assignee: ptrString("worker-1")}); err != nil {
				t.Fatal(err)
			}
			ledger := e.work
			if policy, wrapped := ledger.(*beadPolicyStore); wrapped {
				ledger = policy.Store // the forced create sits below the policy fence, as gc storage migrate's does
			}
			forcer, ok := ledger.(beads.ForeignIDCreator)
			if !ok {
				t.Fatalf("the work store %T cannot model a retained pre-migration copy", e.work)
			}
			if _, err := forcer.CreateWithForeignID(beads.Bead{ID: step.ID, Title: "stale copy", Type: "task"}); err != nil {
				t.Fatalf("seeding the stale ledger copy: %v", err)
			}
		}
		legs := workLegsFromCensus(e.cityPath, e.cfg, cityWorkLegOf(beads.WorkStore{Store: e.work}), nil)
		b, found, err := legs.readLive(step.ID)
		if err != nil || !found || b.ID != step.ID {
			t.Fatalf("split=%v: readLive(%s) = %q found %v err %v, want the routed step", e.split, step.ID, b.ID, found, err)
		}
		if e.split && (b.Status != "in_progress" || b.Assignee != "worker-1") {
			t.Fatalf("readLive read %+v, want the binding's in_progress copy, not the stale ledger copy", b)
		}
		if _, found, err := legs.readLive("gcg-nope"); found || err != nil {
			t.Fatalf("split=%v: an unknown id: found %v err %v, want a clean miss", e.split, found, err)
		}
	})
}

// Kills a new incarnation that inherits its predecessor's spent budget
// without a latch (#7752 review r2 nit): a marker at the cap written for
// another incarnation reads as a new assignment, so the slot is observed and
// nudged, not drained.
func TestNudgeStalledPoolClaims_AnotherIncarnationsMarkerIsANewAssignment(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s := atCapSession(base, "tok-2")
	s.Metadata[idleClaimNudgeTokenKey] = "tok-1" // the cap the previous incarnation spent; no drain latched
	store := beads.NewMemStoreFrom(0, []beads.Bead{s, {ID: "work-a", Status: "open"}}, nil)
	now := base.Add(time.Hour)
	sp := quietClaimFake(t, now)
	drains := &drainRecorder{}
	var out bytes.Buffer
	nudgeStalledPoolClaims(sp, idleClaimTestCfg(), store, []beads.Bead{s}, []beads.Bead{{ID: "work-a", Status: "open"}}, nil, now, liveClaimDrain(store, drains.request), &out)
	got := mustGetTestBead(t, store, s.ID)
	if len(drains.ids) != 0 || got.Metadata[idleClaimNudgeCountKey] != "0" || got.Metadata[idleClaimNudgeTokenKey] != "tok-2" {
		t.Fatalf("drains %v metadata %v, want a fresh observation for tok-2 and no drain", drains.ids, got.Metadata)
	}
}

// Kills silencing template-assigned pool demand (#7752 review r2): a trigger
// assigned to a pool template identity, not a session, is still unclaimed, so
// the backstop nudges the slot bound to it.
func TestNudgeStalledPoolClaims_TemplateAssignedTriggerIsUnclaimed(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	cfg := idleClaimTestCfg()
	cfg.Agents[0].MaxActiveSessions = intPtr(3) // agent-a is a pool
	s := idleClaimPoolSession()
	work := []beads.Bead{{ID: "work-a", Status: "open", Assignee: "agent-a"}}
	store := beads.NewMemStoreFrom(0, []beads.Bead{s}, nil)
	sp := runningIdleClaimFake(t, "session-a")
	var out bytes.Buffer
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{s}, work, nil, base, poolClaimDrain{}, &out)
	nudgeStalledPoolClaims(sp, cfg, store, []beads.Bead{mustGetTestBead(t, store, s.ID)}, work, nil, base.Add(idleClaimNudgeGrace+time.Second), poolClaimDrain{}, &out)
	if got := sp.CountCalls("Nudge", "session-a"); got != 1 {
		t.Fatalf("nudges = %d, want the template-assigned trigger treated as unclaimed", got)
	}
}

// Kills readLive answering from a cache (#7752 review r2): the trigger is
// read through the owner's Live handle, so a claim the cache has not seen
// yet is still read.
func TestWorkLegsReadLiveReadsPastTheCache(t *testing.T) {
	backing := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "hq-1", Status: "open", Title: "step"}}, nil)
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b, err := cache.Get("hq-1"); err != nil || b.Status != "open" {
		t.Fatalf("cache read %+v %v, want the primed open row", b, err)
	}
	if err := backing.Update("hq-1", beads.UpdateOpts{Status: ptrString("in_progress"), Assignee: ptrString("worker-1")}); err != nil {
		t.Fatal(err)
	}
	if b, _ := cache.Get("hq-1"); b.Status != "open" {
		t.Skipf("the cache already serves the backing write (%q); nothing to prove past it", b.Status)
	}
	legs := workLegsFromCensus(t.TempDir(), &config.City{Workspace: config.Workspace{Name: "c"}}, cityWorkLegOf(beads.WorkStore{Store: cache}), nil)
	b, found, err := legs.readLive("hq-1")
	if err != nil || !found || b.Status != "in_progress" {
		t.Fatalf("readLive = %+v found %v err %v, want the live in_progress row", b, found, err)
	}
}
