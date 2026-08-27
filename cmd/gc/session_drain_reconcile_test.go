package main

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"k8s.io/client-go/util/workqueue"
)

// exactDrainAdvanceTestSessionName is the one fixture session every D-DRAIN
// handler test drains.
const exactDrainAdvanceTestSessionName = "worker"

func drainAdvanceAdmission(id string) sessionStartAdmission {
	return sessionStartAdmission{SessionID: id, Source: sessionStartAdmissionDrainAdvance, Version: 7}
}

// newExactDrainAdvanceParams builds the handler's params for one seeded row.
// The row is DESIRED: family precedence routes an undesired row to D-ORPHAN,
// and the D-DRAIN seam sits above it only for rows that already carry intent.
func newExactDrainAdvanceParams(env *reconcilerTestEnv, provider runtime.Provider) exactSessionStartParams {
	const name = exactDrainAdvanceTestSessionName
	statusWriter, _, statusWriterErr := beads.ResolveConditionalWriter(env.store)
	return exactSessionStartParams{
		Generation: 1, CityPath: "test-city", CityName: "test-city",
		Config: env.cfg, Provider: provider, Store: env.store,
		StatusWriter: statusWriter, StatusWriterError: statusWriterErr,
		Recorder: events.Discard, RolloutMode: rollout.Require,
		Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{},
		DrainTracker:        env.dt,
		DrainOps:            newDrainOps(provider),
		DesiredSessionNames: func() map[string]bool { return map[string]bool{name: true} },
	}
}

// seedDrainingSession seeds the fixture the whole family turns on: a live,
// desired, active session with drain intent already recorded in the shared
// in-memory tracker (Q4). The drain reason is the caller's, because the cancel
// arms partition on it.
func seedDrainingSession(t *testing.T, env *reconcilerTestEnv, reason string) (*deadRuntimeProvider, beads.Bead) {
	t.Helper()
	const name = exactDrainAdvanceTestSessionName
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: name, StartCommand: "true"}},
	}
	provider := &deadRuntimeProvider{Fake: env.sp}
	if err := provider.Start(t.Context(), name, runtime.Config{}); err != nil {
		t.Fatalf("start runtime for %q: %v", name, err)
	}
	bead := env.createSessionBead(name, name)
	env.markSessionActive(&bead)
	now := env.clk.Now()
	env.dt.set(bead.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(defaultDrainTimeout),
		reason:     reason,
		generation: 1,
	})
	return provider, bead
}

func dispatchExactDrainAdvance(t *testing.T, env *reconcilerTestEnv, params exactSessionStartParams, id string) (bool, exactSessionStartOwner, error) {
	t.Helper()
	info, response, err := getAuthoritativeSessionStartPersistedRecord(env.store, id)
	if err != nil {
		t.Fatalf("authoritative read: %v", err)
	}
	return reconcileExactSessionDetectorFamily(t.Context(), drainAdvanceAdmission(id), params, info, response, env.clk)
}

// TestExactAckedDrainReachesStopPendingOnceByKey is WD.6's primary RED: an
// acknowledged drain reaches drain_ack_stop_pending exactly once by exact key,
// the in-memory intent retires with the transition, and the family then stops
// claiming the row — the stop leg belongs to the existing keyed drain-ack stop,
// which owns the atomic close committed before the stop (A5).
func TestExactAckedDrainReachesStopPendingOnceByKey(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead := seedDrainingSession(t, env, "idle")
	params := newExactDrainAdvanceParams(env, provider)

	// Leg 1 — the acknowledgement is still outstanding, so the handler writes the
	// deferred signal and nothing else. That deferral IS the one-cycle rescue
	// window: a falsely-drained session gets a full cycle to be canceled.
	handled, owner, err := dispatchExactDrainAdvance(t, env, params, bead.ID)
	if !handled {
		t.Fatal("the D-DRAIN seam did not claim a row carrying drain intent")
	}
	if err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("deferred-signal leg returned owner=%v err=%v, want keyed ownership and no error", owner, err)
	}
	if acked, _ := provider.GetMeta("worker", "GC_DRAIN_ACK"); acked != "1" {
		t.Fatalf("GC_DRAIN_ACK = %q, want the deferred signal set on the first advance", acked)
	}
	if isDrainAckStopPendingInfo(readSessionInfoForTest(t, env, bead.ID)) {
		t.Fatal("the first advance marked stop-pending; the deferred signal must survive one full cycle first")
	}

	// Leg 2 — the acknowledgement is now readable, so the same key marks
	// stop-pending and retires the intent.
	handled, owner, err = dispatchExactDrainAdvance(t, env, params, bead.ID)
	if !handled || err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("stop-pending leg: handled=%v owner=%v err=%v", handled, owner, err)
	}
	info, _, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !isDrainAckStopPendingInfo(info) {
		t.Fatalf("row = %+v, want drain_ack_stop_pending after the acknowledgement was discovered", info)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("drain intent survived the stop-pending transition; the row is the keyed drain-ack stop's from here")
	}
	if !provider.IsRunning("worker") {
		t.Fatal("the stop-pending transition stopped the runtime; the stop leg is the drain-ack stop's, and it is async")
	}

	// Leg 3 — exactly once. The guard excludes a stop-pending row, so the family
	// releases the key rather than re-marking it.
	handled, _, err = dispatchExactDrainAdvance(t, env, params, bead.ID)
	if handled {
		t.Fatal("the D-DRAIN seam re-claimed a stop-pending row; the keyed drain-ack stop owns it")
	}
	if err != nil {
		t.Fatalf("release leg returned err=%v", err)
	}
}

