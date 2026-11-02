package main

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The demand snapshot's read budget.
//
// load_demand_snapshot was 24.2s of a 373s maintainer-city tick, and two of its
// three costs were reads of the remote work ledger that the operator invariant
// says do not belong on the runtime plane at all (ga-l7jdg, bd memory
// gascity-runtime-infra-store-invariant):
//
//   - the CACHE CHECK itself. readyDemandSnapshotFingerprint issued one ReadyLive
//     per store — city ledger, sessions binding, every rig — so asking "may I
//     reuse the snapshot?" cost more remote round trips than most legs.
//   - collect_unassigned_routed, 8.1s: one live open-list per census leg,
//     SEQUENTIALLY, for work the operator ruling says lives only in the graph
//     store ("gc ready work will never be in the work db").
//
// The third, collect_assigned_work, deliberately keeps its ledger leg: a session
// whose only claim is an HQ work bead must stay visible to the census or the
// drain gate reaps a live holder (ga-w8ucu). Latency is not a reason to go blind
// about who holds what.

// TestDemandFingerprintIssuesZeroLedgerReadsOnASplitCity pins the cache check to
// the infra binding.
func TestDemandFingerprintIssuesZeroLedgerReadsOnASplitCity(t *testing.T) {
	ledger := &countingReadyStore{Store: beads.NewMemStore()}
	binding := &countingReadyStore{Store: beads.NewMemStore()}
	cr := bindingFingerprintRuntime(t, ledger, binding)
	routedStepIn(t, binding, "routed graph step")
	routedStepIn(t, ledger, "routed work-ledger bead")

	cr.readyDemandSnapshotFingerprint(nil)

	if ledger.readyCalls != 0 {
		t.Fatalf("the demand cache check issued %d work-ledger Ready read(s), want 0 — at maintainer-city's ~5.4s RTT that is %v spent deciding whether to reuse a cache",
			ledger.readyCalls, time.Duration(ledger.readyCalls)*5400*time.Millisecond)
	}
	// Control: the binding answered, so the zero above is a routing fact and not
	// a fingerprint that stopped reading.
	if binding.readyCalls != 1 {
		t.Fatalf("binding Ready reads = %d, want exactly 1", binding.readyCalls)
	}
}

// TestDemandFingerprintPatrolMaxAgeOutlivesATick is the other half of "the cache
// can engage".
//
// The age gate SHORT-CIRCUITS the fingerprint — loadDemandSnapshot only computes
// the fingerprint when the snapshot is not already due — so a max age at or below
// the tick duration means the fingerprint is never consulted and the snapshot is
// rebuilt every tick no matter how little changed. On maintainer-city the tick
// was 373s against a 30s max age, and the whole cache was dead code.
func TestDemandFingerprintPatrolMaxAgeOutlivesATick(t *testing.T) {
	cr := bindingFingerprintRuntime(t, beads.NewMemStore(), beads.NewMemStore())
	maxAge := cr.demandSnapshotPatrolMaxAge()
	if !cr.demandSnapshotsEnabled() {
		t.Fatal("the fixture is not event-backed, so the fingerprint path is not the one being measured")
	}
	// The default patrol interval is 30s and a tick may legitimately take
	// several of them. A max age that does not clear that window makes the
	// fingerprint unreachable.
	if maxAge <= 2*time.Minute {
		t.Fatalf("event-backed demand may be reused for %v; that is inside one slow tick, so the ready fingerprint below the age gate is never consulted and the snapshot rebuilds every tick", maxAge)
	}
}

