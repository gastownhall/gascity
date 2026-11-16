package main

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// claimedSeatRow is pool seat gc-1, minted for w-1, awake and fresh: the
// protected stand-in for its trigger until it claims.
func claimedSeatRow() beads.Bead {
	return poolRow("gc-1", "worker", 1, "awake", "state_reason", "creation_complete",
		"creation_complete_at", ago(14*time.Second), "gc.trigger_bead_id", "w-1")
}

func claimedDemandCity() *config.City {
	return &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{poolAgent("worker", "", intPtr(8), 0)}}
}

// claimedGather is one v2 pass's demand over a lane-fed leg whose cache holds
// the work as it is now, while the external-reads lane's recording holds the
// open rows as it last read them, a few seconds earlier.
func claimedGather(t *testing.T, reads func(*beads.CachingStore) demandReads, now []beads.Bead) demandView {
	t.Helper()
	cache, _ := newDemandCache(t, false, now...)
	v, err := gatherDemand(demandGatherEnv{
		Cfg: claimedDemandCity(), CityStore: cache,
		OpenSessions: sessionInfosFromBeads([]beads.Bead{claimedSeatRow()}),
	}, reads(cache))
	if err != nil {
		t.Fatalf("gatherDemand: %v", err)
	}
	return v
}

// recordedOpen is a v2 reads whose recording of cache's leg lists open.
func recordedOpen(open ...beads.Bead) func(*beads.CachingStore) demandReads {
	return func(cache *beads.CachingStore) demandReads {
		rec := &externalReadsRecording{Seq: 1, FreshFor: time.Minute, Sources: map[sourceKey]sourceResult{
			keyOf(sourceDemand, cache): {EndedAt: allocNow.Add(-5 * time.Second), sourcePayload: sourcePayload{Leg: legRecording{RawOpen: open, ReadyAll: open}}},
		}}
		return newV2DemandReads(allocNow, rec)
	}
}

// claimedByGC1 is routed work id once seat gc-1 has claimed it.
func claimedByGC1(id string) beads.Bead {
	b := routedDemandBead(id)
	b.Status, b.Assignee = "in_progress", "gc-1"
	return b
}

// Kills: a seat's own claim minting a second seat for the same work, through
// the readers that produced it in production (gc-ent4, 2026-11-02): gc-1 was
// minted for w-1 and claimed it; the leg's cache shows the claim, while the
// lane's recording, a few seconds older, still lists w-1 open and unassigned.
// The claim puts gc-1 on the resume tier, where it no longer stands in for
// w-1 as protected capacity, so counting the recorded row minted a duplicate
// that drained with no work. Gather now cuts the row; the decide plans no
// seat for it, traces why, and still plans one for new work routed meanwhile.
func TestAllocator_ClaimAheadOfTheDemandRecordingMintsNoSecondSeat(t *testing.T) {
	v := claimedGather(t, recordedOpen(routedDemandBead("w-1")), []beads.Bead{claimedByGC1("w-1")})
	if cut := v.ClaimedDemand["worker"]; cut.Read != 1 || !slices.Equal(cut.WorkBeadIDs, []string{"w-1"}) {
		t.Fatalf("claimed demand = %+v, want the recorded w-1 cut", v.ClaimedDemand)
	}
	if got := v.Collected.DefaultCounts["worker"]; got != 0 {
		t.Fatalf("worker default demand = %d, want 0 once the claimed row is cut", got)
	}

	f := newAllocFixture(t, claimedDemandCity()).sessions(claimedSeatRow()).alive("s-gc-1", InventoryAttrs{})
	f.in.Demand = v
	d := f.decide()
	if len(d.Plans) != 0 {
		t.Fatalf("plans %v, want none: gc-1 holds w-1 (trace %v)", planWork(d), d.Trace)
	}
	if got := d.Snapshot.PoolDesired["worker"]; got != 1 {
		t.Fatalf("PoolDesired = %d, want 1 (gc-1 resumes its claim)", got)
	}
	var traced []string
	for _, r := range d.Trace {
		if strings.HasPrefix(r.Reason, allocTraceClaimedDemand) {
			traced = append(traced, r.Template+"/"+r.Reason)
		}
	}
	if !slices.Equal(traced, []string{"worker/claimed-demand:w-1"}) {
		t.Fatalf("claimed-demand trace = %v, want [worker/claimed-demand:w-1]", traced)
	}

	// New work routed meanwhile still gets its seat.
	f.in.Demand = claimedGather(t, recordedOpen(routedDemandBead("w-1"), routedDemandBead("w-2")),
		[]beads.Bead{claimedByGC1("w-1"), routedDemandBead("w-2")})
	if got := planWork(f.decide()); fmt.Sprint(got) != "[w-2]" {
		t.Fatalf("new work behind the claim: plans %v, want [w-2]", got)
	}
}

