package main

import (
	"context"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// waitAdvanceStandDownFixture is the ga-zo9h3 world: the journey's manual
// wait-hold row (session_origin=manual, no wake cause — so no wake family is a
// candidate for it), its durable dependency wait, and a keyed CityRuntime whose
// session-start controller can hold the wait-dependency claim.
type waitAdvanceStandDownFixture struct {
	env          *reconcilerTestEnv
	cr           *CityRuntime
	controller   *sessionStartController
	sessionID    string
	waitID       string
	dependencyID string
}

func newWaitAdvanceStandDownFixture(t *testing.T) *waitAdvanceStandDownFixture {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "true"}},
	}
	dependency, err := env.store.Create(beads.Bead{Title: "keyed start dependency"})
	if err != nil {
		t.Fatalf("create durable dependency: %v", err)
	}
	target := env.createSessionBead("worker", "worker")
	env.setSessionMetadata(&target, map[string]string{
		"state":              string(sessionpkg.StateAsleep),
		"session_origin":     "manual",
		"continuation_epoch": "7",
		"wait_hold":          "true",
		"sleep_intent":       string(sessionpkg.SleepReasonWaitHold),
		"sleep_reason":       string(sessionpkg.SleepReasonWaitHold),
	})
	wait, err := env.store.Create(sessionWaitShadowBead(target.ID, dependency.ID))
	if err != nil {
		t.Fatalf("create durable wait: %v", err)
	}
	if err := env.store.SetMetadata(wait.ID, "registered_epoch", "7"); err != nil {
		t.Fatalf("stamp registered epoch: %v", err)
	}
	// The dependency is closed: from here every legacy pass would advance the
	// wait and turn it into start demand.
	if err := env.store.Close(dependency.ID); err != nil {
		t.Fatalf("close durable dependency: %v", err)
	}

	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 4,
		MaxRetries:  0,
		Stderr:      io.Discard,
		Reconcile: func(ctx context.Context, _ sessionStartAdmission) error {
			<-ctx.Done()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("create session-start controller: %v", err)
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatalf("start session-start controller: %v", err)
	}
	t.Cleanup(controller.Stop)

	return &waitAdvanceStandDownFixture{
		env:          env,
		controller:   controller,
		sessionID:    target.ID,
		waitID:       wait.ID,
		dependencyID: dependency.ID,
		cr: &CityRuntime{
			cityPath:              "test-city",
			cityName:              "test-city",
			cfg:                   env.cfg,
			sp:                    env.sp,
			stderr:                io.Discard,
			sessionStartOwnership: sessionStartOwnershipKeyed,
			sessionStartMode:      rollout.Auto,
			// The runtime holds the controller directly: this exercises the
			// boundary predicate, not the shadow producer that feeds it.
			sessionStartController: controller,
		},
	}
}

// admitKeyedWaitDependency lands the keyed claim on the SESSION. waitID names
// the wait its certificate was minted for, which is not necessarily the wait a
// legacy pass is looking at.
func (f *waitAdvanceStandDownFixture) admitKeyedWaitDependency(t *testing.T, waitID string) {
	t.Helper()
	lease := sessionWaitDependencyStartLease{
		WaitID:               waitID,
		SessionID:            f.sessionID,
		DepIDs:               []string{f.dependencyID},
		DepMode:              "all",
		RegisteredEpoch:      "7",
		WaitRevision:         1,
		SessionRevision:      1,
		IndexGeneration:      1,
		ControllerGeneration: 1,
		Operation:            "wait-dependency-operation",
	}
	outcome, err := f.controller.AdmitWaitDependency(lease)
	if err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit keyed wait-dependency claim = (%q, %v), want accepted", outcome, err)
	}
	if !f.cr.ownsSessionWaitDependencyStart(f.sessionID) {
		t.Fatal("the keyed claim did not take the session's start boundary")
	}
}

// runLegacyWaitAdvance is the legacy tick's wait-advance boundary, wired the way
// CityRuntime.reconcileSessionBeads wires it.
func (f *waitAdvanceStandDownFixture) runLegacyWaitAdvance(t *testing.T) map[string]bool {
	t.Helper()
	ready, err := prepareWaitWakeStateWithSnapshot(
		sessionFrontDoor(f.env.store),
		newWaitDependencyStoreSet(f.env.store, nil, beads.GraphStore{}),
		beads.NudgesStore{Store: f.env.store},
		f.env.clk.Now(),
		nil,
		f.cr.keyedWaitAdvanceExcluded,
	)
	if err != nil {
		t.Fatalf("prepare legacy wait state: %v", err)
	}
	return ready
}

