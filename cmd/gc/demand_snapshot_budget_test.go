package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
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

	cr.readyDemandSnapshotFingerprint()

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

	got, _, refs, partial := collectOpenUnassignedRoutedWork(cityPath, cfg, binding, nil, nil, io.Discard)
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

	got, _, _, partial := collectOpenUnassignedRoutedWork("", residencyTestConfig(), store, nil, nil, io.Discard)
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
