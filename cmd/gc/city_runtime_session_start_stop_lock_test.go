package main

import (
	"context"
	"io"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/rollout"
)

// TestStopSessionStartControllerDrainsWithoutHoldingTheSessionStartLock pins
// ga-f7v2ft.143: the keyed controller's shutdown must not wait for its workers
// while holding the lock those workers need.
//
// stopSessionStartController held cr.sessionStartMu across controller.Stop(),
// and Stop drains the workqueue with ShutDownWithDrain, which blocks until every
// in-flight item finishes. The in-flight item is a reconcile that reads the
// published fleet views — desiredSessionNamesView (reached in production via
// reconcileExactSessionStartWithOwner → reconcileExactSessionDetectorFamily →
// exactSessionOrphanCloseCandidate) and providerHealthSnapshotView — and both
// take that same mutex. Cleanup held the lock and waited for the worker; the
// worker waited for the lock.
//
// That is the whole of the bead's "no slow middle" signature: if the worker
// happens to be idle when stop runs, the drain returns instantly and the test
// finishes in seconds; if a worker is mid-reconcile inside any
// sessionStartMu-taking view, it never returns at all.
//
// Two invariants are asserted while the drain is in flight, both deterministic
// rather than timed: the state lock is FREE (so the parked worker can finish)
// and the lifecycle lock is HELD (so a concurrent ensure cannot build a second
// controller beside the one still draining). Releasing the state lock without
// the lifecycle lock would trade a deadlock for a double owner.
//
// The pre-fix failure of the drain itself is a hang, so every wait here is an
// awaitClose/awaitCond against the package hang budget, and nothing in the test
// can block a cleanup: the controller is deliberately NOT registered with
// t.Cleanup(controller.Stop), because a second Stop blocks on the first one's
// sync.Once and would turn a bounded failure into a whole-package timeout.
func TestStopSessionStartControllerDrainsWithoutHoldingTheSessionStartLock(t *testing.T) {
	cr := &CityRuntime{stdout: io.Discard, stderr: io.Discard}

	entered := make(chan struct{})
	viewed := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorker := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWorker)

	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 1,
		MaxRetries:  0,
		Reconcile: func(context.Context, sessionStartAdmission) error {
			close(entered)
			<-release
			cr.desiredSessionNamesView()
			cr.providerHealthSnapshotView()
			close(viewed)
			return nil
		},
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("newSessionStartController: %v", err)
	}
	if err := controller.Start(context.Background()); err != nil {
		t.Fatalf("start session-start controller: %v", err)
	}

	cr.sessionStartMu.Lock()
	cr.sessionStartController = controller
	cr.sessionStartOwnership = sessionStartOwnershipKeyed
	cr.sessionStartMode = rollout.Auto
	cr.sessionStartMu.Unlock()

	if outcome, err := controller.Admit("gcs-lock-order", sessionStartAdmissionInProcess); err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit session-start key = (%q, %v), want accepted", outcome, err)
	}
	awaitClose(t, entered, "session-start reconcile to enter")

	stopDone := make(chan struct{})
	go func() {
		cr.stopSessionStartController()
		close(stopDone)
	}()
	// Stop has begun as soon as it flips the controller's stopped flag, which it
	// does before draining. That signal holds both before and after the fix,
	// unlike "the ownership lock is held" — which is exactly what the fix
	// removes, and so cannot sequence this test.
	awaitCond(t, func() bool {
		controller.mu.Lock()
		defer controller.mu.Unlock()
		return controller.stopped
	}, "controller stop to begin")

	// The worker is still parked, so the drain cannot have finished: whatever
	// these two locks hold right now is what stop holds ACROSS the drain.
	if cr.sessionStartMu.TryLock() {
		cr.sessionStartMu.Unlock()
	} else {
		t.Fatal("stop is draining while holding sessionStartMu, the same lock its in-flight workers take to read the fleet views")
	}
	if cr.sessionStartLifecycleMu.TryLock() {
		cr.sessionStartLifecycleMu.Unlock()
		t.Fatal("stop is draining without the lifecycle lock, so a concurrent ensure could install a second controller beside it")
	}

	releaseWorker()
	awaitClose(t, viewed, "the in-flight worker to read the fleet views while stop drains")
	awaitClose(t, stopDone, "controller stop to finish draining")

	if got := cr.sessionStartOwnershipState(); got != sessionStartOwnershipLegacy {
		t.Fatalf("ownership after stop = %q, want %q", got, sessionStartOwnershipLegacy)
	}
	cr.sessionStartMu.Lock()
	remaining := cr.sessionStartController
	cr.sessionStartMu.Unlock()
	if remaining != nil {
		t.Fatal("stop left the keyed controller pointer installed")
	}
}