func (f *waitAdvanceStandDownFixture) waitState(t *testing.T) sessionpkg.WaitInfo {
	t.Helper()
	wait, err := sessionFrontDoor(f.env.store).GetWait(f.waitID)
	if err != nil {
		t.Fatalf("read durable wait: %v", err)
	}
	return wait
}

// TestLegacyWaitAdvanceStandsDownForKeyedSessionClaim is the ga-zo9h3 RED. The
// boundary's installed seam is keyed on the WAIT's identity, and legacy's own
// advance is what moves that identity — so the moment the wait row drifts from
// the certificate the keyed family is holding, the seam falls open while the
// keyed claim on the SESSION is still live. Legacy then advances the wait and
// the same tick's forward pass starts the row, which is why the journey's
// keyed wait_dependency commit never lands and the durable wait ends in
// legacy's terminal shape instead of resting at ready/open. The boundary must
// consult the session-level arm the start boundary already consults.
func TestLegacyWaitAdvanceStandsDownForKeyedSessionClaim(t *testing.T) {
	t.Run("claim-names-another-wait", func(t *testing.T) {
		f := newWaitAdvanceStandDownFixture(t)
		f.admitKeyedWaitDependency(t, "other-wait")

		if f.cr.ownsSessionWaitDependencyWait(f.waitState(t)) {
			t.Fatal("test premise: the wait-identity seam already covers this wait")
		}
		ready := f.runLegacyWaitAdvance(t)
		if ready[f.sessionID] {
			t.Fatal("legacy turned a keyed-claimed session's wait into start demand")
		}
		if got := f.waitState(t).State; got != waitStatePending {
			t.Fatalf("durable wait state = %q, want pending until the keyed claim commits", got)
		}
	})

	t.Run("wait-already-in-legacy-ready-shape", func(t *testing.T) {
		f := newWaitAdvanceStandDownFixture(t)
		f.admitKeyedWaitDependency(t, f.waitID)
		// MarkWaitReady clears ready_owner and ready_operation, so a wait an
		// earlier legacy pass advanced no longer matches the certificate the
		// keyed family still holds.
		if err := sessionFrontDoor(f.env.store).MarkWaitReady(f.waitID, f.env.clk.Now()); err != nil {
			t.Fatalf("advance the wait into legacy's ready shape: %v", err)
		}
		if f.cr.ownsSessionWaitDependencyWait(f.waitState(t)) {
			t.Fatal("test premise: the wait-identity seam still covers a legacy-shaped ready wait")
		}

		ready := f.runLegacyWaitAdvance(t)
		if ready[f.sessionID] {
			t.Fatal("legacy turned a keyed-claimed session's ready wait into start demand")
		}
	})
}

// TestLegacyWaitAdvanceProceedsAfterKeyedClaimReleases is the no-lapse RED. The
// stand-down must be level-triggered on a live claim and nothing else: when the
// keyed reservation is released or retired — the
// handleSessionWaitDependencyAdmissionFailure path — legacy advances the wait on
// its very next pass. Fencing on candidacy instead of on a live claim would
// strand every wait no family can certify.
func TestLegacyWaitAdvanceProceedsAfterKeyedClaimReleases(t *testing.T) {
	f := newWaitAdvanceStandDownFixture(t)
	f.admitKeyedWaitDependency(t, f.waitID)
	if ready := f.runLegacyWaitAdvance(t); ready[f.sessionID] {
		t.Fatal("test premise: legacy advanced the wait while the keyed claim was live")
	}

	f.controller.Stop()
	awaitCond(t, func() bool { return !f.cr.ownsSessionWaitDependencyStart(f.sessionID) },
		"the keyed wait-dependency claim to release")

	ready := f.runLegacyWaitAdvance(t)
	if !ready[f.sessionID] {
		t.Fatal("legacy did not advance the wait after the keyed claim released: the wait is stranded")
	}
	if got := f.waitState(t).State; got != waitStateReady {
		t.Fatalf("durable wait state after the release = %q, want ready", got)
	}
}

// TestLegacyWaitAdvanceIgnoresUnclaimedSessions keeps the widened arm honest:
// with no keyed claim anywhere, the boundary behaves exactly as before.
func TestLegacyWaitAdvanceIgnoresUnclaimedSessions(t *testing.T) {
	f := newWaitAdvanceStandDownFixture(t)
	ready := f.runLegacyWaitAdvance(t)
	if !ready[f.sessionID] {
		t.Fatal("legacy did not advance an unclaimed ready dependency wait")
	}
	if got := f.waitState(t).State; got != waitStateReady {
		t.Fatalf("durable wait state = %q, want ready", got)
	}
}