// Legacy reads take no cut: its probe is a live read taken after its claim
// read, so where the two disagree the live probe is the newer one. Here the
// leg's cache still shows gc-1's claim on w-1 after the backing released it;
// the live Ready lists w-1 again, and that row stays demand.
func TestGatherDemandCutsClaimedWorkOnlyUnderV2Reads(t *testing.T) {
	cache, backing := newDemandCache(t, false, claimedByGC1("w-1"))
	open, unassigned := "open", ""
	if err := backing.Update("w-1", beads.UpdateOpts{Status: &open, Assignee: &unassigned}); err != nil {
		t.Fatalf("release w-1 behind the cache: %v", err)
	}
	v, err := gatherDemand(demandGatherEnv{
		Cfg: claimedDemandCity(), CityStore: cache,
		OpenSessions: sessionInfosFromBeads([]beads.Bead{claimedSeatRow()}),
	}, nil)
	if err != nil {
		t.Fatalf("gatherDemand: %v", err)
	}
	if got := ids(v.AssignedWork); !slices.Equal(got, []string{"w-1"}) {
		t.Fatalf("assigned work = %v, want the cache's stale claim on w-1", got)
	}
	if v.ClaimedDemand != nil || v.Collected.DefaultCounts["worker"] != 1 {
		t.Fatalf("legacy reads: claimed demand %v, worker demand %d; want no cut and the live read's w-1", v.ClaimedDemand, v.Collected.DefaultCounts["worker"])
	}
}

// The planner's pass record counts each cut row once, on the first pass that
// cuts it: a row the recording keeps listing is one lagging claim, not one
// per pass, and a row cut again after a pass without it is a new one.
func TestPlannerCountsClaimedDemandCutOncePerClaim(t *testing.T) {
	p := &planner{metrics: newPassMetrics()}
	cut := func(rows map[string][]string) demandView {
		v := demandView{ClaimedDemand: make(map[string]claimedDemandCut)}
		for template, ids := range rows {
			v.ClaimedDemand[template] = claimedDemandCut{WorkBeadIDs: ids}
		}
		return v
	}
	for i, pass := range []struct {
		rows map[string][]string
		want uint64
	}{
		{map[string][]string{"worker": {"w-1", "w-2"}, "builder": {"w-3"}}, 3},
		{map[string][]string{"worker": {"w-1", "w-2"}, "builder": {"w-3"}}, 3},
		{map[string][]string{"worker": {"w-2", "w-4"}}, 4},
		{nil, 4},
		{map[string][]string{"worker": {"w-2"}}, 5},
	} {
		p.observeClaimedDemand(cut(pass.rows))
		if got := p.metrics.series[claimedDemandSeries]; got != pass.want {
			t.Fatalf("pass %d: %s = %d, want %d", i+1, claimedDemandSeries, got, pass.want)
		}
	}
}

// claimedSplitCity is a split city whose work ledger and class binding are
// both lane-fed legs: each cache holds its rows as they are now.
type claimedSplitCity struct {
	path            string
	ledger, binding *beads.CachingStore
}

func newClaimedSplitCity(t *testing.T, ledger, binding []beads.Bead) claimedSplitCity {
	t.Helper()
	open := func(rows []beads.Bead) *beads.CachingStore {
		cache := beads.NewCachingStoreForTest(beads.NewMemStoreFrom(0, rows, nil), nil)
		if err := cache.Prime(context.Background()); err != nil {
			t.Fatalf("prime: %v", err)
		}
		return cache
	}
	c := claimedSplitCity{path: t.TempDir(), ledger: open(ledger), binding: open(binding)}
	routes := splitRoutes(c.binding)
	registerResidencyRoutes(c.path, routes, func() beads.Store { return c.ledger })
	t.Cleanup(func() { unregisterResidencyRoutes(c.path, routes) })
	return c
}