// TestExactDrainAdvanceCompletesWhenTheProcessExited ports
// TestAdvanceSessionDrains_ProcessExited (session_wake_test.go) onto the exact
// key: a drain whose runtime is provably gone completes through the existing
// library's completeDrain and retires its intent.
func TestExactDrainAdvanceCompletesWhenTheProcessExited(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead := seedDrainingSession(t, env, "idle")
	if err := provider.Stop("worker"); err != nil {
		t.Fatalf("stop runtime: %v", err)
	}
	params := newExactDrainAdvanceParams(env, provider)

	handled, owner, err := dispatchExactDrainAdvance(t, env, params, bead.ID)
	if !handled || err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("complete leg: handled=%v owner=%v err=%v", handled, owner, err)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("drain intent survived a completed drain")
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["state"] != "asleep" {
		t.Fatalf("state = %q, want asleep", stored.Metadata["state"])
	}
	if stored.Metadata["sleep_reason"] != "idle" {
		t.Fatalf("sleep_reason = %q, want idle", stored.Metadata["sleep_reason"])
	}
}

// TestExactDrainAdvanceClearsAStaleGenerationWithoutStopping ports
// TestCancelSessionDrain_GenerationMismatch: a drain whose session was re-woken
// under a new generation is CLEARED, never stopped — the stale drain is about an
// incarnation that no longer exists.
func TestExactDrainAdvanceClearsAStaleGenerationWithoutStopping(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead := seedDrainingSession(t, env, "idle")
	if err := provider.SetMeta("worker", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta(GC_DRAIN_ACK): %v", err)
	}
	env.dt.get(bead.ID).ackSet = true
	env.setSessionMetadata(&bead, map[string]string{"generation": "2"})
	params := newExactDrainAdvanceParams(env, provider)

	handled, owner, err := dispatchExactDrainAdvance(t, env, params, bead.ID)
	if !handled || err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("stale leg: handled=%v owner=%v err=%v", handled, owner, err)
	}
	if env.dt.get(bead.ID) != nil {
		t.Fatal("stale drain intent survived; a re-woken session's drain is cleared")
	}
	if ack, _ := provider.GetMeta("worker", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared so the stale ack cannot kill the new incarnation", ack)
	}
	if !provider.IsRunning("worker") {
		t.Fatal("the stale-generation arm stopped the re-woken session")
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["state"] == "asleep" || isDrainAckStopPendingInfo(readSessionInfoForTest(t, env, bead.ID)) {
		t.Fatalf("row = %+v, want the live row untouched by the stale clear", stored.Metadata)
	}
}

// TestExactDrainAdvanceCancelsForAssignedWork ports
// TestAdvanceSessionDrains_OrphanedDrainCanceledForAssignedWork: a drain whose
// session acquired assigned work is CANCELED rather than completed, and the
// acknowledgement metadata is cleared with it.
func TestExactDrainAdvanceCancelsForAssignedWork(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead := seedDrainingSession(t, env, "orphaned")
	if err := provider.SetMeta("worker", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta(GC_DRAIN_ACK): %v", err)
	}
	env.dt.get(bead.ID).ackSet = true
	assignExactDrainWorkForTest(t, env, bead.ID)
	params := newExactDrainAdvanceParams(env, provider)

	handled, owner, err := dispatchExactDrainAdvance(t, env, params, bead.ID)
	if !handled || err != nil || owner != exactSessionStartKeyedOwner {
		t.Fatalf("cancel leg: handled=%v owner=%v err=%v", handled, owner, err)
	}
	if state := env.dt.get(bead.ID); state != nil {
		t.Fatalf("drain = %+v, want canceled for assigned work", state)
	}
	if ack, _ := provider.GetMeta("worker", "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared after the assigned-work cancellation", ack)
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["state"] == "asleep" || isDrainAckStopPendingInfo(readSessionInfoForTest(t, env, bead.ID)) {
		t.Fatalf("row = %+v, want a canceled drain to leave the session awake", stored.Metadata)
	}
	if !provider.IsRunning("worker") {
		t.Fatal("a drain canceled for assigned work stopped the runtime")
	}
}

