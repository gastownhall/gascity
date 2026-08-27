package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/rollout"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

// wakeStandDownFixture is the run-13 world: the canonical singleton row sync
// produces for a dependency-bearing single-session agent, carrying a current
// explicit wake, beside a keyed CityRuntime over the same store whose
// session-start controller can hold a certified wake lease.
type wakeStandDownFixture struct {
	env        *reconcilerTestEnv
	cr         *CityRuntime
	controller *sessionStartController
	targetID   string
	dependency sessionpkg.Info
}

// newWakeStandDownFixture wires the fixture with a reconcile that blocks until
// the test ends, so an admitted lease stays retained and observable exactly the
// way it is while the keyed family is starting the row.
func newWakeStandDownFixture(t *testing.T) *wakeStandDownFixture {
	t.Helper()
	return newWakeStandDownFixtureWithReconcile(t, func(ctx context.Context, _ sessionStartAdmission) error {
		<-ctx.Done()
		return nil
	})
}

func newWakeStandDownFixtureWithReconcile(t *testing.T, reconcile func(context.Context, sessionStartAdmission) error) *wakeStandDownFixture {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = wakeFamilyCityConfig()

	dependency := env.createSessionBead("database", "database")
	env.markSessionActive(&dependency)
	env.addDesired("database", "database", true)
	if err := env.store.SetMetadataBatch(dependency.ID, map[string]string{"instance_token": "database-token"}); err != nil {
		t.Fatalf("stamp dependency instance token: %v", err)
	}

	target := env.createSessionBead("dependent", "dependent")
	stampCanonicalSingleton(t, env, target.ID)
	requestExplicitWake(t, env, target.ID)

	controller, err := newSessionStartController(sessionStartControllerOptions{
		Workers:     1,
		MaxDistinct: 8,
		MaxRetries:  0,
		Stderr:      io.Discard,
		Reconcile:   reconcile,
	})
	if err != nil {
		t.Fatalf("create session-start controller: %v", err)
	}
	if err := controller.Start(t.Context()); err != nil {
		t.Fatalf("start session-start controller: %v", err)
	}
	t.Cleanup(controller.Stop)

	return &wakeStandDownFixture{
		env:        env,
		controller: controller,
		targetID:   target.ID,
		dependency: env.sessionInfo(dependency.ID),
		cr: &CityRuntime{
			cityPath:               "test-city",
			cityName:               "test-city",
			cfg:                    env.cfg,
			sp:                     env.sp,
			cs:                     coherentSessionStartControllerStateForTest(env.cfg, env.sp, env.store, rollout.Auto),
			rec:                    events.Discard,
			stdout:                 io.Discard,
			stderr:                 io.Discard,
			sessionStartOwnership:  sessionStartOwnershipKeyed,
			sessionStartMode:       rollout.Auto,
			sessionStartController: controller,
		},
	}
}

// certifiedWakeLease is the lease the keyed configured-dependency family holds
// while it starts the target.
func (f *wakeStandDownFixture) certifiedWakeLease() configuredDependencyStartLease {
	return configuredDependencyStartLease{
		SessionID:               f.targetID,
		TargetTemplate:          "dependent",
		DependencyTemplate:      "database",
		DependencySessionID:     f.dependency.ID,
		DependencySessionName:   f.dependency.SessionName,
		DependencyInstanceToken: f.dependency.InstanceToken,
		ControllerGeneration:    1,
	}
}

func (f *wakeStandDownFixture) admitCertifiedWake(t *testing.T) {
	t.Helper()
	outcome, err := f.controller.AdmitConfiguredDependency(f.certifiedWakeLease(), sessionStartAdmissionInProcess)
	if err != nil || outcome != sessionStartAdmissionAccepted {
		t.Fatalf("admit certified wake lease = (%q, %v), want accepted", outcome, err)
	}
}

// legacyExclusion is the seam production hands the legacy start wave.
func (f *wakeStandDownFixture) legacyExclusion(t *testing.T) func(sessionpkg.Info) bool {
	t.Helper()
	excluded := f.cr.sessionStartLegacyExclusionPredicate()
	if excluded == nil {
		t.Fatal("keyed city produced no legacy start exclusion seam")
	}
	return excluded
}

