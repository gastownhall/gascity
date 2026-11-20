package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// The named-seat claim-pickup backstop (ga-zogqc1.2.8): a configured-named
// seat that has no bound step bead and whose OWN open work sits unclaimed past
// the grace window is re-nudged on the shared grace -> nudge -> backoff ->
// give-up engine, nudge-only. The pool lanes govern pool seats only, the
// startup-kickoff keys need a bound step, and the work ladder paces pool seats
// only, so without this lane such a seat is never told its work is waiting.
//
// Two lessons from the reverted seat-claim lane (#6302, reverted for #6524)
// are pinned here: the nudge never carries the pool --drain-ack fallback, and
// exhaustion never drains or recycles the seat.

// namedClaimPickupLaneStepName is the external-reads lane step that runs the
// backstop under reconciler v2, in every work_ladder mode.
const namedClaimPickupLaneStepName = "named-claim-pickup"

// claimPickupExhaustedEventType is the typed give-up event's type.
const claimPickupExhaustedEventType = "session.claim_pickup_exhausted"

var claimPickupBase = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// namedClaimPickupSession is a running configured-named seat with no bound
// step bead, as the store holds it.
func namedClaimPickupSession() beads.Bead {
	return beads.Bead{
		ID:     "session-bead-n",
		Status: "open",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":                       "session-n",
			"template":                           "agent-n",
			"agent_name":                         "agent-n",
			"state":                              "awake",
			"instance_token":                     "tok-n",
			session.NamedSessionMetadataKey:      "true",
			session.NamedSessionIdentityMetadata: "agent-n",
		},
	}
}

func claimPickupWork(id, assignee, status string, created time.Time) beads.Bead {
	return beads.Bead{ID: id, Title: id, Type: "task", Status: status, Assignee: assignee, CreatedAt: created}
}

// claimPickupRun is one reconcile tick of the backstop at now over the seat
// row as the store holds it.
func claimPickupRun(t *testing.T, sp runtime.Provider, store beads.Store, rec events.Recorder, work []beads.Bead, partial bool, now time.Time, out *bytes.Buffer) {
	t.Helper()
	rows := []beads.Bead{mustGetTestBead(t, store, "session-bead-n")}
	nudgeNamedClaimPickups(sp, store, rows, work, partial, now, rec, out)
}

func claimPickupSeat(t *testing.T, store beads.Store) map[string]string {
	t.Helper()
	return mustGetTestBead(t, store, "session-bead-n").Metadata
}

func TestNamedClaimPickupBackstopGoverns(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(map[string]string)
		want bool
	}{
		{name: "named seat without a bound step", edit: func(map[string]string) {}, want: true},
		{name: "pool-managed seat", edit: func(m map[string]string) { m["pool_managed"] = "true" }},
		{name: "seat with a bound step", edit: func(m map[string]string) { m[beadmeta.BoundStepIDMetadataKey] = "step-1" }},
		{name: "not a named seat", edit: func(m map[string]string) { delete(m, session.NamedSessionMetadataKey) }},
		{name: "named flag false", edit: func(m map[string]string) { m[session.NamedSessionMetadataKey] = "false" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := namedClaimPickupSession()
			tc.edit(s.Metadata)
			if got := (namedClaimPickupBackstop{}).governs(s); got != tc.want {
				t.Fatalf("governs = %v, want %v", got, tc.want)
			}
		})
	}
}