// TestDetectorDrainSweepIssuesNoProviderGetMeta is the third AC negative and the
// whole reason ack discovery is handler-side: a full detection pass over
// draining sessions performs ZERO provider GetMeta calls. The tracker cannot
// distinguish awaiting-ack from acked and does not need to — the handler
// decides, once, for the one key it holds.
func TestDetectorDrainSweepIssuesNoProviderGetMeta(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Workspace: config.Workspace{Name: "test-city"}}
	provider := &deadRuntimeProvider{Fake: env.sp}
	var infos []sessionpkg.Info
	for _, name := range []string{"w1", "w2", "w3"} {
		env.cfg.Agents = append(env.cfg.Agents, config.Agent{Name: name, StartCommand: "true"})
		if err := provider.Start(t.Context(), name, runtime.Config{}); err != nil {
			t.Fatalf("start runtime for %q: %v", name, err)
		}
		bead := env.createSessionBead(name, name)
		env.markSessionActive(&bead)
		if err := provider.SetMeta(name, "GC_DRAIN_ACK", "1"); err != nil {
			t.Fatalf("SetMeta(GC_DRAIN_ACK): %v", err)
		}
		env.dt.set(bead.ID, &drainState{
			startedAt:  env.clk.Now().Add(-time.Minute),
			deadline:   env.clk.Now().Add(defaultDrainTimeout),
			reason:     "idle",
			generation: 1,
		})
		info, _, err := getAuthoritativeSessionStartPersistedRecord(env.store, bead.ID)
		if err != nil {
			t.Fatal(err)
		}
		infos = append(infos, info)
	}

	admitted := map[string]sessionStartAdmissionSource{}
	in := sleepSweepInput(env, provider, infos, env.clk.Now(), func(id string, source sessionStartAdmissionSource) (sessionStartAdmissionOutcome, error) {
		admitted[id] = source
		return sessionStartAdmissionAccepted, nil
	})
	before := countProviderGetMetaCalls(env.sp.SnapshotCalls())
	result := detectSessionConditions(t.Context(), in)
	routeDetectorConditions(in, &result)
	if got := countProviderGetMetaCalls(env.sp.SnapshotCalls()) - before; got != 0 {
		t.Fatalf("detection pass issued %d provider GetMeta calls, want 0; ack discovery is handler-side", got)
	}

	if len(admitted) != len(infos) {
		t.Fatalf("routed %d draining rows, want %d", len(admitted), len(infos))
	}
	for id, source := range admitted {
		if source != sessionStartAdmissionDrainAdvance {
			t.Fatalf("row %s routed under %q, want %q", id, source, sessionStartAdmissionDrainAdvance)
		}
	}
	for _, cond := range result.Conditions {
		if cond.Family != detectorFamilyDrain {
			continue
		}
		if cond.Reason != detectorReasonDrainInFlight || cond.Outcome != TraceOutcomeDrain {
			t.Fatalf("D-DRAIN condition = %+v, want the drain-in-flight arm", cond)
		}
	}
}

func countProviderGetMetaCalls(calls []runtime.Call) int {
	count := 0
	for _, call := range calls {
		if call.Method == "GetMeta" {
			count++
		}
	}
	return count
}

func readSessionInfoForTest(t *testing.T, env *reconcilerTestEnv, id string) sessionpkg.Info {
	t.Helper()
	info, err := sessionFrontDoor(env.store).Get(id)
	if err != nil {
		t.Fatalf("read session info for %s: %v", id, err)
	}
	return info
}

// assignExactDrainWorkForTest gives the session an open, awake assigned work
// bead so the live reachable-store query the handler re-pays answers true.
func assignExactDrainWorkForTest(t *testing.T, env *reconcilerTestEnv, sessionID string) {
	t.Helper()
	if _, err := env.store.Create(beads.Bead{
		Title:    "assigned work",
		Status:   "in_progress",
		Assignee: sessionID,
	}); err != nil {
		t.Fatalf("create assigned work: %v", err)
	}
}

