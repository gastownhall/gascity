package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/executionevent"
)

func closedStepEvent(t *testing.T, stepID, rootID string) events.Event {
	t.Helper()
	payload, err := json.Marshal(beads.Bead{
		ID: stepID, Status: "closed",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID},
	})
	if err != nil {
		t.Fatalf("marshal step payload: %v", err)
	}
	return events.Event{Type: events.BeadClosed, Subject: stepID, Payload: payload}
}

// TestCompletionsLaneNamesRootsFromBothEventShapes pins the delta feed's two
// inputs. A step's closure reaches this process either as an execution.step_*
// fact (which states its RunID) or as a bead.closed notification carrying the
// physical step snapshot (whose gc.root_bead_id is the root). Missing either
// shape would leave a whole class of closes to wait for the hourly sweep.
func TestCompletionsLaneNamesRootsFromBothEventShapes(t *testing.T) {
	lane := newCompletionsLane()
	lane.observe(events.Event{Type: events.ExecutionStepCompleted, RunID: "gcg-root-a", Subject: "gcg-step-a"})
	lane.observe(closedStepEvent(t, "gcg-step-b", "gcg-root-b"))
	// Neither shape: ordinary traffic must cost the tick nothing.
	lane.observe(events.Event{Type: events.BeadUpdated, Subject: "ga-1"})
	lane.observe(closedStepEvent(t, "ga-2", ""))

	got := map[string]bool{}
	for _, id := range lane.takePending() {
		got[id] = true
	}
	if len(got) != 2 || !got["gcg-root-a"] || !got["gcg-root-b"] {
		t.Fatalf("pending roots = %v, want exactly gcg-root-a and gcg-root-b", got)
	}
	// Control: draining really drained, so the set above is per-pass and not
	// an ever-growing list the tick would re-walk.
	if rest := lane.takePending(); len(rest) != 0 {
		t.Fatalf("pending after drain = %v, want empty", rest)
	}
}

// TestCompletionsLaneSweepCadenceReplacesTriggerNameGating pins the schedule.
// The pre-slice gate was `trigger == "patrol"`, which under overload means
// "every tick" — explicit cadence state is what makes "rare" actually rare.
func TestCompletionsLaneSweepCadenceReplacesTriggerNameGating(t *testing.T) {
	now := time.Now()
	lane := newCompletionsLane()
	if _, due := lane.sweepDue(now); !due {
		t.Fatal("a lane that has never swept is not due; nothing has converged yet")
	}
	lane.noteSweepChunk(now, backstopReasonStartup, 0, 0, true)
	if _, due := lane.sweepDue(now.Add(time.Minute)); due {
		t.Fatal("the sweep is due a minute after a full one; the cadence gate is not gating")
	}
	if _, due := lane.sweepDue(now.Add(completionsBackstopInterval)); !due {
		t.Fatal("the sweep is not due at its cadence")
	}
	// A gap in the feed makes it due immediately: the delta lane can no longer
	// claim to name every changed root.
	lane.force()
	if _, due := lane.sweepDue(now.Add(time.Second)); !due {
		t.Fatal("a feed gap did not force the sweep")
	}
}

// TestCompletionsLaneOverflowForcesTheSweep pins the other gap shape: more named
// roots than the lane will hold means candidates would have to be dropped, and a
// dropped root is a lifecycle gap nothing else is looking for.
func TestCompletionsLaneOverflowForcesTheSweep(t *testing.T) {
	lane := newCompletionsLane()
	lane.noteSweepChunk(time.Now(), backstopReasonStartup, 0, 0, true)
	for i := range completionsCandidateCap + 1 {
		lane.observe(events.Event{Type: events.ExecutionStepCompleted, RunID: overflowBeadID(i)})
	}
	if reason, due := lane.sweepDue(time.Now()); !due || reason != backstopReasonCursorGap {
		t.Fatalf("candidate overflow left the sweep due=%t reason=%q, want due with reason %q", due, reason, backstopReasonCursorGap)
	}
	// Control: below the cap the lane keeps its candidates and stays un-forced.
	small := newCompletionsLane()
	small.noteSweepChunk(time.Now(), backstopReasonStartup, 0, 0, true)
	small.observe(events.Event{Type: events.ExecutionStepCompleted, RunID: "gcg-root-a"})
	if _, due := small.sweepDue(time.Now()); due {
		t.Fatal("a single named root forced the sweep; overflow is not what the assertion above measured")
	}
	if got := small.takePending(); len(got) != 1 || got[0] != "gcg-root-a" {
		t.Fatalf("pending = %v, want [gcg-root-a]", got)
	}
}

