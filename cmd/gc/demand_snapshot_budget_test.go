package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storeref"
)

// The demand snapshot's read budget.
//
// load_demand_snapshot was 24.2s of a 373s maintainer-city tick, and its largest
// avoidable cost was a read of the remote work ledger that the operator
// invariant says does not belong on the runtime plane at all (ga-l7jdg, bd
// memory gascity-runtime-infra-store-invariant): collect_unassigned_routed,
// 8.1s, one live open-list per census leg, SEQUENTIALLY, for work the operator
// ruling says lives only in the graph store ("gc ready work will never be in the
// work db").
//
// collect_assigned_work deliberately keeps its ledger leg: a session whose only
// claim is an HQ work bead must stay visible to the census or the drain gate
// reaps a live holder (ga-w8ucu). Latency is not a reason to go blind about who
// holds what.
//
// The third cost, the per-patrol cache check, is no longer the snapshot's to
// pay: invalidation is the detector sweep's declared routed-work view
// (ready_routed_work_view.go), whose own budget is pinned next to it.

// bindingFingerprintRuntime builds a CityRuntime whose sessions class is served
// from a DIFFERENT store than the work store, the way a converged split city
// serves it.
func bindingFingerprintRuntime(t *testing.T, work, binding beads.Store) *CityRuntime {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Storage:   infraSplitConfig(cityPath).Storage,
	}
	cr := &CityRuntime{
		cityName: "test-city",
		cityPath: cityPath,
		cfg:      cfg,
		cs: &controllerState{
			cityName:      "test-city",
			cityBeadStore: work,
			eventProv:     events.NewFake(),
		},
		stderr: io.Discard,
	}
	cr.storageRoutes = &storageRoutes{binding: "infra", stores: map[coordclass.Class]beads.Store{
		coordclass.ClassGraph:     binding,
		coordclass.ClassSessions:  binding,
		coordclass.ClassMessaging: binding,
		coordclass.ClassOrders:    binding,
		coordclass.ClassNudges:    binding,
	}}
	return cr
}

// TestDemandFingerprintPatrolMaxAgeOutlivesATick is the other half of "the cache
// can engage".
//
// The max age is what licenses reuse when nothing the routed-work view sees has
// moved, so a max age at or below the tick duration means the snapshot is
// rebuilt every tick no matter how little changed and the cache is dead code. On
// maintainer-city the tick was 373s against a 30s max age.
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