// gather is one v2 pass at now whose recordings, a few seconds old, list
// ledgerOpen and bindingOpen as each leg's open rows.
func (c claimedSplitCity) gather(t *testing.T, now time.Time, ledgerOpen, bindingOpen []beads.Bead) demandView {
	t.Helper()
	leg := func(open []beads.Bead) sourceResult {
		return sourceResult{EndedAt: now.Add(-5 * time.Second), sourcePayload: sourcePayload{Leg: legRecording{RawOpen: open, ReadyAll: open, ReadyGated: true}}}
	}
	rec := &externalReadsRecording{Seq: 1, FreshFor: time.Minute, Sources: map[sourceKey]sourceResult{
		keyOf(sourceDemand, c.ledger):  leg(ledgerOpen),
		keyOf(sourceDemand, c.binding): leg(bindingOpen),
	}}
	v, err := gatherDemand(demandGatherEnv{
		CityName: "test-city", CityPath: c.path, Cfg: claimedDemandCity(), CityStore: c.binding,
		OpenSessions: sessionInfosFromBeads([]beads.Bead{claimedSeatRow()}),
	}, newV2DemandReads(now, rec))
	if err != nil {
		t.Fatalf("gatherDemand: %v", err)
	}
	return v
}

// On a split city a claim holds the row the probe read only where the claim
// resides. `gc storage migrate` can leave the work ledger a frozen copy of a
// bead it carried into the binding, under the same ID; an assignee on that
// copy says nothing about the binding row.
//
// Kills: the work ledger's frozen claim cutting its live binding twin for good
// (no recording or cache refresh changes the copy), and a binding or ledger
// row's own claim no longer cutting it.
func TestGatherDemandCutsAClaimOnlyWhereItResides(t *testing.T) {
	for name, tc := range map[string]struct {
		ledger, binding           []beads.Bead // the caches: the work as it is now
		ledgerOpen, bindingOpen   []beads.Bead // the recordings: as the lane last read it
		wantDemand, wantClaimedIn []string
	}{
		"binding-row-claimed-in-the-binding": {
			binding:     []beads.Bead{claimedByGC1("gcg-1")},
			bindingOpen: []beads.Bead{routedDemandBead("gcg-1")}, wantClaimedIn: []string{"gcg-1"},
		},
		"ledger-row-claimed-in-the-ledger": {
			ledger:     []beads.Bead{claimedByGC1("w-1")},
			ledgerOpen: []beads.Bead{routedDemandBead("w-1")}, wantClaimedIn: []string{"w-1"},
		},
		"binding-twin-of-a-frozen-ledger-claim": {
			ledger:      []beads.Bead{claimedByGC1("gcg-1")},
			binding:     []beads.Bead{routedDemandBead("gcg-1")},
			bindingOpen: []beads.Bead{routedDemandBead("gcg-1")}, wantDemand: []string{"gcg-1"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			c := newClaimedSplitCity(t, tc.ledger, tc.binding)
			v := c.gather(t, allocNow, tc.ledgerOpen, tc.bindingOpen)
			if got := v.Collected.DefaultDemand["worker"].WorkBeadIDs; !slices.Equal(got, tc.wantDemand) {
				t.Fatalf("worker demand = %v, want %v (claimed demand %+v)", got, tc.wantDemand, v.ClaimedDemand)
			}
			if got := v.ClaimedDemand["worker"].WorkBeadIDs; !slices.Equal(got, tc.wantClaimedIn) {
				t.Fatalf("claimed demand = %v, want %v", got, tc.wantClaimedIn)
			}
		})
	}
}

// Nothing refreshes the frozen copy, so a twin it cut lost its seat for good.
// The twin keeps a seat pass after pass, while the copy's holder resumes.
func TestAllocator_FrozenLedgerClaimLeavesItsBindingTwinASeat(t *testing.T) {
	c := newClaimedSplitCity(t, []beads.Bead{claimedByGC1("gcg-1")}, []beads.Bead{routedDemandBead("gcg-1")})
	f := newAllocFixture(t, claimedDemandCity()).sessions(claimedSeatRow()).alive("s-gc-1", InventoryAttrs{})
	for pass, now := range []time.Time{allocNow, allocNow.Add(2 * time.Minute)} {
		f.in.Demand = c.gather(t, now, nil, []beads.Bead{routedDemandBead("gcg-1")})
		if got := ids(f.in.Demand.AssignedWork); !slices.Equal(got, []string{"gcg-1"}) {
			t.Fatalf("pass %d: assigned work = %v, want the frozen ledger claim", pass+1, got)
		}
		d := f.decide()
		if got := planWork(d); !slices.Equal(got, []string{"gcg-1"}) {
			t.Fatalf("pass %d: plans %v, want one seat for the binding's released gcg-1 (trace %v)", pass+1, got, d.Trace)
		}
		if got := d.Snapshot.PoolDesired["worker"]; got != 2 {
			t.Fatalf("pass %d: PoolDesired = %d, want 2 (gc-1 resumes its copy, one seat for the twin)", pass+1, got)
		}
	}
}