// TestCompletionReconcileInputsNarrowToTheInfraStoreOnTheRuntimePlane is the
// operator invariant on the completions lane (ga-l7jdg, bd memory
// gascity-runtime-infra-store-invariant): city operations read the infra/class
// store only, so the tick's delta pass gets ONE leg — the graph class store —
// while the off-tick convergence lane keeps the whole fan it must converge.
//
// resolveGraphStore answers "the binding when the graph class is relocated, the
// city store otherwise", so the narrowing needs no special case for a
// single-store city: there the work store IS the infra store, and the runtime
// plane's one leg is the right one.
func TestCompletionReconcileInputsNarrowToTheInfraStoreOnTheRuntimePlane(t *testing.T) {
	cs := &controllerState{
		cfg:           &config.City{Workspace: config.Workspace{Name: "test-city"}},
		cityBeadStore: beads.NewMemStore(),
		beadStores:    map[string]beads.Store{"alpha": beads.NewMemStore(), "beta": beads.NewMemStore()},
		eventProv:     events.NewFake(),
	}

	ep, runtimeFan := cs.completionReconcileInputs(runtimePlane)
	if ep == nil {
		t.Fatal("no event provider; the fixture cannot express the invariant")
	}
	if len(runtimeFan) != 1 {
		t.Fatalf("the runtime plane fans out to %d store(s), want 1 (the infra/class store)", len(runtimeFan))
	}
	// Control: the convergence lane keeps every store, so "one leg" above is a
	// narrowing and not a fan that collapsed for some other reason.
	_, reconcileFan := cs.completionReconcileInputs(reconcilePlane)
	if len(reconcileFan) <= len(runtimeFan) {
		t.Fatalf("the convergence lane fans out to %d store(s) and the runtime plane to %d; the planes are not distinguishable",
			len(reconcileFan), len(runtimeFan))
	}
	if len(reconcileFan) != 3 {
		t.Fatalf("the convergence lane fans out to %d store(s), want 3 (city work + two rigs)", len(reconcileFan))
	}
}

// TestCompletionsSweepSummaryAccumulatesAcrossChunks pins that the sweep reports
// the SWEEP, not the chunk it happened to finish on.
//
// A chunked traversal that logged per chunk would report "converged 2 roots" for
// a city with two hundred, which is worse than silence: it reads as a healthy
// small city. The totals are therefore folded across chunks and emitted once,
// when a full traversal closes.
func TestCompletionsSweepSummaryAccumulatesAcrossChunks(t *testing.T) {
	lane := newCompletionsLane()
	now := time.Now()

	if _, done := lane.noteSweepChunk(now, backstopReasonCadence, 1, 2, false); done {
		t.Fatal("an incomplete chunk closed the sweep")
	}
	if _, done := lane.noteSweepChunk(now.Add(time.Second), backstopReasonCadence, 2, 3, false); done {
		t.Fatal("a second incomplete chunk closed the sweep")
	}
	total, done := lane.noteSweepChunk(now.Add(2*time.Second), backstopReasonCadence, 1, 1, true)
	if !done {
		t.Fatal("the completing chunk did not close the sweep")
	}
	if total.Emitted != 4 || total.Roots != 6 {
		t.Fatalf("sweep totals = %+v, want 4 facts over 6 roots (the sum of all three chunks)", total)
	}
	if total.Elapsed < 2*time.Second {
		t.Fatalf("sweep elapsed = %s, want at least the 2s the chunks spanned", total.Elapsed)
	}

	// Control: the accumulators reset, so the NEXT sweep reports its own totals
	// rather than the running total since boot.
	second, done := lane.noteSweepChunk(now.Add(3*time.Second), backstopReasonCadence, 5, 5, true)
	if !done || second.Emitted != 5 || second.Roots != 5 {
		t.Fatalf("second sweep totals = %+v (done=%t), want 5 facts over 5 roots", second, done)
	}
}

// TestCompletionsSweepAlwaysReportsItselfAndItsDarkStores pins the observability
// contract for a lane that runs on a background goroutine: a clean pass says so,
// and a store it could not list says so louder.
//
// A traversal skips an unlistable store deliberately, so one dark store cannot
// stall the sweep. Skipped SILENTLY, that is a convergence lane converging
// nothing while looking exactly like a lane with nothing to converge.
func TestCompletionsSweepAlwaysReportsItselfAndItsDarkStores(t *testing.T) {
	var stderr bytes.Buffer
	cs := &controllerState{
		cfg:           &config.City{Workspace: config.Workspace{Name: "test-city"}},
		cityBeadStore: beads.NewMemStore(),
		eventProv:     events.NewFake(),
	}
	cr := &CityRuntime{cityName: "test-city", cityPath: t.TempDir(), cfg: cs.cfg, cs: cs, logPrefix: "gc", stderr: &stderr}

	cr.runCompletionsSweepChunk(&executionevent.CompletionBackstop{}, cr.completionsLaneOf(), backstopReasonCadence)
	if got := stderr.String(); !strings.Contains(got, "completions sweep: reason=cadence converged") {
		t.Fatalf("a clean sweep logged %q, want a completion line — a silent convergence lane cannot be told from a stopped one", got)
	}

	// A store whose root list fails is named.
	stderr.Reset()
	cs.cityBeadStore = errorListStore{Store: beads.NewMemStore(), err: errors.New("dolt circuit breaker is open")}
	cr.runCompletionsSweepChunk(&executionevent.CompletionBackstop{}, cr.completionsLaneOf(), backstopReasonCursorGap)
	got := stderr.String()
	if !strings.Contains(got, "dolt circuit breaker is open") {
		t.Fatalf("a dark store logged %q, want the list failure named", got)
	}
	// Control: the sweep still reports itself, so the failure line is additional
	// information rather than a replacement for the liveness signal.
	if !strings.Contains(got, "completions sweep: reason=cursor-gap converged") {
		t.Fatalf("a sweep over a dark store logged %q, want the completion line too, naming why it was due", got)
	}
}

