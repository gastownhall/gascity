package main

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// isolateKeyedRoutedWorkAllocations gives one test exclusive use of the
// process-wide seam.
func isolateKeyedRoutedWorkAllocations(t *testing.T) {
	t.Helper()
	keyedRoutedWorkAllocations.reset()
	t.Cleanup(keyedRoutedWorkAllocations.reset)
}

func legacyPoolBuilderFixture(t *testing.T) (*agentBuildParams, *config.City, beads.Store) {
	t.Helper()
	store := beads.NewMemStore()
	cfg := poolMemberDemandCity()
	var stderr bytes.Buffer
	bp := newAgentBuildParams("test-city", t.TempDir(), cfg, runtime.NewFake(), time.Now().UTC(), store, &stderr)
	bp.sessionBeads = newSessionBeadSnapshot(nil)
	return bp, cfg, store
}

// TestLegacyPoolBuilderStandsDownForKeyedAllocationReservation is the
// allocation-ownership seam's RED (ga-f7v2ft.126's cutover arm). The keyed
// allocation holds the exact key but has NOT created its member yet, so no
// durable claim exists — which is precisely the window first-creator-wins gave
// to legacy, because legacy plans from a per-tick snapshot and creates
// immediately. Without the stand-down the legacy builder materializes the
// member and wins the work item.
func TestLegacyPoolBuilderStandsDownForKeyedAllocationReservation(t *testing.T) {
	isolateKeyedRoutedWorkAllocations(t)
	bp, cfg, store := legacyPoolBuilderFixture(t)

	const workID = "kw-101"
	const storeRef = "city:test-city"
	// Premise: nothing durable claims the work — the seam, not the claim, is
	// what stands legacy down here.
	if members := livePoolMembersForWork(t, store, cfg, workID, storeRef); len(members) != 0 {
		t.Fatalf("premise broken: live members before the keyed create = %+v, want none", members)
	}
	keyedRoutedWorkAllocations.reserve(routedWorkAllocationKeyFor(workID, "worker", storeRef), time.Now())

	info, err := executePlannedPoolSessionBeadCreate(bp, &cfg.Agents[0], "worker", poolMemberDemandPlan(workID, storeRef))
	if !errors.Is(err, errKeyedPoolAllocationOwnsWork) || info.ID != "" {
		t.Fatalf("legacy create inside the keyed allocation window = %+v / %v, want errKeyedPoolAllocationOwnsWork", info, err)
	}
	if members := livePoolMembersForWork(t, store, cfg, workID, storeRef); len(members) != 0 {
		t.Fatalf("legacy materialized %+v inside the keyed allocation window; the stand-down must create no member and contribute no demand", members)
	}
}

// TestLegacyPoolBuilderProceedsAfterKeyedAllocationReleases is the no-lapse RED
// the round-6 template requires per arm: the stand-down is lease-triggered, not
// candidacy-triggered. A released reservation — which the allocation handler's
// deferred release performs on every path, including refusal and error — leaves
// legacy free to materialize on its very next pass.
func TestLegacyPoolBuilderProceedsAfterKeyedAllocationReleases(t *testing.T) {
	isolateKeyedRoutedWorkAllocations(t)
	bp, cfg, store := legacyPoolBuilderFixture(t)

	const workID = "kw-102"
	const storeRef = "city:test-city"
	key := routedWorkAllocationKeyFor(workID, "worker", storeRef)
	keyedRoutedWorkAllocations.reserve(key, time.Now())
	keyedRoutedWorkAllocations.release(key)

	info, err := executePlannedPoolSessionBeadCreate(bp, &cfg.Agents[0], "worker", poolMemberDemandPlan(workID, storeRef))
	if err != nil || info.ID == "" {
		t.Fatalf("legacy create after the keyed allocation released = %+v / %v, want a materialized member", info, err)
	}
	if members := livePoolMembersForWork(t, store, cfg, workID, storeRef); len(members) != 1 {
		t.Fatalf("live members after the release = %+v, want exactly the legacy member", members)
	}
}

// TestKeyedAllocationReservationLapsesRatherThanFencingForever pins the one real
// lapse hazard the ga-ij8mh round-6 ruling names, applied to this boundary: a
// keyed controller that stops between enqueue and handling would otherwise fence
// legacy off the work item forever. The reservation answers on CURRENT state and
// retires itself past the lapse bound.
func TestKeyedAllocationReservationLapsesRatherThanFencingForever(t *testing.T) {
	isolateKeyedRoutedWorkAllocations(t)
	bp, cfg, _ := legacyPoolBuilderFixture(t)

	const workID = "kw-103"
	const storeRef = "city:test-city"
	key := routedWorkAllocationKeyFor(workID, "worker", storeRef)
	keyedRoutedWorkAllocations.reserve(key, time.Now().Add(-2*routedWorkAllocationReservationLapse))

	if keyedRoutedWorkAllocations.owns(key, time.Now()) {
		t.Fatal("a reservation past its lapse bound still fences legacy")
	}
	info, err := executePlannedPoolSessionBeadCreate(bp, &cfg.Agents[0], "worker", poolMemberDemandPlan(workID, storeRef))
	if err != nil || info.ID == "" {
		t.Fatalf("legacy create after the reservation lapsed = %+v / %v, want a materialized member", info, err)
	}
}