// TestSessionStartControllerReleasesAPermanentlyRefusedDrainAckAtTheDrainDeadline
// is RULING 1b's RED. The drain-ack re-queue is unbounded by design — a drain-ack
// is a durable obligation — but while an admission is parked the keyed
// controller EXCLUDES legacy from the row, so a permanently-refused
// authorization blocks the drain from finishing under any owner. The bound is the
// drain's own ack-or-timeout deadline, not a retry count: on expiry the
// admission is deleted, the retained lease dropped, an audit armed, and the row
// released so level-triggered re-detection re-owns it.
func TestSessionStartControllerReleasesAPermanentlyRefusedDrainAckAtTheDrainDeadline(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	clockNow := now
	attempts := 0
	fencedWhileParked := true
	const attemptsBeforeDeadline = 5
	released := make(chan sessionStartReconcileResult, 1)
	var controller *sessionStartController
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers: 1, MaxDistinct: 4, MaxRetries: 2,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			mu.Lock()
			attempts++
			seen := attempts
			mu.Unlock()
			// The whole point of the retained re-queue is that it outlives
			// maxRetries; while it does, the fence must hold.
			if seen == attemptsBeforeDeadline-1 && !controller.ownsPoolDrainAckStop("gc-drain-1", "tok-1") {
				fencedWhileParked = false
			}
			if seen >= attemptsBeforeDeadline {
				mu.Lock()
				clockNow = now.Add(drainAckAdmissionBudget + time.Second)
				mu.Unlock()
			}
			// The bare-"city" storeref refusal shape: authorization permanently
			// answers (false, nil), which is indistinguishable from transient.
			return errSessionStartPoolDrainAckPending
		},
		Observer: func(result sessionStartReconcileResult) {
			if result.Outcome != sessionStartReconcileDeadlineExceeded {
				return
			}
			select {
			case released <- result:
			default:
			}
		},
		RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](0, 0),
		Now: func() time.Time {
			mu.Lock()
			defer mu.Unlock()
			return clockNow
		},
		Stderr: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatalf("start controller: %v", err)
	}
	defer controller.Stop()

	lease := routedWorkPoolDrainAckLease{
		SessionID: "gc-drain-1", InstanceToken: "tok-1",
		RequesterSessionID: "gc-drain-1", RequesterInstanceToken: "tok-1",
		ControllerGeneration: 1, PoolTarget: "worker", WorkID: "gc-work-1",
		SourceStore: "city:test", MembershipRevision: 1,
	}
	if _, err := controller.AdmitPoolDrainAck(lease); err != nil {
		t.Fatalf("admit drain ack: %v", err)
	}

	var result sessionStartReconcileResult
	select {
	case result = <-released:
	case <-time.After(30 * time.Second):
		mu.Lock()
		seen := attempts
		mu.Unlock()
		t.Fatalf("a permanently refused drain-ack never released its admission after %d attempts; the obligation is unbounded and legacy stays fenced out of the row", seen)
	}
	if !fencedWhileParked {
		t.Fatal("the controller released the drain-ack fence before the drain's own deadline")
	}
	mu.Lock()
	seen := attempts
	mu.Unlock()
	if seen <= 2 {
		t.Fatalf("reconcile ran %d times, want more than maxRetries: a drain-ack obligation is bounded by the drain deadline, not by a retry count", seen)
	}
	if result.Admission.PoolDrainAck == nil {
		t.Fatalf("released result = %+v, want the drain-ack lease carried on the released admission", result)
	}
	if result.DrainAckRefusals < 1 {
		t.Fatalf("released result carried %d consecutive refusals, want the count the diagnostic throttles on", result.DrainAckRefusals)
	}
	if controller.ownsPoolDrainAckStop("gc-drain-1", "tok-1") {
		t.Fatal("the drain-ack fence survived the deadline; legacy stays excluded from a row nobody is finishing")
	}
	if controller.holdsAnyAdmission("gc-drain-1") {
		t.Fatal("the admission survived its deadline release")
	}
	if !controller.TakeAuditRequest() {
		t.Fatal("the deadline release did not arm an authoritative audit")
	}
}

// TestSessionStartControllerAppliesAnAuthorizedDrainAckWithinTheDeadlineOnce is
// the paired positive: an acknowledgement that IS authorized before the deadline
// still applies exactly once and releases its admission normally.
func TestSessionStartControllerAppliesAnAuthorizedDrainAckWithinTheDeadlineOnce(t *testing.T) {
	now := time.Date(2026, 8, 9, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	applied := 0
	succeeded := make(chan sessionStartReconcileResult, 1)
	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers: 1, MaxDistinct: 4, MaxRetries: 2,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			mu.Lock()
			applied++
			seen := applied
			mu.Unlock()
			if seen < 2 {
				return errSessionStartPoolDrainAckPending
			}
			return nil
		},
		Observer: func(result sessionStartReconcileResult) {
			if result.Outcome == sessionStartReconcileDeadlineExceeded {
				t.Errorf("an acknowledgement authorized inside the deadline was released as deadline_exceeded: %+v", result)
			}
			if result.Outcome != sessionStartReconcileSucceeded {
				return
			}
			select {
			case succeeded <- result:
			default:
			}
		},
		RateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](0, 0),
		Now:         func() time.Time { return now },
		Stderr:      &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("new controller: %v", err)
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatalf("start controller: %v", err)
	}
	defer controller.Stop()

	lease := routedWorkPoolDrainAckLease{
		SessionID: "gc-drain-2", InstanceToken: "tok-2",
		RequesterSessionID: "gc-drain-2", RequesterInstanceToken: "tok-2",
		ControllerGeneration: 1, PoolTarget: "worker", WorkID: "gc-work-2",
		SourceStore: "city:test", MembershipRevision: 1,
	}
	if _, err := controller.AdmitPoolDrainAck(lease); err != nil {
		t.Fatalf("admit drain ack: %v", err)
	}
	select {
	case <-succeeded:
	case <-time.After(30 * time.Second):
		t.Fatal("an acknowledgement authorized inside the deadline never applied")
	}
	mu.Lock()
	seen := applied
	mu.Unlock()
	if seen != 2 {
		t.Fatalf("reconcile ran %d times, want exactly 2 (one refusal, one authorized apply)", seen)
	}
	if controller.holdsAnyAdmission("gc-drain-2") {
		t.Fatal("an authorized drain-ack left its admission behind")
	}
}