// TestCollectOpenUnassignedRoutedWorkReadsTheBindingAloneOnASplitCity is the
// operator ruling on the routed-demand read: routed work lives ONLY in the graph
// store, so the census legs the runtime plane serves it from are the bindings.
func TestCollectOpenUnassignedRoutedWorkReadsTheBindingAloneOnASplitCity(t *testing.T) {
	cityPath := t.TempDir()
	cfg := residencyTestConfig()
	ledgerBacking, bindingBacking := beads.NewMemStore(), beads.NewMemStore()
	ledgerBacking.HonorExplicitIDs = true
	bindingBacking.HonorExplicitIDs = true
	ledger := &routedDemandCountingStore{Store: ledgerBacking}
	binding := &routedDemandCountingStore{Store: bindingBacking}
	routes := splitRoutes(binding)
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return ledger })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	seedRoutedOpenBead(t, binding, "gcg-routed-1")
	seedRoutedOpenBead(t, ledger, "ga-routed-1")

	got, _, refs, partial := collectOpenUnassignedRoutedWork(cityPath, cfg, binding, nil, nil, io.Discard, nil, nil)
	if partial {
		t.Fatal("routed demand reported partial over healthy legs")
	}
	if ledger.listCalls != 0 {
		t.Fatalf("routed demand issued %d work-ledger List(s), want 0 — routed work is not in the work db (operator ruling, ga-4qdfn)", ledger.listCalls)
	}
	if binding.listCalls == 0 {
		t.Fatal("the binding was not read either; the ledger zero proves nothing")
	}
	if len(got) != 1 || got[0].ID != "gcg-routed-1" {
		t.Fatalf("routed demand = %v, want the one binding-resident routed bead", beadIDsOf(got))
	}
	if len(refs) != len(got) {
		t.Fatalf("refs = %v for %d bead(s); the index-aligned slices have drifted", refs, len(got))
	}
	if !storeref.IsClassRef(refs[0]) {
		t.Fatalf("routed bead attributed to ref %q, want the binding's class ref", refs[0])
	}
}

// TestCollectOpenUnassignedRoutedWorkReadsTheOnlyStoreOnASingleStoreCity is the
// degradation half: with no binding, the work store IS the infra store.
func TestCollectOpenUnassignedRoutedWorkReadsTheOnlyStoreOnASingleStoreCity(t *testing.T) {
	backing := beads.NewMemStore()
	backing.HonorExplicitIDs = true
	store := &routedDemandCountingStore{Store: backing}
	seedRoutedOpenBead(t, store, "ga-routed-1")

	got, _, _, partial := collectOpenUnassignedRoutedWork("", residencyTestConfig(), store, nil, nil, io.Discard, nil, nil)
	if partial {
		t.Fatal("routed demand reported partial over a healthy single store")
	}
	if store.listCalls == 0 {
		t.Fatal("a single-store city's routed demand read nothing; the runtime plane must degrade to \"the only store there is\"")
	}
	if len(got) != 1 || got[0].ID != "ga-routed-1" {
		t.Fatalf("routed demand = %v, want the one routed bead", beadIDsOf(got))
	}
}

func seedRoutedOpenBead(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if _, err := store.Create(beads.Bead{
		ID:       id,
		Title:    id,
		Type:     "task",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	}); err != nil {
		t.Fatalf("seed %q: %v", id, err)
	}
}

// routedDemandCountingStore counts List round trips, which is the unit a remote leg's
// latency is actually made of.
type routedDemandCountingStore struct {
	beads.Store
	listCalls int
}

func (s *routedDemandCountingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.Store.List(q)
}

// gateCountingLeg counts the remote round trips the demand pass's cross-store
// gate makes of a work ledger: its by-id blocker reads (beads.GetMany, through
// the engine's exact batch read), its single-bead Gets, and its edge reads. A
// List carrying ids is counted apart: the native engine cannot narrow a listing
// to ids, so it is a by-id read that walks the whole ledger. The leg embeds the
// Store interface, so it strands nothing and the gate re-judges every row the
// ledger serves, as it does over bd/Dolt; and because an embedded interface
// hides every optional capability, it forwards the exact batch read explicitly,
// as each production wrapper does.
type gateCountingLeg struct {
	beads.Store
	byID, gets, listByID, edges atomic.Int64
}

func (s *gateCountingLeg) GetExactBatch(ids []string) (map[string]beads.Bead, []string, error) {
	s.byID.Add(1)
	getter, ok := s.Store.(beads.ExactBatchGetter)
	if !ok {
		return nil, nil, beads.ErrExactBatchGetUnsupported
	}
	return getter.GetExactBatch(ids)
}

func (s *gateCountingLeg) Get(id string) (beads.Bead, error) {
	s.gets.Add(1)
	return s.Store.Get(id)
}

func (s *gateCountingLeg) List(q beads.ListQuery) ([]beads.Bead, error) {
	if len(q.IDs) > 0 {
		s.listByID.Add(1)
	}
	return s.Store.List(q)
}

func (s *gateCountingLeg) DepList(id, direction string) ([]beads.Dep, error) {
	s.edges.Add(1)
	return s.Store.DepList(id, direction)
}

