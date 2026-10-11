package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
)

// refusingCloseStore reproduces the live deadlock exactly: bd refuses to close
// blockedID because blockerID still blocks it, and blockerID is the control
// bead asking for the close. MEASURED on platform as pl-mmneh ↛ pl-pujtf and on
// substrate as su-d04es ↛ su-5e9db.
type refusingCloseStore struct {
	beads.Store
	blockedID string
	blockerID string
	refusals  int
}

func (s *refusingCloseStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.blockedID && opts.Status != nil && *opts.Status == "closed" {
		s.refusals++
		return fmt.Errorf("updating bead %q: exit status 1: cannot close blocked issue: %s is blocked by [%s]",
			id, id, s.blockerID)
	}
	return s.Store.Update(id, opts)
}

// unansweringCloseStore is the Tier-A control: the store never answers the
// close at all — the connection is refused rather than the request refused.
type unansweringCloseStore struct {
	beads.Store
	failID   string
	failures int
}

func (s *unansweringCloseStore) Update(id string, opts beads.UpdateOpts) error {
	if id == s.failID && opts.Status != nil && *opts.Status == "closed" {
		s.failures++
		return fmt.Errorf("updating bead %q: dial tcp 127.0.0.1:3306: connect: connection refused", id)
	}
	return s.Store.Update(id, opts)
}

type deadlockedFinalizeFixture struct {
	cityPath  string
	base      beads.Store
	rootID    string
	controlID string
}