// seedReacquiredWakeDrain seeds the field's shape for ga-f7v2ft.179: a drain
// that has already signaled (GC_DRAIN_ACK set, so the very next advance carries
// the row into the stop leg) on a session the tick's wake evaluation now says
// should be awake for `reasons`.
//
// The acknowledgement is what makes the fixture load-bearing rather than
// decorative: without a cancel arm above it the handler discovers the ack, marks
// drain_ack_stop_pending, and the session is killed. The rescue and the kill are
// therefore one dispatch apart, which is exactly the field report.
func seedReacquiredWakeDrain(t *testing.T, env *reconcilerTestEnv, drainReason string, reasons []WakeReason) (*deadRuntimeProvider, beads.Bead, exactSessionStartParams) {
	t.Helper()
	provider, bead := seedDrainingSession(t, env, drainReason)
	if err := provider.SetMeta(exactDrainAdvanceTestSessionName, "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta(GC_DRAIN_ACK): %v", err)
	}
	env.dt.get(bead.ID).ackSet = true
	params := newExactDrainAdvanceParams(env, provider)
	params.SessionWakeEvaluations = func() map[string]wakeEvaluation {
		if reasons == nil {
			return map[string]wakeEvaluation{bead.ID: {}}
		}
		return map[string]wakeEvaluation{bead.ID: {Reasons: reasons}}
	}
	return provider, bead, params
}

// assertExactDrainRescued asserts the whole rescue. The stop-pending check comes
// FIRST because it is the one assertion that discriminates: the stop leg retires
// the tracker entry too, so "drain intent is gone" is satisfied by both the
// rescue and the kill and proves nothing on its own.
func assertExactDrainRescued(t *testing.T, env *reconcilerTestEnv, provider *deadRuntimeProvider, bead beads.Bead) {
	t.Helper()
	if isDrainAckStopPendingInfo(readSessionInfoForTest(t, env, bead.ID)) {
		t.Fatal("the row reached drain_ack_stop_pending instead of being rescued; the cancel arms run above the stop leg")
	}
	if ack, _ := provider.GetMeta(exactDrainAdvanceTestSessionName, "GC_DRAIN_ACK"); ack != "" {
		t.Fatalf("GC_DRAIN_ACK = %q, want cleared: a stale ack kills the rescued session on the next drain-ack check", ack)
	}
	if !provider.IsRunning(exactDrainAdvanceTestSessionName) {
		t.Fatal("the rescued session's runtime was stopped")
	}
	if state := env.dt.get(bead.ID); state != nil {
		t.Fatalf("drain = %+v, want canceled for a reacquired wake reason", state)
	}
}

// TestExactDrainAdvanceCancelsForAnyReacquiredWakeReason is ga-f7v2ft.179's RED.
// The fleet drain scan has THREE cancel arms; the keyed advance had two. Arms 1
// and 2 read WakePending and WakeWork off the wake evaluation, so the other seven
// reasons in the WakeReason vocabulary — config, create, session, keep-warm,
// attached, wait, pin — never canceled a keyed drain. A session that got pinned,
// attached to, or became wait-ready while draining ran to its deadline and was
// force-stopped where the fleet scan would have spared it, and under
// session_reconciler=auto the keyed family holds the key so legacy never covers
// the row.
//
// Every one of the seven is exercised, because the gap was the vocabulary and
// not any single reason.
func TestExactDrainAdvanceCancelsForAnyReacquiredWakeReason(t *testing.T) {
	for _, reason := range []WakeReason{
		WakeConfig, WakeCreate, WakeSession, WakeKeepWarm, WakeAttached, WakeWait, WakePin,
	} {
		t.Run(string(reason), func(t *testing.T) {
			env := newReconcilerTestEnv()
			provider, bead, params := seedReacquiredWakeDrain(t, env, "idle", []WakeReason{reason})

			handled, owner, err := dispatchExactDrainAdvance(t, env, params, bead.ID)
			if !handled || err != nil || owner != exactSessionStartKeyedOwner {
				t.Fatalf("cancel leg: handled=%v owner=%v err=%v", handled, owner, err)
			}
			assertExactDrainRescued(t, env, provider, bead)
		})
	}
}