func (f *wakeStandDownFixture) prepareLegacyStart(snapshot sessionpkg.Info, excluded func(sessionpkg.Info) bool) (*preparedStart, error) {
	return prepareStartCandidateForCity(
		startCandidate{
			info: snapshot,
			tp:   TemplateParams{Command: "test-cmd", SessionName: "dependent", TemplateName: "dependent"},
		},
		"", "", f.env.cfg, f.env.sp, f.env.store, f.env.clk, io.Discard, nil,
		excluded,
	)
}

// TestLegacyStandsDownForCertifiedWakeLease is RED 1 of the sixth-round ruling:
// the run-13 shape itself. A legacy start candidate decided on a snapshot taken
// before the certified wake lease existed passes its loop-top exclusion check,
// and by the time it reaches prepare the keyed family owns the row. Clause (i)
// of the pre-wake validator only asked classifyExactSessionStartOwnership, which
// answers LEGACY for exactly this canonical-singleton shape, so both writers
// entered: run 13 logged "op=start wave=0 session=dependent outcome=success"
// beside the keyed start, after which the row was orphan-closed and its runtime
// reaped. The validator must consult the keyed-ownership seam it already
// receives, on CURRENT info, inside the per-session mutation lock.
func TestLegacyStandsDownForCertifiedWakeLease(t *testing.T) {
	f := newWakeStandDownFixture(t)
	snapshot := f.env.sessionInfo(f.targetID)

	// Premise: the classification arm alone cannot see this. If it could, the
	// pre-existing clause (i) would already have closed run 13.
	if _, _, owner := classifyExactSessionStartOwnership(snapshot, f.env.cfg, f.env.clk.Now().UTC()); owner == exactSessionStartKeyedOwner {
		t.Fatalf("test premise: the canonical singleton classifies %v; run 13 needs the legacy classification", owner)
	}

	f.admitCertifiedWake(t)
	excluded := f.legacyExclusion(t)
	if !excluded(f.env.sessionInfo(f.targetID)) {
		t.Fatal("the installed seam does not exclude a row a certified wake lease owns")
	}

	prepared, err := f.prepareLegacyStart(snapshot, excluded)
	if !errors.Is(err, errPreWakeSuperseded) || prepared != nil {
		t.Fatalf("legacy prepare inside the certified wake window = %v (prepared=%t), want errPreWakeSuperseded", err, prepared != nil)
	}
	var skip *legacyStartPreWakeSkip
	if !errors.As(err, &skip) || skip.reason != "keyed_start_owner" {
		t.Fatalf("supersede reason = %+v, want keyed_start_owner", skip)
	}

	after := f.env.sessionInfo(f.targetID)
	if after.InstanceToken != snapshot.InstanceToken || after.MetadataState != snapshot.MetadataState {
		t.Fatalf("legacy rotated a keyed-owned incarnation: token %q->%q state %q->%q",
			snapshot.InstanceToken, after.InstanceToken, snapshot.MetadataState, after.MetadataState)
	}
	if after.WakeRequest != snapshot.WakeRequest {
		t.Fatalf("legacy consumed the certified wake cause: %q -> %q", snapshot.WakeRequest, after.WakeRequest)
	}
	if got := f.env.sp.CountCalls("Start", "dependent"); got != 0 {
		t.Fatalf("provider starts from the superseded legacy candidate = %d, want 0", got)
	}
}

// TestLegacyStartsWhenKeyedWakeCertificationRefuses is RED 2 — the no-lapse
// standard, forward direction. Candidacy-based stand-down was rejected for
// exactly this case: when keyed certification refuses, no lease is admitted, so
// the seam stays false and the legacy pass must start the row on the durable,
// unconsumed wake_request. The condition is level-triggered; nothing strands.
func TestLegacyStartsWhenKeyedWakeCertificationRefuses(t *testing.T) {
	f := newWakeStandDownFixture(t)
	if f.controller.ownsConfiguredDependencyStart(f.targetID) {
		t.Fatal("test premise: a lease exists where certification refused")
	}
	before := f.env.sessionInfo(f.targetID)

	prepared, err := f.prepareLegacyStart(before, f.legacyExclusion(t))
	if err != nil || prepared == nil {
		t.Fatalf("legacy prepare after a refused keyed certification = %v (prepared=%t), want the start to proceed", err, prepared != nil)
	}
	after := f.env.sessionInfo(f.targetID)
	if after.InstanceToken == before.InstanceToken || after.MetadataState != string(sessionpkg.StateCreating) {
		t.Fatalf("legacy did not take the un-owned wake: token %q->%q state %q",
			before.InstanceToken, after.InstanceToken, after.MetadataState)
	}
}

