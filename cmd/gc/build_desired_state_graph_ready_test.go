package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// graphDemandReadyReader is a CachedReader/LiveReader whose Ready returns a
// fixed slice. In these tests it stands in for the FULL FEDERATED ready set
// under graph_store=sqlite: the per-tick Limit has already evicted a
// genuinely-ready graph wisp because the Dolt work leg filled the window.
type graphDemandReadyReader struct {
	ready []beads.Bead
}

func (r graphDemandReadyReader) Get(string) (beads.Bead, error)              { return beads.Bead{}, nil }
func (r graphDemandReadyReader) List(beads.ListQuery) ([]beads.Bead, error)  { return nil, nil }
func (r graphDemandReadyReader) DepList(string, string) ([]beads.Dep, error) { return nil, nil }
func (r graphDemandReadyReader) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return append([]beads.Bead(nil), r.ready...), nil
}

// graphDemandGraphOnlyReader returns the ClassGraph ready slice alone, where the
// assigned wisp survives the per-tick limit because the Dolt work leg is excluded.
type graphDemandGraphOnlyReader struct {
	ready []beads.Bead
}

func (r graphDemandGraphOnlyReader) ReadyGraphOnly(...beads.ReadyQuery) ([]beads.Bead, error) {
	return append([]beads.Bead(nil), r.ready...), nil
}

// graphDemandStore models a graph_store=sqlite Router store: the federated
// Cached/Live Ready returns a work-leg backlog that has truncated a
// genuinely-ready graph wisp out of the per-tick limit window, while the
// graph-only capability returns the wisp. The controller-demand readiness
// probes must prefer the graph-only slice (mirroring readyStoreSet) so an
// assigned, deps-satisfied wisp is recognized as ready. When hasGraph is false
// (identity phase, no distinct ClassGraph backend) the capability is absent and
// the federated path must be used byte-identically.
type graphDemandStore struct {
	beads.Store
	federated []beads.Bead
	graphOnly []beads.Bead
	hasGraph  bool
}

func (s *graphDemandStore) Handles() beads.StoreHandles {
	r := graphDemandReadyReader{ready: s.federated}
	return beads.StoreHandles{Cached: r, Live: r}
}

func (s *graphDemandStore) ReadyGraphOnlyHandle() (beads.GraphOnlyReadyStore, bool) {
	if !s.hasGraph {
		return nil, false
	}
	return graphDemandGraphOnlyReader{ready: s.graphOnly}, true
}

func graphDemandContains(rows []beads.Bead, id string) bool {
	for _, b := range rows {
		if b.ID == id {
			return true
		}
	}
	return false
}

// The assigned cleanup wisp, ready (its sole blocks-dep closed), but evicted
// from the federated per-tick limit window by older work-leg beads.
func graphDemandWisp() beads.Bead {
	return beads.Bead{ID: "gcg-1590", Status: "open", Assignee: "mc-test"}
}

func TestLiveReadyForControllerDemandPrefersGraphOnly(t *testing.T) {
	store := &graphDemandStore{
		federated: []beads.Bead{{ID: "work-1", Status: "open"}, {ID: "work-2", Status: "open"}},
		graphOnly: []beads.Bead{graphDemandWisp()},
		hasGraph:  true,
	}
	got, err := liveReadyForControllerDemandQuery(store, beads.ReadyQuery{Assignee: "mc-test", Limit: 5})
	if err != nil {
		t.Fatalf("liveReadyForControllerDemandQuery: %v", err)
	}
	if !graphDemandContains(got, "gcg-1590") {
		t.Fatalf("expected graph-only ready slice to contain the assigned wisp gcg-1590, got %v", got)
	}
}

func TestControllerDemandReadyFallsBackWithoutGraphCapability(t *testing.T) {
	// Identity phase: no distinct ClassGraph backend -> GraphOnlyReadyFor=false
	// -> the federated Live.Ready set is returned byte-identically, so default
	// (Dolt-only) cities are unaffected by the fix.
	store := &graphDemandStore{
		federated: []beads.Bead{{ID: "work-1", Status: "open"}},
		graphOnly: []beads.Bead{graphDemandWisp()},
		hasGraph:  false,
	}
	got, err := liveReadyForControllerDemandQuery(store, beads.ReadyQuery{Limit: 5})
	if err != nil {
		t.Fatalf("liveReadyForControllerDemandQuery: %v", err)
	}
	if len(got) != 1 || got[0].ID != "work-1" {
		t.Fatalf("expected federated fallback [work-1], got %v", got)
	}
	if graphDemandContains(got, "gcg-1590") {
		t.Fatalf("graph-only wisp must NOT appear when the graph capability is absent")
	}
}
