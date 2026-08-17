package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestExecutionStalledFenceAdoptsCrashLeftoverWithoutLatch: a stalled fence
// whose durable latch never landed (controller died between the fence CAS and
// the latch write) must be adoptable by the next exhausted() attempt, not a
// permanent block.
func TestExecutionStalledFenceAdoptsCrashLeftoverWithoutLatch(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	info := sessionpkg.InfoFromPersistedBead(f.session)
	target, resolution := resolveExecutionBackstopForTest(t, f)
	if resolution != backstopResolutionOutstanding {
		t.Fatalf("resolve resolution = %v", resolution)
	}
	// Simulate the crash window: install the fence exactly as exhausted()
	// would, but never write the latch.
	if !installExecutionStalledFenceForTest(t, f, info, target) {
		t.Fatal("precondition: initial fence install failed")
	}
	// The next install attempt must adopt the identical fence (same
	// coordinates) rather than reporting blocked.
	if !installExecutionStalledFenceForTest(t, f, info, target) {
		t.Fatal("crash-leftover fence was not adopted")
	}
}

// TestExecutionStalledFenceAdoptsAfterReleasedTombstoneAcknowledge: an active
// hook lease blocks the fence, but once the hook released its tombstone and
// the seat is quiet past the grace (already proven inside exhausted's
// boundary), the fence install acknowledges the tombstone and proceeds.
func TestExecutionStalledFenceAdoptsAfterReleasedTombstoneAcknowledge(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	info := sessionpkg.InfoFromPersistedBead(f.session)
	target, resolution := resolveExecutionBackstopForTest(t, f)
	if resolution != backstopResolutionOutstanding {
		t.Fatalf("resolve resolution = %v", resolution)
	}
	lease, _, err := sessionpkg.AcquireHookActivityLease(f.store, sessionpkg.HookActivityCoordinates{
		SessionID:     info.ID,
		Generation:    info.Generation,
		InstanceToken: info.InstanceToken,
	})
	if err != nil {
		t.Fatalf("hook lease: %v", err)
	}
	if installExecutionStalledFenceForTest(t, f, info, target) {
		t.Fatal("fence installed over an ACTIVE hook lease")
	}
	if err := lease.Release(); err != nil {
		t.Fatalf("hook release: %v", err)
	}
	if !installExecutionStalledFenceForTest(t, f, info, target) {
		t.Fatal("fence not installed after released tombstone + quiet boundary")
	}
	raw, err := f.store.Get(f.session.ID)
	if err != nil {
		t.Fatalf("reading session: %v", err)
	}
	gate := raw.Metadata[sessionpkg.SessionHookActivityGateMetadataKey]
	if !sessionpkg.IsExecutionStalledActivityFence(gate) {
		t.Fatalf("gate = %q, want the stalled fence (tombstone must be consumed, not left behind)", gate)
	}
}

// TestExecutionStalledFenceRespectsForeignStalledAuthority: a stalled fence
// installed for DIFFERENT work authority must block the install without being
// cleared or adopted.
func TestExecutionStalledFenceRespectsForeignStalledAuthority(t *testing.T) {
	f := newExecutionBackstopFixture(t)
	f.idleFor(t, 10*time.Minute)
	info := sessionpkg.InfoFromPersistedBead(f.session)
	target, _ := resolveExecutionBackstopForTest(t, f)
	foreign := stalledFenceCoordinatesFor(info, target)
	foreign.WorkID = "work-foreign"
	foreign.WorkRevision = target.WorkRevision + 100
	if _, _, err := sessionpkg.AcquireExecutionStalledActivityFence(f.store, foreign); err != nil {
		t.Fatalf("installing foreign fence: %v", err)
	}
	if installExecutionStalledFenceForTest(t, f, info, target) {
		t.Fatal("fence installed over a foreign stalled authority")
	}
	raw, _ := f.store.Get(f.session.ID)
	gate := raw.Metadata[sessionpkg.SessionHookActivityGateMetadataKey]
	if !sessionpkg.ExecutionStalledActivityFenceMatches(gate, foreign) {
		t.Fatalf("foreign fence was disturbed: %q", gate)
	}
}

// resolveExecutionBackstopForTest drives the fixture through the same
// resolve path nudgeStalledPoolExecution builds in production.
func resolveExecutionBackstopForTest(t *testing.T, f *executionBackstopFixture) (backstopTarget, backstopResolution) {
	t.Helper()
	work, err := f.store.List(beads.ListQuery{Status: "in_progress"})
	if err != nil {
		t.Fatalf("listing work: %v", err)
	}
	stores := make([]beads.Store, len(work))
	refs := make([]string, len(work))
	for i := range work {
		stores[i] = f.store
		refs[i] = "city"
	}
	pred := poolExecutionBackstop{
		cfg:    f.cfg,
		sp:     f.sp,
		now:    f.now,
		rec:    f.rec,
		claims: newExecutionClaimSnapshot(work, stores, refs),
	}
	target, resolution := pred.resolve(f.session, nil, f.sessName)
	if !executionTargetAuthorityProven(target) {
		t.Fatalf("target authority not proven: %+v", target)
	}
	return target, resolution
}

func installExecutionStalledFenceForTest(t *testing.T, f *executionBackstopFixture, info sessionpkg.Info, target backstopTarget) bool {
	t.Helper()
	pred := poolExecutionBackstop{cfg: f.cfg, sp: f.sp, now: f.now, rec: f.rec}
	return pred.installExecutionStalledActivityFence(f.store, info, target)
}

func stalledFenceCoordinatesFor(info sessionpkg.Info, target backstopTarget) sessionpkg.ExecutionStalledActivityFenceCoordinates {
	return sessionpkg.ExecutionStalledActivityFenceCoordinates{
		HookActivityCoordinates: sessionpkg.HookActivityCoordinates{
			SessionID:     info.ID,
			Generation:    info.Generation,
			InstanceToken: info.InstanceToken,
		},
		AwakeStartedAt:     target.AwakeStartedAt,
		LifecycleAuthority: target.LifecycleAuthority,
		WorkID:             target.ID,
		WorkStoreRef:       target.StoreRef,
		WorkRevision:       target.WorkRevision,
		WorkClaimFence:     target.WorkClaimFence,
		Assignee:           target.Assignee,
	}
}