// TestGatedDemandReadsTheLedgerOncePerReadPoint is the gated path's budget
// (#8360 review, Finding 2). Once the cross-store gate arms, the demand pass
// judges ledger rows inside the tick, and each read point gates a cached and a
// live snapshot of every store. The gate it keeps for the read point reads each
// ledger row's edges and each blocker once, however many snapshots it gates.
// The control is the same snapshots gated one fresh gate each, which reads the
// ledger again for every snapshot.
//
// The blocker read is also KEYED (#8360 review, iteration 2 blocker): the leg is
// the controller's shape, the bead policy over a CachingStore over the native
// engine, and the read must reach the engine's exact batch read through it. A
// read by id that took a listing instead walks the whole ledger, and the
// engine counts that listing as unkeyed.
func TestGatedDemandReadsTheLedgerOncePerReadPoint(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "gc", Prefix: "gc"},
		Storage:   splitEnvStorageConfig(),
	}
	engine := splittest.NewNativeWorkStore(t, "gc")
	counting := &gateCountingLeg{Store: engine}
	cached := beads.NewCachingStoreForTest(counting, nil)
	ledger := wrapStoreWithBeadPolicies(cached, cfg)
	binding := newSplitEnvClassLeaf(t)
	routes := splitEnvRoutes(binding)
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return ledger })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	step, err := binding.Create(beads.Bead{Title: "graph blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	const rows = 4
	for i := 0; i < rows; i++ {
		row, err := engine.Create(beads.Bead{Title: "routed work", Type: "task", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"}})
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.DepAdd(row.ID, "external:"+step.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	legs, err := readyLegsForTopology(residencyTopologyForCity(cityPath, cfg, ledger, nil))
	if err != nil {
		t.Fatal(err)
	}
	if !readywork.IsSplit(legs) {
		t.Fatal("fixture: the legs do not read as a split city")
	}
	// The controller's ledger cache is primed long before a demand pass; its
	// prime reads every row's edges once, which is the cache's cost, not the
	// gate's.
	if err := cached.Prime(context.Background()); err != nil {
		t.Fatal(err)
	}
	reset := func() {
		counting.byID.Store(0)
		counting.gets.Store(0)
		counting.listByID.Store(0)
		counting.edges.Store(0)
	}
	unkeyed := func() int64 {
		t.Helper()
		stats, ok := beads.ListRequestStatsOf(engine)
		if !ok {
			t.Fatal("fixture: the native engine keeps no listing counts")
		}
		return stats.Unkeyed
	}

	// One read point, as the demand pass reads it: handed the sessions binding,
	// a cached and a live snapshot of every leg.
	reset()
	unkeyedBefore := unkeyed()
	cache := newReadyDemandCache().withCrossStoreGate(cityPath, cfg, binding, nil, nil)
	for _, leg := range legs {
		if _, err := cache.controllerDemandReady(leg.Store, leg.Label); err != nil {
			t.Fatalf("demand ready on %s: %v", leg.Label, err)
		}
		if _, err := cache.liveReady(leg.Store, leg.Label, beads.ReadyQuery{Limit: 100}); err != nil {
			t.Fatalf("live demand ready on %s: %v", leg.Label, err)
		}
	}
	byID, edges := counting.byID.Load(), counting.edges.Load()
	t.Logf("one read point: %d ledger blocker read(s), %d single Get(s), %d List(s) by id, %d ledger edge read(s)",
		byID, counting.gets.Load(), counting.listByID.Load(), edges)
	if byID != 1 {
		t.Errorf("the read point read the ledger's blockers %d time(s), want 1: a blocker is read once per read point, in one exact batch", byID)
	}
	if n := counting.listByID.Load(); n != 0 {
		t.Errorf("the read point listed the ledger by id %d time(s), want 0: the native engine walks the whole ledger for a listing by id", n)
	}
	if n := counting.gets.Load(); n != 0 {
		t.Errorf("the read point read the ledger %d time(s) one bead at a time, want 0: the blockers are one batched read", n)
	}
	if n := unkeyed() - unkeyedBefore; n != 0 {
		t.Errorf("the read point sent the ledger %d unkeyed listing request(s), want 0", n)
	}
	if edges != rows {
		t.Errorf("the read point read %d ledger edge list(s) for %d rows, want %d: each row's edges are read once", edges, rows, rows)
	}

	// Control: the same two snapshots of the ledger, each through a fresh gate.
	reset()
	ready, err := beads.HandlesFor(ledger).Live.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := readywork.NewLegGate(legs, beads.ReadyQuery{TierMode: beads.TierBoth}).Gate(context.Background(), 0, ready); err != nil {
			t.Fatal(err)
		}
	}
	if got := counting.byID.Load(); got != 2*byID {
		t.Errorf("control: two fresh gates read the ledger's blockers %d time(s), want %d; the counter does not see what it is meant to bound", got, 2*byID)
	}
}
