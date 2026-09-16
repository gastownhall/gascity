package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// These tests express the acceptance criteria for ga-8x82f4: the poll loop in
// cmdNudgePoll must consult the cheap nudgePollTargetHasDueWork check BEFORE
// running the expensive nudgeObserveTarget/tryDeliverQueuedNudgesByPoller
// pair, skipping the expensive pair entirely when nothing is queued for this
// target -- while still observing often enough that a poller whose session
// has died is never immortalized by an empty queue, and without regressing
// delivery latency for due work. See ga-8x82f4 and ga-aa01xj for the measured
// cost and full fix spec.

// TestCmdNudgePollSkipsDeliveryAttemptWithEmptyQueue proves that with an
// empty queue for the target, a poll tick performs no delivery attempt at
// all. nudgePollDeliverQueued is the seam the GREEN step wires in front of
// tryDeliverQueuedNudgesByPoller's call site so this is observable from a
// black-box test; it does not exist yet, which is what makes this RED.
func TestCmdNudgePollSkipsDeliveryAttemptWithEmptyQueue(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "Session: worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"state":        string(session.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("store.Create session: %v", err)
	}
	// Deliberately empty queue: nothing enqueued for "worker".

	deliverCalls := 0
	origDeliver := nudgePollDeliverQueued
	nudgePollDeliverQueued = func(target nudgeTarget, store, sessStore beads.Store, sp runtime.Provider, quiescence time.Duration, obs worker.LiveObservation) (bool, error) {
		deliverCalls++
		return origDeliver(target, store, sessStore, sp, quiescence, obs)
	}
	defer func() { nudgePollDeliverQueued = origDeliver }()

	const liveTicks = 5
	observeCalls := 0
	origObserve := nudgeObserveTarget
	nudgeObserveTarget = func(_ nudgeTarget, _ beads.Store, _ runtime.Provider) (worker.LiveObservation, error) {
		observeCalls++
		if observeCalls <= liveTicks {
			last := time.Now()
			return worker.LiveObservation{Running: true, LastActivity: &last}, nil
		}
		// Empty queue, so shouldKeepNudgePollerAlive lets the poller exit.
		return worker.LiveObservation{Running: false}, nil
	}
	defer func() { nudgeObserveTarget = origObserve }()

	var stdout, stderr bytes.Buffer
	code := cmdNudgePoll([]string{created.ID}, "worker-session", time.Millisecond, 0, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdNudgePoll = %d, want 0; stderr=%s", code, stderr.String())
	}
	if deliverCalls != 0 {
		t.Fatalf("delivery attempts = %d, want 0 (queue is empty; the cheap due-work check must skip the delivery path entirely)", deliverCalls)
	}
}

// TestCmdNudgePollExitsWhenSessionDiesWithEmptyQueue is the immortality
// regression guard: reducing nudgeObserveTarget's cadence while the queue is
// empty must never stop it running altogether, or a poller whose session has
// died would loop forever once its queue drains. Runs cmdNudgePoll on a
// background goroutine and bounds the wait with a timeout instead of calling
// it inline, so a regression fails fast instead of hanging the suite for the
// full test binary timeout.
func TestCmdNudgePollExitsWhenSessionDiesWithEmptyQueue(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "Session: worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"state":        string(session.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("store.Create session: %v", err)
	}
	// Deliberately empty queue -- the shape the immortality regression needs.

	const deadAfter = 5
	observeCalls := 0
	origObserve := nudgeObserveTarget
	nudgeObserveTarget = func(_ nudgeTarget, _ beads.Store, _ runtime.Provider) (worker.LiveObservation, error) {
		observeCalls++
		if observeCalls < deadAfter {
			last := time.Now()
			return worker.LiveObservation{Running: true, LastActivity: &last}, nil
		}
		return worker.LiveObservation{Running: false}, nil
	}
	defer func() { nudgeObserveTarget = origObserve }()

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- cmdNudgePoll([]string{created.ID}, "worker-session", time.Millisecond, 0, true, &stdout, &stderr)
	}()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("cmdNudgePoll = %d, want 0; stderr=%s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cmdNudgePoll did not exit within 5s of its session dying with an empty queue -- reduced observation cadence must never stop entirely (immortality regression, ga-8x82f4)")
	}
}

