package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

// reviewedStoreIdentitySites are this package's store-identity comparisons
// and store-keyed maps (typed-stores T6 plan, OD-9, R3), keyed "file func
// rule". Every one compares two raw stores today, which is why it answers
// correctly; each is rewritten to beads.SameStore or beads.PhysicalStoreKey
// by the PR that hands it a class handle. The table only shrinks: a new
// comparison uses SameStore.
var reviewedStoreIdentitySites = map[string]beadstest.IdentitySite{
	"beads_graph_federation.go Server.bindingReservedPrefixes eq":     {Count: 1, Reason: "binding.Leg.Store == store: both raw (residency topology legs, relocatedGraphStore); T7 by-id residency"},
	"beads_graph_federation.go relocatedGraphStore eq":                {Count: 1, Reason: "GraphBeadStore().Store == CityBeadStore(): both raw unwraps; T7 (relocatedGraphStore is residue)"},
	"handler_agents.go activeBeadSearchScope eq":                      {Count: 1, Reason: "stores[name] == shared over the raw BeadStores() map; T6c workStoresByName"},
	"handler_agents_graph_work.go Server.graphActiveWorkBySession eq": {Count: 1, Reason: "graphStore == CityBeadStore(): both raw; T6c moves it to st.Graph() and SameStore"},
	"handler_beads.go sortedRigNames map-key":                         {Count: 1, Reason: "dedupes the raw BeadStores() map by value; T7 (sortedRigNames is residue)"},
	"handler_convoy_dispatch.go workflowStoreByRef eq":                {Count: 2, Reason: "graph/orders store == CityBeadStore(): raw unwraps; T7 workflow legs"},
	"handler_convoy_dispatch.go workflowStoresWith eq":                {Count: 1, Reason: "GraphBeadStore().Store != the raw city store; T7 workflow legs"},
	"handler_convoys.go isGraphConvoyID eq":                           {Count: 1, Reason: "GraphBeadStore().Store != CityBeadStore(): both raw; T7 (federated marker)"},
	"handler_status.go Server.statusWorkCounts eq":                    {Count: 1, Reason: "q.store == graph over raw work-count legs; T7 status federation"},
	"huma_handlers_beads.go Server.storeClassHolds eq":                {Count: 1, Reason: "binding.Leg.Store == store: raw topology legs and the raw by-id owner; T7 holds rule"},
	"huma_handlers_orders.go appendOrdersClassStoreInfo eq":           {Count: 1, Reason: "info.store == ordersStore: raw legs and a raw unwrap; T7 orders legs"},
	"residency_by_id.go Server.workLegFor eq":                         {Count: 1, Reason: "leg.Store == store over raw by-id legs; T7 by-id residency"},
	"residency_topology.go Server.residencyTopology eq":               {Count: 2, Reason: "a class store == the raw city store; T7 residency topology"},
	"residency_topology.go Server.residencyTopology map-key":          {Count: 1, Reason: "groups raw class stores into bindings by store value; T7 residency topology"},
	"residency_topology.go apiResidencyBindings map-key":              {Count: 1, Reason: "the grouping map residencyTopology builds; T7 residency topology"},
	"rig_graph_legs.go workflowGraphStoreInfo eq":                     {Count: 1, Reason: "graphStore == CityBeadStore(): both raw; T7 workflow legs"},
	"session_ops.go sameStoreValue eq":                                {Count: 1, Reason: "the panic-safe raw == helper serverOps.store uses; T6b replaces it with beads.SameStore"},
}

// TestStoreIdentityGuard holds internal/api's store comparisons to the
// reviewed table.
func TestStoreIdentityGuard(t *testing.T) {
	beadstest.CheckStoreIdentity(t, ".", reviewedStoreIdentitySites)
}

// TestAPIBeadRouterHonorsTheGraphBindingThroughAHandle pins apiBeadRouter's
// identity test to SameStore (T6a, R3): the core names the store that holds
// the routed bead, and on a city whose graph is relocated the route honors it
// when it is the graph binding, whether the core names it raw or as a class
// handle. With == a handle never matched, so a standalone formula root minted
// in the binding was routed through the sling's own store and the route
// failed.
func TestAPIBeadRouterHonorsTheGraphBindingThroughAHandle(t *testing.T) {
	for name, wrap := range apiHandleWraps {
		state := newFakeState(t)
		city, binding, rig := beads.NewMemStore(), beads.NewMemStore(), beads.NewMemStore()
		state.cityBeadStore = city
		state.graphBeadStore = binding
		root, err := binding.Create(beads.Bead{Title: "wisp root"})
		if err != nil {
			t.Fatal(err)
		}
		r := apiBeadRouter{server: New(state), store: rig}
		if err := r.Route(t.Context(), sling.RouteRequest{BeadID: root.ID, Target: "myrig/worker", Store: wrap(binding)}); err != nil {
			t.Fatalf("%s: route: %v", name, err)
		}
		got, err := binding.Get(root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Metadata[beadmeta.RoutedToMetadataKey] == "" {
			t.Errorf("%s: the root in the graph binding was not routed", name)
		}
	}
}

// TestWorkRecordLegacyRepoDirMatchesARigThroughAHandle pins
// workRecordLegacyRepoDir's identity test to SameStore (T6a, R3): a bead read
// through a class handle over a rig's store is judged against that rig's
// checkout, as one read through the raw store is. With == the handle matched
// no rig and the ADR-0009 gate judged a rig bead against the city checkout.
func TestWorkRecordLegacyRepoDirMatchesARigThroughAHandle(t *testing.T) {
	for name, wrap := range apiHandleWraps {
		state := newFakeState(t)
		rigPath := t.TempDir()
		state.cfg.Rigs = []config.Rig{{Name: "myrig", Path: rigPath}, {Name: "unbound"}}
		s := New(state)
		dirs := s.workRecordScopeDirs()
		if got := s.workRecordLegacyRepoDir(wrap(state.BeadStore("myrig")), dirs); got != rigPath {
			t.Errorf("%s over the rig's store: repo dir %q, want the rig checkout %q", name, got, rigPath)
		}
		if got := s.workRecordLegacyRepoDir(wrap(beads.NewMemStore()), dirs); got != dirs.CityDir() {
			t.Errorf("%s over another store: repo dir %q, want the city checkout %q", name, got, dirs.CityDir())
		}
	}
}

// TestRetiredSessionWorkStoresDedupesAHandleOverAListedStore pins
// retiredSessionWorkStores's dedupe to SameStore (typed-stores T6 plan, R3): a
// sessions store handed in as a class handle over a store the sweep already
// lists is that store, so the sweep lists it once. slices.Contains compared
// the handle with ==, never equal to the raw store, and swept it twice.
func TestRetiredSessionWorkStoresDedupesAHandleOverAListedStore(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.graphBeadStore = beads.NewMemStore()
	s := New(state)
	raw := s.retiredSessionWorkStores(state.CityBeadStore())
	handle := s.retiredSessionWorkStores(beadstest.Sessions(state.CityBeadStore()))
	if len(handle) != len(raw) {
		t.Fatalf("the sweep over a sessions handle lists %d stores, over the raw store %d: the handle was listed beside the store it wraps", len(handle), len(raw))
	}
	for i := range raw {
		if !beads.SameStore(handle[i], raw[i]) {
			t.Errorf("store %d: %v, want %v", i, handle[i], raw[i])
		}
	}
}