// AC2/AC5: the oldest open bead assigned to the seat seeds the lazily-created
// pacing keys on first sighting, with no nudge inside the grace, and a nudge
// follows once the grace has passed. The nudge is the plain claim nudge, never
// the pool --drain-ack fallback.
func TestNudgeNamedClaimPickups_SeedsThenNudgesAfterGrace(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-n")
	sess := namedClaimPickupSession()
	work := []beads.Bead{claimPickupWork("work-1", "session-n", "open", claimPickupBase.Add(-time.Hour))}
	store := beads.NewMemStoreFrom(0, append([]beads.Bead{sess}, work...), nil)
	clk := &clock.Fake{Time: claimPickupBase}
	var out bytes.Buffer

	claimPickupRun(t, sp, store, events.Discard, work, false, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", "session-n"); got != 0 {
		t.Fatalf("first tick Nudge calls = %d, want 0 inside the grace", got)
	}
	m := claimPickupSeat(t, store)
	for key, want := range map[string]string{
		claimPickupStateKey:     claimPickupStatePending,
		claimPickupStartedAtKey: clk.Now().UTC().Format(time.RFC3339),
		claimPickupAttemptsKey:  "0",
		claimPickupBeadIDKey:    "work-1",
	} {
		if m[key] != want {
			t.Fatalf("%s = %q after the first sighting, want %q", key, m[key], want)
		}
	}

	clk.Advance(idleClaimNudgeGrace + time.Second)
	claimPickupRun(t, sp, store, events.Discard, work, false, clk.Now(), &out)
	if got := sp.CountCalls("Nudge", "session-n"); got != 1 {
		t.Fatalf("Nudge calls = %d, want 1 after the grace; log:\n%s", got, out.String())
	}
	m = claimPickupSeat(t, store)
	for key, want := range map[string]string{
		claimPickupStateKey:       claimPickupStateNudged,
		claimPickupAttemptsKey:    "1",
		claimPickupLastNudgeAtKey: clk.Now().UTC().Format(time.RFC3339),
	} {
		if m[key] != want {
			t.Fatalf("%s = %q after the nudge, want %q", key, m[key], want)
		}
	}
	for _, call := range sp.SnapshotCalls() {
		if call.Method != "Nudge" {
			continue
		}
		if call.Message == "" {
			t.Fatal("the claim-pickup nudge was empty")
		}
		if strings.Contains(call.Message, "drain-ack") {
			t.Fatalf("nudge %q carries the pool --drain-ack fallback; that restarted named seats (#6524)", call.Message)
		}
	}
}

// Named work is routinely assigned by the seat's configured identity, not its
// runtime session name; matching only the session name would never fire.
func TestNudgeNamedClaimPickups_MatchesConfiguredIdentityAssignee(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-n")
	sess := namedClaimPickupSession()
	work := []beads.Bead{claimPickupWork("work-1", "agent-n", "open", claimPickupBase.Add(-time.Hour))}
	store := beads.NewMemStoreFrom(0, append([]beads.Bead{sess}, work...), nil)
	var out bytes.Buffer

	claimPickupRun(t, sp, store, events.Discard, work, false, claimPickupBase, &out)
	if got := claimPickupSeat(t, store)[claimPickupBeadIDKey]; got != "work-1" {
		t.Fatalf("claim_pickup_bead_id = %q, want work-1 for work assigned to the configured identity", got)
	}
}

// AC2: with several open assigned beads, the oldest is the target.
func TestNudgeNamedClaimPickups_TargetsOldestOpenBead(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-n")
	sess := namedClaimPickupSession()
	work := []beads.Bead{
		claimPickupWork("work-new", "session-n", "open", claimPickupBase.Add(-time.Minute)),
		claimPickupWork("work-old", "session-n", "open", claimPickupBase.Add(-time.Hour)),
		claimPickupWork("work-mid", "session-n", "open", claimPickupBase.Add(-10*time.Minute)),
	}
	store := beads.NewMemStoreFrom(0, append([]beads.Bead{sess}, work...), nil)
	var out bytes.Buffer

	claimPickupRun(t, sp, store, events.Discard, work, false, claimPickupBase, &out)
	if got := claimPickupSeat(t, store)[claimPickupBeadIDKey]; got != "work-old" {
		t.Fatalf("claim_pickup_bead_id = %q, want the oldest open bead work-old", got)
	}
}