// The cut runs before the claim-stall guard, so the one row a held template
// admits as its probe when the hold runs out is unassigned work, never a row
// a seat already claimed. A probe spent on a claimed row starts no seat, and
// the guard scores a probe that never woke as failed: it doubles the hold and
// alerts again.
func TestGatherDemandAdmitsTheStallProbeFromUnclaimedWork(t *testing.T) {
	h := newClaimStallHarness(t)
	h.guard.configure(claimedDemandCity())
	h.demand["worker"] = []string{"w-1", "w-2", "w-3"}
	for _, trigger := range h.demand["worker"] {
		seat := h.spawn("worker", trigger)
		h.advance(30 * time.Second)
		h.wake(seat)
		h.pass("worker")
		h.advance(claimStallDeadline)
		h.pass("worker")
	}
	if len(h.stalls()) != 1 {
		t.Fatalf("stalls = %d, want worker held after %d seats never claimed", len(h.stalls()), claimStallTrip)
	}
	h.advance(claimStallHoldBase)
	cityPath := t.TempDir()
	t.Cleanup(registerClaimStallGuard(context.Background(), cityPath, h.guard))

	cache, _ := newDemandCache(t, false, claimedByGC1("w-1"), routedDemandBead("w-2"))
	v, err := gatherDemand(demandGatherEnv{
		CityPath: cityPath, Cfg: claimedDemandCity(), CityStore: cache,
		OpenSessions: sessionInfosFromBeads([]beads.Bead{claimedSeatRow()}),
	}, recordedOpen(routedDemandBead("w-1"), routedDemandBead("w-2"))(cache))
	if err != nil {
		t.Fatalf("gatherDemand: %v", err)
	}
	if got := v.Collected.DefaultDemand["worker"].WorkBeadIDs; !slices.Equal(got, []string{"w-2"}) {
		t.Fatalf("worker demand = %v, want the probe admitted for unclaimed w-2 (claimed demand %+v)", got, v.ClaimedDemand)
	}
}

// The planner's pass record counts the cut gather makes in a real pass, once
// for a claim the recording keeps listing pass after pass. The pool is
// rig-scoped so its leg is lane-fed while the sessions store stays exact.
func TestPassRecordCountsALaggingClaimOnce(t *testing.T) {
	f := newObserveFixture(t)
	f.cur.Store(&reconcileEnv{Gen: 1, SP: f.sp, Cfg: &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "alpha", Path: t.TempDir()}},
		Agents:    []config.Agent{{Name: "worker", Dir: "alpha", StartCommand: "true", MaxActiveSessions: intPtr(3)}},
	}})
	routed := func(id string) beads.Bead {
		b := routedDemandBead(id)
		b.Metadata = map[string]string{beadmeta.RoutedToMetadataKey: "alpha/worker"}
		return b
	}
	claimed := routed("w-1")
	claimed.Status, claimed.Assignee = "in_progress", "gc-1"
	rig, _ := newDemandCache(t, false, claimed)
	f.env.RigStores = func() map[string]beads.Store { return map[string]beads.Store{"alpha": rig} }
	f.rec.Store(&externalReadsRecording{Seq: 1, FreshFor: time.Hour, Sources: map[sourceKey]sourceResult{
		keyOf(sourceDemand, rig): {EndedAt: gatherNow, sourcePayload: sourcePayload{Leg: legRecording{RawOpen: []beads.Bead{routed("w-1")}, ReadyAll: []beads.Bead{routed("w-1")}}}},
	}})

	f.runPass(0)
	f.runPass(time.Minute) // a patrol later: the record is emitted again
	if len(f.records) != 2 {
		t.Fatalf("records = %d, want 2 (one at the first pass, one a patrol later)", len(f.records))
	}
	for i, rec := range f.records {
		if got := rec[claimedDemandSeries]; got != uint64(1) {
			t.Fatalf("record %d: %s = %v (%T), want 1: one lagging claim on w-1", i+1, claimedDemandSeries, got, got)
		}
	}
}