// TestKeyedWakeEntryRefusesAfterLegacyWinsTheRotation is RED 3 — the no-lapse
// standard, backward direction. When the lease lands AFTER legacy's in-lock
// check, legacy wins the rotation and consumes wake_request; the keyed entry
// then re-validates its certificate on CURRENT info and refuses cleanly, so
// exactly one starter enters either way. In Auto the refusal hands the row back
// to legacy instead of parking it.
func TestKeyedWakeEntryRefusesAfterLegacyWinsTheRotation(t *testing.T) {
	f := newWakeStandDownFixture(t)
	before := f.env.sessionInfo(f.targetID)

	prepared, err := f.prepareLegacyStart(before, f.legacyExclusion(t))
	if err != nil || prepared == nil {
		t.Fatalf("legacy prepare before the lease = %v (prepared=%t), want the rotation to proceed", err, prepared != nil)
	}
	rotated := f.env.sessionInfo(f.targetID)
	if rotated.WakeRequest != "" {
		t.Fatalf("legacy rotation left the wake cause unconsumed: %+v", rotated)
	}

	lease := f.certifiedWakeLease()
	params := exactSessionStartTestParams(t, f.env)
	params.Generation = 1
	params.RolloutMode = rollout.Auto
	params.ValidateConfiguredDependencyStart = func(sessionpkg.Info, configuredDependencyStartLease) bool { return true }

	owner, reconcileErr := reconcileExactSessionStartWithOwner(t.Context(), sessionStartAdmission{
		SessionID:            f.targetID,
		Source:               sessionStartAdmissionInProcess,
		ConfiguredDependency: &lease,
	}, params)
	if reconcileErr != nil {
		t.Fatalf("late keyed entry = %v, want a clean refusal", reconcileErr)
	}
	if owner != exactSessionStartLegacyOwner {
		t.Fatalf("late keyed entry owner = %v, want the legacy owner it lost to", owner)
	}
	final := f.env.sessionInfo(f.targetID)
	if final.InstanceToken != rotated.InstanceToken {
		t.Fatalf("the late keyed entry minted a second incarnation: %q -> %q", rotated.InstanceToken, final.InstanceToken)
	}
	if got := f.env.sp.CountCalls("Start", "dependent"); got > 1 {
		t.Fatalf("provider starts across the interleave = %d, want at most 1", got)
	}
}

// TestTerminallyRefusedWakeLeaseReleasesInTheSameReconcile is RED 4 — the
// retained-witness invariant, and the one real lapse hazard the stand-down
// creates. Wake-family admissions that ERROR retain their leases ("a retained
// handoff witness is never exhausted"), which keeps the seam true; now that
// legacy stands down on the seam, a permanently-refusing keyed reconcile would
// fence legacy out of the row forever. The invariant: a terminal refusal must
// release the lease and its admission in the SAME reconcile, so the seam clears
// and legacy starts the row within a bounded number of passes.
func TestTerminallyRefusedWakeLeaseReleasesInTheSameReconcile(t *testing.T) {
	reconciled := make(chan struct{}, 4)
	f := newWakeStandDownFixtureWithReconcile(t, func(context.Context, sessionStartAdmission) error {
		reconciled <- struct{}{}
		// The refusal every terminal entry-validation failure produces in Auto:
		// the handler yields the row to legacy rather than parking it.
		return errSessionStartLegacyFallbackRequired
	})
	f.admitCertifiedWake(t)

	select {
	case <-reconciled:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("terminally refused wake lease never reached the reconciler")
	}
	awaitCond(t, func() bool { return !f.controller.ownsConfiguredDependencyStart(f.targetID) },
		"a terminally refused wake lease to release its ownership witness (retained forever, legacy is fenced out of the row permanently)")

	before := f.env.sessionInfo(f.targetID)
	prepared, err := f.prepareLegacyStart(before, f.legacyExclusion(t))
	if err != nil || prepared == nil {
		t.Fatalf("legacy prepare after the released wake lease = %v (prepared=%t), want the start to proceed", err, prepared != nil)
	}
	if after := f.env.sessionInfo(f.targetID); after.InstanceToken == before.InstanceToken {
		t.Fatalf("legacy did not re-own the released row: token stayed %q", before.InstanceToken)
	}
}