// The seat's pacing is per target: when the oldest open bead changes, the new
// one starts its own grace instead of inheriting the old one's attempts.
func TestNudgeNamedClaimPickups_ANewTargetRestartsPacing(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-n")
	sess := namedClaimPickupSession()
	sess.Metadata[claimPickupStateKey] = claimPickupStateNudged
	sess.Metadata[claimPickupBeadIDKey] = "work-gone"
	sess.Metadata[claimPickupAttemptsKey] = "2"
	sess.Metadata[claimPickupStartedAtKey] = claimPickupBase.Add(-time.Hour).Format(time.RFC3339)
	sess.Metadata[claimPickupLastNudgeAtKey] = claimPickupBase.Add(-time.Hour).Format(time.RFC3339)
	work := []beads.Bead{claimPickupWork("work-2", "session-n", "open", claimPickupBase.Add(-time.Hour))}
	store := beads.NewMemStoreFrom(0, append([]beads.Bead{sess}, work...), nil)
	var out bytes.Buffer

	claimPickupRun(t, sp, store, events.Discard, work, false, claimPickupBase, &out)
	if got := sp.CountCalls("Nudge", "session-n"); got != 0 {
		t.Fatalf("Nudge calls = %d, want 0: a new target starts inside its own grace", got)
	}
	m := claimPickupSeat(t, store)
	if m[claimPickupBeadIDKey] != "work-2" || m[claimPickupAttemptsKey] != "0" || m[claimPickupStateKey] != claimPickupStatePending {
		t.Fatalf("pacing after a target change = bead %q attempts %q state %q, want work-2 / 0 / pending",
			m[claimPickupBeadIDKey], m[claimPickupAttemptsKey], m[claimPickupStateKey])
	}
}

// AC3: nothing open is assigned to the seat, so the lane is a no-op and writes
// nothing to the seat row.
func TestNudgeNamedClaimPickups_NoOpenAssignedWorkIsANoOp(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-n")
	sess := namedClaimPickupSession()
	work := []beads.Bead{
		claimPickupWork("someone-elses", "session-other", "open", claimPickupBase.Add(-time.Hour)),
		claimPickupWork("unassigned", "", "open", claimPickupBase.Add(-time.Hour)),
		claimPickupWork("closed-mine", "session-n", "closed", claimPickupBase.Add(-time.Hour)),
	}
	store := beads.NewMemStoreFrom(0, append([]beads.Bead{sess}, work...), nil)
	var out bytes.Buffer

	for i := 0; i < 3; i++ {
		claimPickupRun(t, sp, store, events.Discard, work, false, claimPickupBase.Add(time.Duration(i)*time.Hour), &out)
	}
	if got := sp.CountCalls("Nudge", "session-n"); got != 0 {
		t.Fatalf("Nudge calls = %d, want 0 with nothing open assigned to the seat", got)
	}
	m := claimPickupSeat(t, store)
	for _, key := range []string{claimPickupStateKey, claimPickupStartedAtKey, claimPickupAttemptsKey, claimPickupLastNudgeAtKey, claimPickupBeadIDKey} {
		if m[key] != "" {
			t.Fatalf("%s = %q, want nothing written when no work is outstanding", key, m[key])
		}
	}
}