// TestExactDrainAdvanceKeepsDrainsTheFleetScanAlsoRefusesToCancel is the control
// the rescue above is meaningless without. The fleet scan's third arm is gated on
// drainReasonCancelable, and on a session with NO reacquired reason it does not
// fire at all. Both negatives have to keep behaving exactly as they did:
//
//   - the four non-cancelable drain reasons are the ones whose whole purpose is
//     to survive a wake signal — an execution-stalled seat is alive, awake and
//     holding a claim by construction, so a cancel there is the wedge this lane
//     exists to end;
//   - a cancelable drain with an empty reason set is the ordinary path to the
//     stop, and a fix that rescued it would have disabled draining altogether.
//
// Each case must fail DIFFERENTLY from the rescue: the row reaches the
// stop-pending transition instead of surviving.
func TestExactDrainAdvanceKeepsDrainsTheFleetScanAlsoRefusesToCancel(t *testing.T) {
	cases := []struct {
		name        string
		drainReason string
		reasons     []WakeReason
	}{
		{name: "config-drift", drainReason: "config-drift", reasons: []WakeReason{WakePin}},
		{name: "orphaned", drainReason: "orphaned", reasons: []WakeReason{WakePin}},
		{name: "suspended", drainReason: "suspended", reasons: []WakeReason{WakePin}},
		{name: "execution-stalled", drainReason: executionStalledDrainReason, reasons: []WakeReason{WakePin}},
		{name: "cancelable drain with no reacquired reason", drainReason: "idle", reasons: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			provider, bead, params := seedReacquiredWakeDrain(t, env, tc.drainReason, tc.reasons)

			handled, owner, err := dispatchExactDrainAdvance(t, env, params, bead.ID)
			if !handled || err != nil || owner != exactSessionStartKeyedOwner {
				t.Fatalf("stop-pending leg: handled=%v owner=%v err=%v", handled, owner, err)
			}
			if !isDrainAckStopPendingInfo(readSessionInfoForTest(t, env, bead.ID)) {
				t.Fatalf("row = %+v, want drain_ack_stop_pending: this drain is not cancelable by a wake reason",
					readSessionInfoForTest(t, env, bead.ID))
			}
			if env.dt.get(bead.ID) != nil {
				t.Fatal("drain intent survived the stop-pending transition")
			}
			if !provider.IsRunning(exactDrainAdvanceTestSessionName) {
				t.Fatal("the stop-pending transition stopped the runtime synchronously; the stop leg is the drain-ack stop's")
			}
		})
	}
}