// errorListStore fails every metadata list, standing in for a store whose
// backend is refusing (a dolt circuit breaker, a dead remote).
type errorListStore struct {
	beads.Store
	err error
}

func (s errorListStore) ListByMetadata(map[string]string, int, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}

// TestBackstopAgeFieldsDistinguishNeverRanFromJustRan pins the one distinction a
// liveness field has to make. A lane that has never converged and a lane that
// converged a second ago are opposite conditions; reporting both as an age of
// zero is how a stalled backstop hides in a dashboard.
func TestBackstopAgeFieldsDistinguishNeverRanFromJustRan(t *testing.T) {
	never := map[string]any{}
	addBackstopAgeFields(never, time.Time{}, "", false)
	if never["backstop_ran"] != false {
		t.Fatalf("never-ran fields = %v, want backstop_ran=false", never)
	}
	if _, ok := never["backstop_age_seconds"]; ok {
		t.Fatalf("never-ran fields = %v, want no age — an age of zero reads as 'just converged'", never)
	}

	ran := map[string]any{}
	addBackstopAgeFields(ran, time.Now().Add(-90*time.Second), backstopReasonCadence, true)
	if ran["backstop_ran"] != true {
		t.Fatalf("ran fields = %v, want backstop_ran=true", ran)
	}
	age, ok := ran["backstop_age_seconds"].(int)
	if !ok || age < 89 || age > 91 {
		t.Fatalf("ran fields = %v, want an age near 90s", ran)
	}
	if ran["backstop_last_reason"] != backstopReasonCadence {
		t.Fatalf("ran fields = %v, want the reason the pass was due", ran)
	}
}

// TestCompletionsSweepReportsWhyItWasDue pins the completions lane's half of the
// trace contract, which the route-recovery lane already met and this one did not.
//
// `backstop_last_reason` is one trace field written by two lanes, so an operator
// reading it must not have to know which lane wrote it — and a lane that always
// writes an empty reason turns "why did the convergence pass run" into a question
// the trace cannot answer. That matters most in the case worth noticing: a sweep
// running because the event feed declared a GAP is a different event from a sweep
// running on its hourly cadence, and only the reason distinguishes them.
func TestCompletionsSweepReportsWhyItWasDue(t *testing.T) {
	now := time.Now()

	// Nothing has converged yet: the first sweep is a startup pass.
	lane := newCompletionsLane()
	if reason, due := lane.sweepDue(now); !due || reason != backstopReasonStartup {
		t.Fatalf("a fresh lane is due=%t reason=%q, want due with reason %q", due, reason, backstopReasonStartup)
	}
	if _, done := lane.noteSweepChunk(now, backstopReasonStartup, 1, 1, true); !done {
		t.Fatal("the completing chunk did not close the sweep")
	}
	at, reason, ran := lane.lastSweep()
	if !ran || reason != backstopReasonStartup || !at.Equal(now) {
		t.Fatalf("lastSweep = (%s, %q, %t), want the startup pass", at, reason, ran)
	}

	// Cadence and a feed gap are different events, and the trace must say which.
	if r, due := lane.sweepDue(now.Add(completionsBackstopInterval)); !due || r != backstopReasonCadence {
		t.Fatalf("at cadence due=%t reason=%q, want due with reason %q", due, r, backstopReasonCadence)
	}
	lane.force()
	gapReason, due := lane.sweepDue(now.Add(time.Second))
	if !due || gapReason != backstopReasonCursorGap {
		t.Fatalf("after a feed gap due=%t reason=%q, want due with reason %q", due, gapReason, backstopReasonCursorGap)
	}
	lane.noteSweepChunk(now.Add(time.Second), gapReason, 0, 0, true)
	if _, reason, _ = lane.lastSweep(); reason != backstopReasonCursorGap {
		t.Fatalf("lastSweep reason after a gap-driven sweep = %q, want %q", reason, backstopReasonCursorGap)
	}

	// And it reaches the tick record, which is the whole point of latching it.
	fields := map[string]any{}
	at, reason, ran = lane.lastSweep()
	addBackstopAgeFields(fields, at, reason, ran)
	if fields["backstop_last_reason"] != backstopReasonCursorGap {
		t.Fatalf("tick fields = %v, want backstop_last_reason=%q", fields, backstopReasonCursorGap)
	}
	// Control: the field is absent, not empty, before any sweep — so a reason
	// that never got latched fails loudly here instead of reading as "cadence".
	fresh := map[string]any{}
	freshAt, freshReason, freshRan := newCompletionsLane().lastSweep()
	addBackstopAgeFields(fresh, freshAt, freshReason, freshRan)
	if _, present := fresh["backstop_last_reason"]; present {
		t.Fatalf("a lane that never swept reported %v, want no reason at all", fresh)
	}
}