// AC4: once the seat holds an in_progress bead it has picked its work up, so
// the lane stands down: no nudge, and a pacing window already running is wiped.
func TestNudgeNamedClaimPickups_ClearsOnceTheSeatClaims(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-n")
	sess := namedClaimPickupSession()
	sess.Metadata[claimPickupStateKey] = claimPickupStateNudged
	sess.Metadata[claimPickupBeadIDKey] = "work-1"
	sess.Metadata[claimPickupAttemptsKey] = "1"
	sess.Metadata[claimPickupStartedAtKey] = claimPickupBase.Add(-time.Hour).Format(time.RFC3339)
	sess.Metadata[claimPickupLastNudgeAtKey] = claimPickupBase.Add(-time.Hour).Format(time.RFC3339)
	work := []beads.Bead{
		claimPickupWork("work-1", "session-n", "in_progress", claimPickupBase.Add(-2*time.Hour)),
		claimPickupWork("work-2", "session-n", "open", claimPickupBase.Add(-time.Hour)),
	}
	store := beads.NewMemStoreFrom(0, append([]beads.Bead{sess}, work...), nil)
	var out bytes.Buffer

	claimPickupRun(t, sp, store, events.Discard, work, false, claimPickupBase, &out)
	if got := sp.CountCalls("Nudge", "session-n"); got != 0 {
		t.Fatalf("Nudge calls = %d, want 0 for a seat already working an in_progress bead", got)
	}
	m := claimPickupSeat(t, store)
	for _, key := range []string{claimPickupStateKey, claimPickupStartedAtKey, claimPickupAttemptsKey, claimPickupLastNudgeAtKey, claimPickupBeadIDKey} {
		if m[key] != "" {
			t.Fatalf("%s = %q, want the pacing window wiped once the seat claimed", key, m[key])
		}
	}
}

// Work on a dispatch hold stays with its agent, unnudged, until the hold lifts
// (the work ladder's owner ruling of 2026-10-18).
func TestNudgeNamedClaimPickups_IgnoresDispatchHeldWork(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-n")
	sess := namedClaimPickupSession()
	held := claimPickupWork("work-held", "session-n", "open", claimPickupBase.Add(-time.Hour))
	held.Labels = []string{beadmeta.DispatchHoldLabels[0]}
	work := []beads.Bead{held}
	store := beads.NewMemStoreFrom(0, append([]beads.Bead{sess}, work...), nil)
	var out bytes.Buffer

	claimPickupRun(t, sp, store, events.Discard, work, false, claimPickupBase, &out)
	claimPickupRun(t, sp, store, events.Discard, work, false, claimPickupBase.Add(time.Hour), &out)
	if got := sp.CountCalls("Nudge", "session-n"); got != 0 {
		t.Fatalf("Nudge calls = %d, want 0 for work on a dispatch hold", got)
	}
	if got := claimPickupSeat(t, store)[claimPickupStateKey]; got != "" {
		t.Fatalf("claim_pickup_state = %q, want nothing seeded for held work", got)
	}
}