// TestExactDrainAdvanceDeclinesTheWakeCancelWithoutAPublishedView pins the
// fail-safe direction of the fleet-shaped read. No accessor, or a view no tick
// has published, must leave the arm inert rather than guess — the fleet scan's
// own answer for a row its wakeEvals does not carry.
func TestExactDrainAdvanceDeclinesTheWakeCancelWithoutAPublishedView(t *testing.T) {
	for _, tc := range []struct {
		name string
		view func() map[string]wakeEvaluation
	}{
		{name: "no accessor", view: nil},
		{name: "nothing published yet", view: func() map[string]wakeEvaluation { return nil }},
		{name: "key absent from the view", view: func() map[string]wakeEvaluation {
			return map[string]wakeEvaluation{"some-other-session": {Reasons: []WakeReason{WakePin}}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			provider, bead, params := seedReacquiredWakeDrain(t, env, "idle", []WakeReason{WakePin})
			params.SessionWakeEvaluations = tc.view

			handled, owner, err := dispatchExactDrainAdvance(t, env, params, bead.ID)
			if !handled || err != nil || owner != exactSessionStartKeyedOwner {
				t.Fatalf("stop-pending leg: handled=%v owner=%v err=%v", handled, owner, err)
			}
			if !isDrainAckStopPendingInfo(readSessionInfoForTest(t, env, bead.ID)) {
				t.Fatal("the arm fired without a published wake verdict; an unpublished view must decline, not rescue")
			}
			if !provider.IsRunning(exactDrainAdvanceTestSessionName) {
				t.Fatal("the stop-pending transition stopped the runtime synchronously")
			}
		})
	}
}

// realDetectorWakeEvals publishes the tick's wake projection the way the SWEEP
// publishes it — detectorAwakeSet over the authoritative row — and returns the
// result unedited. Every other test in this file hands the handler a view it
// wrote by hand, which is exactly how a permanently-empty AttachedSessions went
// unnoticed: a hand-seeded WakeAttached proves the handler reads the view, not
// that any tick can put WakeAttached in it.
func realDetectorWakeEvals(t *testing.T, env *reconcilerTestEnv, provider runtime.Provider, id string) map[string]wakeEvaluation {
	t.Helper()
	info := readSessionInfoForTest(t, env, id)
	_, evals := detectorAwakeSet(
		detectorSweepInput{
			CityName: env.cfg.EffectiveCityName(),
			Cfg:      env.cfg,
			Provider: provider,
			Clock:    env.clk,
		},
		[]sessionpkg.ReconcileSession{{Info: info}},
		map[string]detectorLivenessBits{info.ID: {Probed: true, Running: true, Alive: true}},
		env.clk.Now(),
	)
	return evals
}

// TestDetectorWakeProjectionCannotCarryAttachment is ga-f7v2ft.161's structural
// proof, and the reason the rescue below has to be handler-side. The two
// AwakeInput builders are fed the SAME attached row and the SAME provider: the
// legacy bridge probes attachment and reports WakeAttached, the detector's
// builder leaves AwakeInput.AttachedSessions empty by design (fleet-wide provider
// I/O the sweep may not pay), so ComputeAwakeSet never reaches its "attached"
// rung and the published projection cannot carry the reason at all.
//
// The legacy leg is the control. Without it a detector projection that carries
// nothing for an unrelated reason — a broken fixture, a row the builder dropped —
// would read as the same "vacancy" and prove nothing.
func TestDetectorWakeProjectionCannotCarryAttachment(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead := seedDrainingSession(t, env, "idle")
	provider.SetAttached(exactDrainAdvanceTestSessionName, true)
	info := readSessionInfoForTest(t, env, bead.ID)

	legacyInput := buildAwakeInputFromReconciler(
		env.cfg, "", []sessionpkg.Info{info}, nil, nil, nil, nil, nil, nil, nil,
		[]wakeTarget{{info: info, alive: true}}, provider, env.clk.Now(),
	)
	legacy := awakeSetToWakeEvals(ComputeAwakeSet(legacyInput), legacyInput.SessionBeads)
	if !containsWakeReason(legacy[bead.ID].Reasons, WakeAttached) {
		t.Fatalf("the legacy bridge answered %v for an attached row, want WakeAttached.\n"+
			"The fixture no longer exercises the divergence, so the detector leg below proves nothing.",
			legacy[bead.ID].Reasons)
	}

	detector := realDetectorWakeEvals(t, env, provider, bead.ID)
	if containsWakeReason(detector[bead.ID].Reasons, WakeAttached) {
		t.Fatal("the detector projection now carries WakeAttached.\n" +
			"That is a welcome change, but it means the sweep pays the attachment probe fleet-wide: " +
			"revisit D-DRAIN's handler-side re-pay and the AttachedSessions census exemption before keeping it.")
	}
}

// TestExactDrainAdvanceRescuesAnAttachedSessionThroughTheRealPublication is
// ga-f7v2ft.161's RED for council finding B2. The .179 arm rescues a drain whose
// session reacquired any wake reason, but it reads only the published projection —
// and per the test above that projection can never say "attached". Under
// session_reconciler=auto the fleet scan skips a row the keyed controller holds an
// advance admission for, so nothing else covers it either: a keyed idle drain
// force-stopped the session a person was sitting in.
//
// The drain is already acknowledged, so absent a cancel arm the very next advance
// marks drain_ack_stop_pending and the session dies. Rescue and kill are one
// dispatch apart, which is the field report.
//
// The unattached control has to fail DIFFERENTLY — it reaches stop-pending — or a
// fix that simply stopped draining would pass.
func TestExactDrainAdvanceRescuesAnAttachedSessionThroughTheRealPublication(t *testing.T) {
	for _, tc := range []struct {
		name     string
		attached bool
	}{
		{name: "a human is attached", attached: true},
		{name: "control: nobody is attached", attached: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newReconcilerTestEnv()
			provider, bead := seedDrainingSession(t, env, "idle")
			if err := provider.SetMeta(exactDrainAdvanceTestSessionName, "GC_DRAIN_ACK", "1"); err != nil {
				t.Fatalf("SetMeta(GC_DRAIN_ACK): %v", err)
			}
			env.dt.get(bead.ID).ackSet = true
			provider.SetAttached(exactDrainAdvanceTestSessionName, tc.attached)

			published := realDetectorWakeEvals(t, env, provider, bead.ID)
			eval, ok := published[bead.ID]
			if !ok {
				t.Fatal("the real publication carried no entry for the drained row; the arm would decline on an absent key rather than on an empty verdict, which is not the premise under test")
			}
			if len(eval.Reasons) != 0 {
				t.Fatalf("the real publication gave the row wake reasons %v.\n"+
					"The published-view half of the arm would fire on its own and the attachment re-pay would never be exercised.",
					eval.Reasons)
			}

			params := newExactDrainAdvanceParams(env, provider)
			params.SessionWakeEvaluations = func() map[string]wakeEvaluation { return published }

			handled, owner, err := dispatchExactDrainAdvance(t, env, params, bead.ID)
			if !handled || err != nil || owner != exactSessionStartKeyedOwner {
				t.Fatalf("advance: handled=%v owner=%v err=%v", handled, owner, err)
			}

			if tc.attached {
				assertExactDrainRescued(t, env, provider, bead)
				return
			}
			if !isDrainAckStopPendingInfo(readSessionInfoForTest(t, env, bead.ID)) {
				t.Fatal("an unattended acknowledged drain did not reach stop-pending; the rescue fires for rows nobody is attached to")
			}
			if env.dt.get(bead.ID) != nil {
				t.Fatal("drain intent survived the stop-pending transition")
			}
		})
	}
}