// TestKeyedAllocationReservationIsExactKeyed is the negative: the seam fences the
// work item it owns and nothing else. A different work item, a different pool
// target, or a different source store is untouched, so one allocation never
// stalls an unrelated pool.
func TestKeyedAllocationReservationIsExactKeyed(t *testing.T) {
	isolateKeyedRoutedWorkAllocations(t)
	now := time.Now()
	keyedRoutedWorkAllocations.reserve(routedWorkAllocationKeyFor("kw-104", "worker", "city:test-city"), now)

	for _, other := range []routedWorkAllocationKey{
		routedWorkAllocationKeyFor("kw-999", "worker", "city:test-city"),
		routedWorkAllocationKeyFor("kw-104", "reviewer", "city:test-city"),
		routedWorkAllocationKeyFor("kw-104", "worker", "rig:frontend"),
	} {
		if keyedRoutedWorkAllocations.owns(other, now) {
			t.Fatalf("keyed allocation fenced an unrelated key %+v", other)
		}
	}

	bp, cfg, store := legacyPoolBuilderFixture(t)
	info, err := executePlannedPoolSessionBeadCreate(bp, &cfg.Agents[0], "worker", poolMemberDemandPlan("kw-999", "city:test-city"))
	if err != nil || info.ID == "" {
		t.Fatalf("legacy create for an unreserved work item = %+v / %v, want a materialized member", info, err)
	}
	if members := livePoolMembersForWork(t, store, cfg, "kw-999", "city:test-city"); len(members) != 1 {
		t.Fatalf("live members for the unreserved work item = %+v, want one", members)
	}
}

// TestKeyedAllocationReservationCoalescesReplays pins the refcount: a replayed
// hint for a key already in the lane must not let the replay's release retire
// the original owner's fence early.
func TestKeyedAllocationReservationCoalescesReplays(t *testing.T) {
	isolateKeyedRoutedWorkAllocations(t)
	now := time.Now()
	key := routedWorkAllocationKeyFor("kw-105", "worker", "city:test-city")

	keyedRoutedWorkAllocations.reserve(key, now)
	keyedRoutedWorkAllocations.reserve(key, now)
	keyedRoutedWorkAllocations.release(key)
	if !keyedRoutedWorkAllocations.owns(key, now) {
		t.Fatal("a replayed hint's release retired the original owner's fence")
	}
	keyedRoutedWorkAllocations.release(key)
	if keyedRoutedWorkAllocations.owns(key, now) {
		t.Fatal("the last release did not retire the fence")
	}
}

// TestKeyedPoolAllocationLaneReservesAndReleasesAroundTheHandler proves the seam
// is driven by the real lane rather than by the tests: enqueueing an exact key
// opens the reservation, and handleRoutedWorkPoolAllocation closes it on the way
// out — including when the allocation is refused.
func TestKeyedPoolAllocationLaneReservesAndReleasesAroundTheHandler(t *testing.T) {
	isolateKeyedRoutedWorkAllocations(t)
	fixture := newRoutedWorkPoolAllocationFixture(t, beads.NewMemStore())
	work, err := fixture.store.Create(beads.Bead{
		Title:    "ready routed work",
		Type:     "task",
		Status:   "open",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create ready work: %v", err)
	}
	key := routedWorkAllocationKeyFor(work.ID, "worker", "city:test-city")
	fixture.cr.routedWorkPoolAllocationCh = make(chan routedWorkPoolAllocationHint, 4)

	if !fixture.cr.enqueueRoutedWorkPoolAllocation(readyRoutedWorkDemandContribution{
		WorkID: work.ID, PoolTarget: "worker", SourceStore: "city:test-city",
	}) {
		t.Fatal("enqueueing an exact routed-work key was refused")
	}
	if !keyedRoutedWorkAllocations.owns(key, time.Now()) {
		t.Fatal("enqueueing the exact key did not open the allocation-ownership seam")
	}

	hint := <-fixture.cr.routedWorkPoolAllocationCh
	fixture.cr.handleRoutedWorkPoolAllocation(t.Context(), hint)

	if keyedRoutedWorkAllocations.owns(key, time.Now()) {
		t.Fatal("the allocation handler returned without closing the ownership seam")
	}
}