// AC6: the engine's cap is unchanged (idleClaimNudgeMaxAttempts attempts, each
// with its backoff). Exhaustion latches a durable given_up state and emits one
// typed event; it nudges no further and never drains or recycles the seat.
func TestNudgeNamedClaimPickups_ExhaustionLatchesGivenUpAndEmitsOnce(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-n")
	sess := namedClaimPickupSession()
	sess.Metadata[claimPickupStateKey] = claimPickupStateNudged
	sess.Metadata[claimPickupBeadIDKey] = "work-1"
	sess.Metadata[claimPickupAttemptsKey] = strconv.Itoa(idleClaimNudgeMaxAttempts)
	sess.Metadata[claimPickupStartedAtKey] = claimPickupBase.Add(-time.Hour).Format(time.RFC3339)
	sess.Metadata[claimPickupLastNudgeAtKey] = claimPickupBase.Format(time.RFC3339)
	work := []beads.Bead{claimPickupWork("work-1", "session-n", "open", claimPickupBase.Add(-time.Hour))}
	store := beads.NewMemStoreFrom(0, append([]beads.Bead{sess}, work...), nil)
	rec := events.NewFake()
	var out bytes.Buffer

	// The last attempt still has its backoff to be answered.
	claimPickupRun(t, sp, store, rec, work, false, claimPickupBase.Add(idleClaimNudgeBackoff-time.Second), &out)
	if got := claimPickupSeat(t, store)[claimPickupStateKey]; got != claimPickupStateNudged {
		t.Fatalf("claim_pickup_state = %q inside the last backoff, want %q", got, claimPickupStateNudged)
	}

	after := claimPickupBase.Add(idleClaimNudgeBackoff + time.Second)
	claimPickupRun(t, sp, store, rec, work, false, after, &out)
	m := claimPickupSeat(t, store)
	if m[claimPickupStateKey] != claimPickupStateGivenUp {
		t.Fatalf("claim_pickup_state = %q after exhaustion, want %q", m[claimPickupStateKey], claimPickupStateGivenUp)
	}
	evs, err := rec.List(events.Filter{Type: claimPickupExhaustedEventType})
	if err != nil {
		t.Fatalf("listing events: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("%s events = %d, want exactly 1", claimPickupExhaustedEventType, len(evs))
	}
	var payload struct {
		SessionName string `json:"session_name"`
		BeadID      string `json:"bead_id"`
		Attempts    int    `json:"attempts"`
	}
	if err := json.Unmarshal(evs[0].Payload, &payload); err != nil {
		t.Fatalf("decoding the event payload: %v", err)
	}
	if payload.SessionName != "session-n" || payload.BeadID != "work-1" || payload.Attempts != idleClaimNudgeMaxAttempts {
		t.Fatalf("event payload = %+v, want session-n / work-1 / %d attempts", payload, idleClaimNudgeMaxAttempts)
	}

	// Latched: later ticks neither nudge, re-emit, nor drain.
	claimPickupRun(t, sp, store, rec, work, false, after.Add(time.Hour), &out)
	claimPickupRun(t, sp, store, rec, work, false, after.Add(2*time.Hour), &out)
	if evs, _ := rec.List(events.Filter{Type: claimPickupExhaustedEventType}); len(evs) != 1 {
		t.Fatalf("%s events after later ticks = %d, want still 1", claimPickupExhaustedEventType, len(evs))
	}
	if got := sp.CountCalls("Nudge", "session-n"); got != 0 {
		t.Fatalf("Nudge calls = %d, want 0 once exhausted", got)
	}
	if got := sp.CountCalls("Stop", "session-n"); got != 0 {
		t.Fatalf("Stop calls = %d, want 0: exhaustion must never recycle a named seat (#6524)", got)
	}
	for _, key := range []string{idleClaimNudgeStalledKey, executionClaimNudgeStalledKey} {
		if v := claimPickupSeat(t, store)[key]; v != "" {
			t.Fatalf("%s = %q, want no drain latch on a named seat", key, v)
		}
	}
}

// A snapshot the planner could not read in full cannot show that nothing is
// waiting or that the seat claimed, so the lane does nothing on it.
func TestNudgeNamedClaimPickups_PartialSnapshotDoesNothing(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-n")
	sess := namedClaimPickupSession()
	work := []beads.Bead{claimPickupWork("work-1", "session-n", "open", claimPickupBase.Add(-time.Hour))}
	store := beads.NewMemStoreFrom(0, append([]beads.Bead{sess}, work...), nil)
	var out bytes.Buffer

	claimPickupRun(t, sp, store, events.Discard, work, true, claimPickupBase, &out)
	claimPickupRun(t, sp, store, events.Discard, work, true, claimPickupBase.Add(time.Hour), &out)
	if got := sp.CountCalls("Nudge", "session-n"); got != 0 {
		t.Fatalf("Nudge calls = %d, want 0 on a partial snapshot", got)
	}
	if got := claimPickupSeat(t, store)[claimPickupStateKey]; got != "" {
		t.Fatalf("claim_pickup_state = %q, want nothing seeded on a partial snapshot", got)
	}
}

// A seat whose turn is running holds the nudge and spends no attempt.
func TestNudgeNamedClaimPickups_BusySeatSpendsNoAttempt(t *testing.T) {
	sp := runningIdleClaimFake(t, "session-n")
	sess := namedClaimPickupSession()
	sess.Metadata[claimPickupStateKey] = claimPickupStatePending
	sess.Metadata[claimPickupBeadIDKey] = "work-1"
	sess.Metadata[claimPickupAttemptsKey] = "0"
	sess.Metadata[claimPickupStartedAtKey] = claimPickupBase.Format(time.RFC3339)
	work := []beads.Bead{claimPickupWork("work-1", "session-n", "open", claimPickupBase.Add(-time.Hour))}
	store := beads.NewMemStoreFrom(0, append([]beads.Bead{sess}, work...), nil)
	now := claimPickupBase.Add(idleClaimNudgeGrace + time.Minute)
	sp.SetActivity("session-n", now.Add(-time.Second))
	var out bytes.Buffer

	claimPickupRun(t, sp, store, events.Discard, work, false, now, &out)
	if got := sp.CountCalls("Nudge", "session-n"); got != 0 {
		t.Fatalf("Nudge calls = %d, want 0 for a seat active a second ago", got)
	}
	if got := claimPickupSeat(t, store)[claimPickupAttemptsKey]; got != "0" {
		t.Fatalf("claim_pickup_attempts = %q, want 0: a hold spends no attempt", got)
	}
}

// A seat whose runtime is not running is merely asleep: nothing is seeded, and
// nothing is nudged.
func TestNudgeNamedClaimPickups_StoppedSeatSeedsNothing(t *testing.T) {
	sp := runtime.NewFake() // never started
	sess := namedClaimPickupSession()
	work := []beads.Bead{claimPickupWork("work-1", "session-n", "open", claimPickupBase.Add(-time.Hour))}
	store := beads.NewMemStoreFrom(0, append([]beads.Bead{sess}, work...), nil)
	var out bytes.Buffer

	claimPickupRun(t, sp, store, events.Discard, work, false, claimPickupBase, &out)
	if got := claimPickupSeat(t, store)[claimPickupStateKey]; got != "" {
		t.Fatalf("claim_pickup_state = %q, want nothing seeded for a seat that is not running", got)
	}
}

// AC7: under reconciler v2 the external-reads lane registers the step, and it
// runs in every work_ladder mode (the ladder paces pool seats only). A partial
// or stale allocation summary does nothing.
func TestNamedClaimPickupLaneStep(t *testing.T) {
	for _, mode := range []string{"on", "off"} {
		for _, tc := range []struct {
			name      string
			edit      func(*allocSummary)
			wantState string
		}{
			{name: "open assigned work", edit: func(*allocSummary) {}, wantState: claimPickupStatePending},
			{name: "partial summary", edit: func(s *allocSummary) { s.Partial = true }},
			{name: "stale summary", edit: func(s *allocSummary) { s.At = s.At.Add(-time.Hour) }},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				sp := runningIdleClaimFake(t, "session-n")
				work := claimPickupWork("work-1", "session-n", "open", stepsTestNow.Add(-time.Hour))
				store := beads.NewMemStoreFrom(0, []beads.Bead{namedClaimPickupSession(), work}, nil)
				cfg := idleClaimTestCfg()
				cfg.Daemon.WorkLadder = mode
				sum := &allocSummary{At: stepsTestNow, AssignedWork: []beads.Bead{work}, AssignedStoreRefs: []string{""}}
				tc.edit(sum)
				env := externalReadsEnv{Cfg: cfg, CityStore: store, SP: sp}
				lane, _ := newPoolStepsTestLane(env, func() *allocSummary { return sum }, nil)
				lane.events = events.NewFake()
				var run func(context.Context, externalReadsEnv)
				for _, st := range lane.steps {
					if st.name == namedClaimPickupLaneStepName {
						run = st.run
					}
				}
				if run == nil {
					t.Fatalf("lane has no %q step: the backstop is unreachable under reconciler v2", namedClaimPickupLaneStepName)
				}
				run(context.Background(), env)
				if got := claimPickupSeat(t, store)[claimPickupStateKey]; got != tc.wantState {
					t.Fatalf("claim_pickup_state = %q, want %q", got, tc.wantState)
				}
			})
		}
	}
}