// newDeadlockedFinalizeFixture builds the §1.2 cycle: a workflow root blocked by
// its own workflow-finalize control bead, where processWorkflowFinalize must
// close the root before it can close itself.
func newDeadlockedFinalizeFixture(t *testing.T, orderRunLabel string) deadlockedFinalizeFixture {
	t.Helper()
	clearGCEnv(t)

	base := beads.NewMemStore()
	var labels []string
	if orderRunLabel != "" {
		labels = []string{orderRunLabel}
	}
	root, err := base.Create(beads.Bead{
		Title:    "mol-technical-health-patrol",
		Type:     "task",
		Labels:   labels,
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	control, err := base.Create(beads.Bead{
		Title: "workflow-finalize",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:       beadmeta.KindWorkflowFinalize,
			beadmeta.RootBeadIDMetadataKey: root.ID,
		},
	})
	if err != nil {
		t.Fatalf("create finalize control: %v", err)
	}
	if err := base.DepAdd(root.ID, control.ID, "blocks"); err != nil {
		t.Fatalf("add the self-blocking edge: %v", err)
	}
	return deadlockedFinalizeFixture{
		cityPath:  t.TempDir(),
		base:      base,
		rootID:    root.ID,
		controlID: control.ID,
	}
}

// dispatchOnce runs one control-dispatcher pass. Each call is deliberately
// self-contained — a fresh store handle, no carried state — so a sequence of
// them models a sequence of dispatcher processes, not one long-lived loop.
func (f deadlockedFinalizeFixture) dispatchOnce(t *testing.T, store beads.Store, at time.Time) (string, error) {
	t.Helper()
	prevNow := workflowTraceNow
	workflowTraceNow = func() time.Time { return at }
	defer func() { workflowTraceNow = prevNow }()

	var stderr bytes.Buffer
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	err := runControlDispatcherWithStoreAndConfig(f.cityPath, f.cityPath, store, f.controlID, cfg, io.Discard, &stderr)
	return stderr.String(), err
}

func (f deadlockedFinalizeFixture) recordedEvents(t *testing.T) []events.Event {
	t.Helper()
	path := filepath.Join(f.cityPath, ".gc", "events.jsonl")
	recorded, err := events.ReadAll(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return recorded
}

func countEventsOfType(recorded []events.Event, eventType string) int {
	n := 0
	for _, e := range recorded {
		if e.Type == eventType {
			n++
		}
	}
	return n
}

func mustGetBead(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return bead
}

// TestControlDispatchRecordsSemanticRefusalInsideBudget pins the first half of
// the bounded tier: the retry is preserved (the classification #5020 added is
// still correct for a genuine sibling race), but the bead now explains itself.
// MEASURED before this change: pl-mmneh carried NO gc.controller_error, no
// class, no retry marker after 6,712 consecutive failures.
func TestControlDispatchRecordsSemanticRefusalInsideBudget(t *testing.T) {
	f := newDeadlockedFinalizeFixture(t, "")
	store := &refusingCloseStore{Store: f.base, blockedID: f.rootID, blockerID: f.controlID}
	start := time.Date(2026, 8, 11, 8, 0, 47, 0, time.UTC)

	_, err := f.dispatchOnce(t, store, start)
	if err == nil {
		t.Fatal("first semantic refusal returned nil; inside the budget it must still retry")
	}
	if got := dispatch.ClassifyControllerError(err); got != dispatch.TierSemantic {
		t.Fatalf("ClassifyControllerError = %v, want %v", got, dispatch.TierSemantic)
	}
	if dispatch.IsQuietControllerRetry(err) {
		t.Fatal("the FIRST refusal was marked quiet; only a verbatim repeat may be quiet")
	}

	control := mustGetBead(t, f.base, f.controlID)
	if control.Status != "open" {
		t.Fatalf("control status = %q, want open inside the budget", control.Status)
	}
	if got := control.Metadata[beadmeta.ControllerRetryFirstSeenMetadataKey]; got != start.Format(time.RFC3339) {
		t.Fatalf("%s = %q, want %q", beadmeta.ControllerRetryFirstSeenMetadataKey, got, start.Format(time.RFC3339))
	}
	if got := control.Metadata[beadmeta.ControllerRetryCountMetadataKey]; got != "1" {
		t.Fatalf("%s = %q, want \"1\"", beadmeta.ControllerRetryCountMetadataKey, got)
	}
	if got := control.Metadata[beadmeta.ControllerErrorClassMetadataKey]; got != beadmeta.FailureClassTransient {
		t.Fatalf("%s = %q, want %q", beadmeta.ControllerErrorClassMetadataKey, got, beadmeta.FailureClassTransient)
	}
	if got := control.Metadata[beadmeta.ControllerErrorMetadataKey]; got == "" {
		t.Fatalf("%s is empty; a stuck control bead must explain itself to bd show", beadmeta.ControllerErrorMetadataKey)
	}
	if n := countEventsOfType(f.recordedEvents(t), events.ControlStalled); n != 0 {
		t.Fatalf("control.stalled emitted %d times inside the budget, want 0 (the event is edge-triggered on the quarantine, not on the retry)", n)
	}

	// A verbatim repeat is quiet: it must not reset the dispatcher's idle
	// backoff, which is what pinned the loop at its 1s floor for three days.
	_, repeat := f.dispatchOnce(t, store, start.Add(5*time.Second))
	if !dispatch.IsQuietControllerRetry(repeat) {
		t.Fatalf("repeated identical refusal = %v, want a quiet-marked retry", repeat)
	}
	if !dispatch.IsTransientControllerError(repeat) {
		t.Fatal("a quiet retry stopped classifying as transient; the dispatcher would treat it as fatal")
	}
}

// TestControlDispatchQuarantinesSemanticRefusalAtBudgetExpiry is the fix. Before
// it, this call returned the refusal forever and the fleet sat idle for three
// days reporting green.
func TestControlDispatchQuarantinesSemanticRefusalAtBudgetExpiry(t *testing.T) {
	f := newDeadlockedFinalizeFixture(t, "order-run:core.technical-health-patrol")
	store := &refusingCloseStore{Store: f.base, blockedID: f.rootID, blockerID: f.controlID}
	start := time.Date(2026, 8, 11, 8, 0, 47, 0, time.UTC)

	if _, err := f.dispatchOnce(t, store, start); err == nil {
		t.Fatal("first refusal returned nil, want a retry inside the budget")
	}

	expiry := start.Add(dispatch.DefaultSemanticRetryBudget)
	stderr, err := f.dispatchOnce(t, store, expiry)
	if err != nil {
		t.Fatalf("dispatch at the budget deadline = %v, want nil (the bead is quarantined, so the sweep made progress)", err)
	}
	if !strings.Contains(stderr, "control dispatch: stalled bead="+f.controlID) {
		t.Fatalf("stderr = %q, want a stalled line naming %s", stderr, f.controlID)
	}

	control := mustGetBead(t, f.base, f.controlID)
	if control.Status != "closed" {
		t.Fatalf("control status = %q, want closed after the budget expired", control.Status)
	}
	if got := control.Metadata[beadmeta.ControlQuarantinedMetadataKey]; got != "true" {
		t.Fatalf("%s = %q, want \"true\"", beadmeta.ControlQuarantinedMetadataKey, got)
	}
	if !slices.Contains(control.Labels, "gc:control-quarantined") {
		t.Fatalf("labels = %#v, want gc:control-quarantined", control.Labels)
	}

	// Quarantining the blocker is what restarts the fleet: closing the control
	// bead removes the only blocker of the workflow root it could not close.
	root := mustGetBead(t, f.base, f.rootID)
	if root.Status != "open" {
		t.Fatalf("workflow root status = %q; this fixture's root closes on the next pass, not this one", root.Status)
	}
	deps, err := f.base.DepList(f.rootID, "down")
	if err != nil {
		t.Fatalf("dep list: %v", err)
	}
	for _, dep := range deps {
		if dep.DependsOnID != f.controlID {
			continue
		}
		blocker := mustGetBead(t, f.base, dep.DependsOnID)
		if blocker.Status != "closed" {
			t.Fatalf("the root's blocker %s is still %q; the deadlock is not broken", dep.DependsOnID, blocker.Status)
		}
	}

	recorded := f.recordedEvents(t)
	if n := countEventsOfType(recorded, events.ControlStalled); n != 1 {
		t.Fatalf("control.stalled emitted %d times, want exactly 1 per stalled bead", n)
	}
	if n := countEventsOfType(recorded, events.OrderFailed); n != 1 {
		t.Fatalf("order.failed emitted %d times, want 1 for a root labeled order-run:", n)
	}

	var payload events.ControlStalledPayload
	for _, e := range recorded {
		if e.Type != events.ControlStalled {
			continue
		}
		if e.Subject != f.controlID {
			t.Fatalf("control.stalled subject = %q, want %q", e.Subject, f.controlID)
		}
		if e.RunID != f.rootID {
			t.Fatalf("control.stalled run_id = %q, want the workflow root %q", e.RunID, f.rootID)
		}
		if err := json.Unmarshal(e.Payload, &payload); err != nil {
			t.Fatalf("decode control.stalled payload: %v", err)
		}
	}
	if payload.BeadID != f.controlID {
		t.Fatalf("payload.BeadID = %q, want %q", payload.BeadID, f.controlID)
	}
	if payload.Kind != beadmeta.KindWorkflowFinalize {
		t.Fatalf("payload.Kind = %q, want %q", payload.Kind, beadmeta.KindWorkflowFinalize)
	}
	if payload.RootBeadID != f.rootID {
		t.Fatalf("payload.RootBeadID = %q, want %q", payload.RootBeadID, f.rootID)
	}
	if payload.OrderName != "core.technical-health-patrol" {
		t.Fatalf("payload.OrderName = %q, want core.technical-health-patrol", payload.OrderName)
	}
	if payload.FirstSeen != start.Format(time.RFC3339) {
		t.Fatalf("payload.FirstSeen = %q, want the original anchor %q", payload.FirstSeen, start.Format(time.RFC3339))
	}
	if payload.Attempts != 2 {
		t.Fatalf("payload.Attempts = %d, want 2", payload.Attempts)
	}
	if payload.ErrorClass != dispatch.TierSemantic.String() {
		t.Fatalf("payload.ErrorClass = %q, want %q", payload.ErrorClass, dispatch.TierSemantic.String())
	}
	if !strings.Contains(payload.Error, "cannot close blocked issue") {
		t.Fatalf("payload.Error = %q, want the store's refusal", payload.Error)
	}

	// One event per stalled bead, not one per sweep: the next pass finds the
	// bead closed and does nothing.
	if _, err := f.dispatchOnce(t, store, expiry.Add(time.Minute)); err != nil {
		t.Fatalf("post-quarantine dispatch = %v, want nil", err)
	}
	if n := countEventsOfType(f.recordedEvents(t), events.ControlStalled); n != 1 {
		t.Fatalf("control.stalled emitted %d times after a follow-up sweep, want it to stay at 1", n)
	}
}

// TestControlDispatchSemanticBudgetSurvivesDispatcherRestart is the reason the
// deadline lives on the bead. MEASURED: the control dispatcher restarted five
// times during the outage (deploys at 08-10T20:39, 08-11T07:00, 10:39, 14:09,
// 21:54). Every call below is a distinct dispatcher process — new store handle,
// no carried state — and the budget must still expire 15 minutes after the
// FIRST refusal, not 15 minutes after the last restart.
func TestControlDispatchSemanticBudgetSurvivesDispatcherRestart(t *testing.T) {
	f := newDeadlockedFinalizeFixture(t, "")
	start := time.Date(2026, 8, 11, 8, 0, 47, 0, time.UTC)

	newDispatcher := func() *refusingCloseStore {
		return &refusingCloseStore{Store: f.base, blockedID: f.rootID, blockerID: f.controlID}
	}

	if _, err := f.dispatchOnce(t, newDispatcher(), start); err == nil {
		t.Fatal("first refusal returned nil, want a retry inside the budget")
	}

	for restart := 1; restart <= 5; restart++ {
		at := start.Add(time.Duration(restart) * 2 * time.Minute)
		_, err := f.dispatchOnce(t, newDispatcher(), at)
		if err == nil {
			t.Fatalf("restart %d at +%s quarantined inside the %s budget",
				restart, at.Sub(start), dispatch.DefaultSemanticRetryBudget)
		}
		control := mustGetBead(t, f.base, f.controlID)
		if got := control.Metadata[beadmeta.ControllerRetryFirstSeenMetadataKey]; got != start.Format(time.RFC3339) {
			t.Fatalf("restart %d moved the anchor to %q, want the original %q; a restart must not extend the budget",
				restart, got, start.Format(time.RFC3339))
		}
		if control.Status != "open" {
			t.Fatalf("restart %d closed the control bead inside the budget", restart)
		}
	}

	// One nanosecond before the deadline: still retrying.
	justBefore := start.Add(dispatch.DefaultSemanticRetryBudget - time.Nanosecond)
	if _, err := f.dispatchOnce(t, newDispatcher(), justBefore); err == nil {
		t.Fatalf("dispatch at +%s (one ns inside the budget) quarantined early", justBefore.Sub(start))
	}

	// A sixth dispatcher, which never saw any of the five before it, expires the
	// budget from the anchor the bead carries.
	at := start.Add(dispatch.DefaultSemanticRetryBudget)
	if _, err := f.dispatchOnce(t, newDispatcher(), at); err != nil {
		t.Fatalf("a restarted dispatcher at +%s = %v, want the quarantine; the deadline did not survive the restarts",
			at.Sub(start), err)
	}
	control := mustGetBead(t, f.base, f.controlID)
	if control.Status != "closed" {
		t.Fatalf("control status = %q, want closed", control.Status)
	}
	// One per dispatcher pass: the anchor, five restarts, the just-before probe,
	// and the pass that expired the budget.
	if got := control.Metadata[beadmeta.ControllerRetryCountMetadataKey]; got != "8" {
		t.Fatalf("%s = %q, want \"8\" (one per dispatcher pass)", beadmeta.ControllerRetryCountMetadataKey, got)
	}
}

// TestControlDispatchAvailabilityErrorStaysUnbounded is the Tier-A control. An
// error that means "the store never answered" must retry forever, exactly as it
// did before the tier split — and must write nothing, because recording a
// budget would require the very store that is unavailable.
func TestControlDispatchAvailabilityErrorStaysUnbounded(t *testing.T) {
	f := newDeadlockedFinalizeFixture(t, "order-run:core.technical-health-patrol")
	store := &unansweringCloseStore{Store: f.base, failID: f.rootID}
	start := time.Date(2026, 8, 11, 8, 0, 47, 0, time.UTC)

	// Ten times the semantic budget, and a hundred attempts.
	for i := range 100 {
		at := start.Add(time.Duration(i) * 6 * time.Minute)
		stderr, err := f.dispatchOnce(t, store, at)
		if err == nil {
			t.Fatalf("availability failure %d at +%s returned nil; Tier A must retry unbounded", i, at.Sub(start))
		}
		if got := dispatch.ClassifyControllerError(err); got != dispatch.TierAvailability {
			t.Fatalf("ClassifyControllerError = %v, want %v", got, dispatch.TierAvailability)
		}
		if dispatch.IsQuietControllerRetry(err) {
			t.Fatalf("availability failure %d was marked quiet; Tier A keeps its pre-split pendingAny behavior", i)
		}
		if stderr != "" {
			t.Fatalf("availability failure %d wrote stderr %q, want silence", i, stderr)
		}
	}

	control := mustGetBead(t, f.base, f.controlID)
	if control.Status != "open" {
		t.Fatalf("control status = %q after 100 availability failures spanning %s, want open",
			control.Status, 100*6*time.Minute)
	}
	if got := control.Metadata[beadmeta.ControllerRetryFirstSeenMetadataKey]; got != "" {
		t.Fatalf("%s = %q, want empty; Tier A must not persist a budget on a store that is not answering",
			beadmeta.ControllerRetryFirstSeenMetadataKey, got)
	}
	if got := control.Metadata[beadmeta.ControllerRetryCountMetadataKey]; got != "" {
		t.Fatalf("%s = %q, want empty", beadmeta.ControllerRetryCountMetadataKey, got)
	}
	if got := control.Metadata[beadmeta.ControlQuarantinedMetadataKey]; got != "" {
		t.Fatalf("%s = %q, want empty", beadmeta.ControlQuarantinedMetadataKey, got)
	}
	if n := countEventsOfType(f.recordedEvents(t), events.ControlStalled); n != 0 {
		t.Fatalf("control.stalled emitted %d times for an availability failure, want 0", n)
	}
	if store.failures != 100 {
		t.Fatalf("store saw %d close attempts, want 100", store.failures)
	}
}

// TestControlDispatchStalledWithoutAnOrderEmitsNoOrderFailed keeps the
// order-health surface honest: a workflow nothing scheduled must not manufacture
// an order failure.
func TestControlDispatchStalledWithoutAnOrderEmitsNoOrderFailed(t *testing.T) {
	f := newDeadlockedFinalizeFixture(t, "")
	store := &refusingCloseStore{Store: f.base, blockedID: f.rootID, blockerID: f.controlID}
	start := time.Date(2026, 8, 11, 8, 0, 47, 0, time.UTC)

	if _, err := f.dispatchOnce(t, store, start); err == nil {
		t.Fatal("first refusal returned nil, want a retry")
	}
	if _, err := f.dispatchOnce(t, store, start.Add(dispatch.DefaultSemanticRetryBudget)); err != nil {
		t.Fatalf("dispatch at the deadline = %v, want the quarantine", err)
	}

	recorded := f.recordedEvents(t)
	if n := countEventsOfType(recorded, events.ControlStalled); n != 1 {
		t.Fatalf("control.stalled emitted %d times, want 1", n)
	}
	if n := countEventsOfType(recorded, events.OrderFailed); n != 0 {
		t.Fatalf("order.failed emitted %d times for a workflow with no order-run label, want 0", n)
	}
}

// TestSemanticControlRetryBudgetEnvOverride pins the incident knob: "0s"
// restores quarantine-on-first-refusal for an operator clearing a wedged fleet,
// and a negative value restores unbounded retry if the classification is ever
// wrong.
func TestSemanticControlRetryBudgetEnvOverride(t *testing.T) {
	tests := []struct {
		name string
		env  string
		want time.Duration
	}{
		{name: "unset", env: "", want: dispatch.DefaultSemanticRetryBudget},
		{name: "immediate", env: "0s", want: 0},
		{name: "unbounded", env: "-1s", want: -time.Second},
		{name: "custom", env: "90s", want: 90 * time.Second},
		{name: "garbage falls back", env: "not-a-duration", want: dispatch.DefaultSemanticRetryBudget},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GC_CONTROL_SEMANTIC_RETRY_BUDGET", tt.env)
			if got := semanticControlRetryBudget(); got != tt.want {
				t.Fatalf("semanticControlRetryBudget() = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestControlDispatchQuarantinesOnFirstRefusalWithAZeroBudget(t *testing.T) {
	f := newDeadlockedFinalizeFixture(t, "")
	t.Setenv("GC_CONTROL_SEMANTIC_RETRY_BUDGET", "0s")
	store := &refusingCloseStore{Store: f.base, blockedID: f.rootID, blockerID: f.controlID}

	if _, err := f.dispatchOnce(t, store, time.Date(2026, 8, 11, 8, 0, 47, 0, time.UTC)); err != nil {
		t.Fatalf("dispatch with a zero budget = %v, want an immediate quarantine", err)
	}
	if control := mustGetBead(t, f.base, f.controlID); control.Status != "closed" {
		t.Fatalf("control status = %q, want closed", control.Status)
	}
	if n := countEventsOfType(f.recordedEvents(t), events.ControlStalled); n != 1 {
		t.Fatalf("control.stalled emitted %d times, want 1", n)
	}
}