// TestCmdNudgePollDeliversDueWorkPromptly proves the reordering does not
// regress delivery latency: work enqueued before the poller starts is still
// delivered on the very tick that first observes the session, not delayed
// to some later reduced-cadence observation tick.
func TestCmdNudgePollDeliversDueWorkPromptly(t *testing.T) {
	clearGCEnv(t)
	disableManagedDoltRecoveryForTest(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	writeNamedSessionCityTOML(t, cityDir)
	t.Setenv("GC_CITY", cityDir)

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	created, err := store.Create(beads.Bead{
		Title:  "Session: worker",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name": "worker-session",
			"agent_name":   "worker",
			"template":     "worker",
			"state":        string(session.StateActive),
		},
	})
	if err != nil {
		t.Fatalf("store.Create session: %v", err)
	}

	item := newQueuedNudgeWithOptions("worker", "review the deploy logs", "session", time.Now().Add(-time.Minute), queuedNudgeOptions{
		SessionID: created.ID,
	})
	if err := enqueueQueuedNudgeWithStore(cityDir, beads.NudgesStore{Store: store}, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}

	// cmdNudgePoll constructs its own private runtime.Fake internally (via
	// newSessionProvider), which this test has no way to reach in order to
	// Start "worker-session" on it. A real handle.Nudge(..., Wake:
	// NudgeWakeLiveOnly) call would therefore always find the session not
	// running and decline -- and tryDeliverQueuedNudgesByPoller deliberately
	// releases the claim on a decline so the next tick retries promptly,
	// which would spin this test forever chasing a liveness gate it
	// structurally cannot satisfy. Mock nudgePollDeliverQueued instead (the
	// same seam TestCmdNudgePollSkipsDeliveryAttemptWithEmptyQueue uses) so
	// the test verifies what it actually claims -- that the due item reaches
	// the delivery path on the very first observation -- without depending
	// on that unreachable internal provider. Acking the item here mirrors
	// the real success path's side effect on the queue, so the loop's own
	// due-work/cadence logic (untouched by this mock) sees the same
	// post-delivery state a genuine successful delivery would leave behind.
	deliverCalls := 0
	origDeliver := nudgePollDeliverQueued
	nudgePollDeliverQueued = func(_ nudgeTarget, _, _ beads.Store, _ runtime.Provider, _ time.Duration, _ worker.LiveObservation) (bool, error) {
		deliverCalls++
		if err := ackQueuedNudges(cityDir, []string{item.ID}); err != nil {
			return false, err
		}
		return true, nil
	}
	defer func() { nudgePollDeliverQueued = origDeliver }()

	start := time.Now()
	observeCalls := 0
	origObserve := nudgeObserveTarget
	nudgeObserveTarget = func(_ nudgeTarget, _ beads.Store, _ runtime.Provider) (worker.LiveObservation, error) {
		observeCalls++
		if observeCalls == 1 {
			// Idle long enough to clear the quiescence gate so the due item
			// delivers on this very tick.
			idle := time.Now().Add(-time.Hour)
			return worker.LiveObservation{Running: true, LastActivity: &idle}, nil
		}
		// Queue has drained; let the poller exit.
		return worker.LiveObservation{Running: false}, nil
	}
	defer func() { nudgeObserveTarget = origObserve }()

	var stdout, stderr bytes.Buffer
	code := cmdNudgePoll([]string{created.ID}, "worker-session", time.Millisecond, time.Second, true, &stdout, &stderr)
	elapsed := time.Since(start)
	if code != 0 {
		t.Fatalf("cmdNudgePoll = %d, want 0; stderr=%s", code, stderr.String())
	}
	if deliverCalls != 1 {
		t.Fatalf("delivery attempts = %d, want exactly 1 (the due item must reach the delivery path on the first observation, not a later tick)", deliverCalls)
	}
	if observeCalls != 2 {
		t.Fatalf("observe calls = %d, want exactly 2 (deliver on the first observation, exit on the second) -- due work must not wait for a reduced-cadence observation tick", observeCalls)
	}
	if elapsed > 5*time.Second {
		t.Fatalf("cmdNudgePoll took %s to deliver due work and exit, want well under 5s (latency regression)", elapsed)
	}

	pending, inFlight, _, err := listQueuedNudges(cityDir, "worker", time.Now())
	if err != nil {
		t.Fatalf("listQueuedNudges: %v", err)
	}
	if len(pending) != 0 || len(inFlight) != 0 {
		t.Fatalf("pending=%d inFlight=%d, want both 0 (the due nudge must be delivered and acked on the first tick)", len(pending), len(inFlight))
	}
}