// TestExactDrainAdvanceAttachmentRescueRespectsTheFleetScansCancelGate is the
// second control. The handler-side re-pay is an extra SOURCE for cancel arm 3,
// never a wider gate: legacy's arm is bounded by drainReasonCancelable, and an
// execution-stalled seat is alive, awake and holding a claim by construction, so
// rescuing one on attachment would restore the wedge this lane exists to end.
func TestExactDrainAdvanceAttachmentRescueRespectsTheFleetScansCancelGate(t *testing.T) {
	for _, reason := range []string{"config-drift", "orphaned", "suspended", executionStalledDrainReason} {
		t.Run(reason, func(t *testing.T) {
			env := newReconcilerTestEnv()
			provider, bead := seedDrainingSession(t, env, reason)
			if err := provider.SetMeta(exactDrainAdvanceTestSessionName, "GC_DRAIN_ACK", "1"); err != nil {
				t.Fatalf("SetMeta(GC_DRAIN_ACK): %v", err)
			}
			env.dt.get(bead.ID).ackSet = true
			provider.SetAttached(exactDrainAdvanceTestSessionName, true)

			published := realDetectorWakeEvals(t, env, provider, bead.ID)
			params := newExactDrainAdvanceParams(env, provider)
			params.SessionWakeEvaluations = func() map[string]wakeEvaluation { return published }

			handled, owner, err := dispatchExactDrainAdvance(t, env, params, bead.ID)
			if !handled || err != nil || owner != exactSessionStartKeyedOwner {
				t.Fatalf("advance: handled=%v owner=%v err=%v", handled, owner, err)
			}
			if !isDrainAckStopPendingInfo(readSessionInfoForTest(t, env, bead.ID)) {
				t.Fatalf("an attached %s drain was canceled; this drain reason is not cancelable by a wake reason in the fleet scan either", reason)
			}
		})
	}
}

// TestExactDrainAdvanceRefusesWhenLivenessIsIncomplete pins the one place the
// keyed arm is deliberately STRICTER than the fleet scan: the fleet loop treats
// an unreadable running-probe as "exited" and completes the drain, which writes
// asleep onto a row whose agent may still be working. The keyed arm refuses with
// zero effect and re-detects.
func TestExactDrainAdvanceRefusesWhenLivenessIsIncomplete(t *testing.T) {
	env := newReconcilerTestEnv()
	provider, bead := seedDrainingSession(t, env, "idle")
	provider.incomplete = true
	params := newExactDrainAdvanceParams(env, provider)

	handled, _, err := dispatchExactDrainAdvance(t, env, params, bead.ID)
	if !handled {
		t.Fatal("the D-DRAIN seam did not claim a row carrying drain intent")
	}
	if err == nil || !strings.Contains(err.Error(), "liveness observation is incomplete") {
		t.Fatalf("err = %v, want an incomplete-liveness refusal", err)
	}
	if env.dt.get(bead.ID) == nil {
		t.Fatal("the refusal retired the drain intent; it must be level-triggered")
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["state"] == "asleep" {
		t.Fatal("an unproven absence completed the drain")
	}
}

// TestExactDrainAdvanceAliveSessionProceedsOnIncompleteScan is the second half
// of the field wedge TestExactSleepDrainAliveSessionProceedsOnIncompleteScan
// pins on D-SLEEP: once a drain begins against an ALIVE session, the advance
// must be able to send its deferred drain signal even though the live pane
// withholds the tmux-absence license and keeps the /proc sweep incomplete.
// Scan completeness proves absence; every destructive arm downstream (the
// dead-completion above, the timeout stop's confirm) re-proves absence behind
// its own COMPLETE observation, so a positive observation is decisive here.
func TestExactDrainAdvanceAliveSessionProceedsOnIncompleteScan(t *testing.T) {
	const name = exactDrainAdvanceTestSessionName
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: name, StartCommand: "true"}},
	}
	provider := &aliveIncompleteObservationProvider{Fake: env.sp}
	if err := provider.Start(t.Context(), name, runtime.Config{}); err != nil {
		t.Fatalf("start runtime for %q: %v", name, err)
	}
	bead := env.createSessionBead(name, name)
	env.markSessionActive(&bead)
	now := env.clk.Now()
	env.dt.set(bead.ID, &drainState{
		startedAt:  now.Add(-10 * time.Second),
		deadline:   now.Add(defaultDrainTimeout),
		reason:     "idle",
		generation: 1,
	})
	params := newExactDrainAdvanceParams(env, provider)

	handled, _, err := dispatchExactDrainAdvance(t, env, params, bead.ID)
	if !handled {
		t.Fatal("the D-DRAIN seam did not claim a row carrying drain intent")
	}
	if err != nil {
		t.Fatalf("an alive session's incomplete scan parked the drain advance: %v", err)
	}
	if ack, _ := provider.GetMeta(name, "GC_DRAIN_ACK"); ack != "1" {
		t.Fatalf("GC_DRAIN_ACK = %q, want the deferred drain signal sent on the first advance past a positive observation", ack)
	}
	if env.dt.get(bead.ID) == nil {
		t.Fatal("the advance retired the drain intent; only completion or cancel may")
	}
	stored, err := env.store.Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Metadata["state"] == "asleep" {
		t.Fatal("a positive observation completed the drain; completion needs a proven-dead COMPLETE observation")
	}
}
