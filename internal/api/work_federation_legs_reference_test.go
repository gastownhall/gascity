package api

// Frozen reference copies (typed-stores T7b.3, plan principle 3) of the API's
// work leg builders as they stand on main at 6bd609f5bf, before T7e.2 reads
// storeref's WorkFederation legs: the GET /v0/beads fan-out
// (beadListFanOut, with relocatedGraphStore), the head of statusWorkCounts
// that picks its queries, and the retired-session sweep's store list
// (retiredSessionWorkStores). sortedRigNames is T7d's frozen copy
// (frozenSortedRigNames). The equivalence table
// (work_federation_legs_equivalence_test.go) runs each beside the plan.
//
// statusWorkCounts is copied up to the point its queries are chosen: what it
// does with them (concurrent per-store counts and ready reads, the tallies) is
// not the plan's to change, and the table reads each query's store itself.
// The reassign functions the sweep runs over each store are copied too, since
// T7e.2 hands them the sweep's writable handles. Everything else the copies
// call is live code T7e.2 leaves byte-identical: State's accessors,
// Server.loadCacheColdRigs and cacheColdRigNames.
//
// The copies keep main's code, read markers included: markers in test files
// are neither linted (storeclass) nor ledgered (reviewedMarkers).

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// frozenBeadListLeg is main's beadListLeg.
type frozenBeadListLeg struct {
	label string
	store beads.Store
	// graph marks the relocated graph plane, which does not degrade: it either
	// answers or the request fails loud (see graphPlaneUnavailable).
	graph bool
}

// frozenBeadListLegs is the head of main's humaHandleBeadList that picks its
// legs (huma_handlers_beads.go:94-104), verbatim but for the input.
func frozenBeadListLegs(state State, rig string) []frozenBeadListLeg {
	stores := state.BeadStores()
	var rigNames []string
	if rig != "" {
		if _, ok := stores[rig]; ok {
			rigNames = []string{rig}
		}
	} else {
		rigNames = frozenSortedRigNames(stores)
	}
	return frozenBeadListFanOut(state, stores, rigNames, rig != "")
}

// frozenBeadListFanOut is main's beadListFanOut.
func frozenBeadListFanOut(state State, stores map[string]beads.Store, rigNames []string, rigScoped bool) []frozenBeadListLeg {
	legs := make([]frozenBeadListLeg, 0, len(rigNames)+1)
	for _, rigName := range rigNames {
		legs = append(legs, frozenBeadListLeg{label: "rig " + rigName, store: stores[rigName]})
	}
	if rigScoped {
		return legs
	}
	if graph, ok := frozenRelocatedGraphStore(state); ok {
		legs = append(legs, frozenBeadListLeg{label: "graph", store: graph, graph: true})
	}
	return legs
}

// frozenRelocatedGraphStore is main's relocatedGraphStore.
func frozenRelocatedGraphStore(state State) (beads.GraphStore, bool) {
	st := state.Stores()
	graph := st.Graph()
	if beads.IsNilStore(graph) || beads.SameStore(graph, st.Work()) {
		return beads.GraphStore{}, false
	}
	return graph, true
}

// frozenWorkQuery is main's statusWorkCounts' workQuery.
type frozenWorkQuery struct {
	label         string
	store         beads.Store
	includeStored bool
	includeReady  bool
}

// frozenStatusWorkQueries is main's statusWorkCounts up to its queries
// (handler_status.go:953-988), with the excluded count it reports.
func frozenStatusWorkQueries(s *Server, cacheColdRigs map[string]bool) ([]frozenWorkQuery, int) {
	stores := s.state.BeadStores()
	// sortedRigNames deduplicates rigs sharing one store instance, so each
	// store's persisted statuses are counted exactly once.
	rigNames := frozenSortedRigNames(stores)
	queries := make([]frozenWorkQuery, 0, len(rigNames)+1)
	if cityStore := s.state.CityBeadStore(); cityStore != nil {
		queries = append(queries, frozenWorkQuery{
			label:        "city",
			store:        cityStore,
			includeReady: true,
		})
	}
	cityName := s.state.CityName()
	excluded := 0
	for _, rigName := range rigNames {
		if cacheColdRigs[rigName] {
			excluded++
			continue
		}
		queries = append(queries, frozenWorkQuery{
			label:         "rig " + rigName,
			store:         stores[rigName],
			includeStored: true,
			includeReady:  rigName != cityName,
		})
	}

	if graph := s.state.Stores().Graph().Store; graph != nil && !slices.ContainsFunc(queries, func(q frozenWorkQuery) bool { return q.store == graph }) {
		queries = append(queries, frozenWorkQuery{label: "graph", store: graph, includeReady: true})
	}
	return queries, excluded
}

// frozenRetiredSessionWorkStores is main's retiredSessionWorkStores.
func frozenRetiredSessionWorkStores(s *Server, sessions beads.Store) []beads.Store {
	if _, relocated := frozenRelocatedGraphStore(s.state); !relocated {
		return []beads.Store{sessions} // residency:allow — a city that relocates nothing sweeps the one sessions store it always swept
	}
	var out []beads.Store
	add := func(store beads.Store) {
		if store == nil {
			return
		}
		// SameStore, not ==: a class handle over a store already listed is
		// that store, and sweeping it twice moves nothing more.
		for _, listed := range out {
			if beads.SameStore(listed, store) {
				return
			}
		}
		out = append(out, store)
	}
	add(s.state.CityBeadStore())
	cold := s.loadCacheColdRigs()
	stores := s.state.BeadStores() // residency:allow — the reassign sweep above: every store that can hold the retired session's work
	// Cold names drop before add dedupes the stores, so a store a cold rig
	// shares stays reachable through a rig that is not.
	for _, rigName := range slices.Sorted(maps.Keys(stores)) {
		if !cold[rigName] {
			add(stores[rigName])
		}
	}
	add(s.state.Stores().Graph().Store)
	add(sessions)
	return out
}

// frozenReassignHeldOpenWorkAssignedToSession is main's
// reassignHeldOpenWorkAssignedToSession.
func frozenReassignHeldOpenWorkAssignedToSession(store beads.Store, oldID, newID string) error {
	if store == nil || strings.TrimSpace(oldID) == "" || strings.TrimSpace(newID) == "" {
		return nil
	}
	for _, status := range retiredSessionWorkStatuses {
		work, err := store.List(beads.ListQuery{Assignee: oldID, Status: status, TierMode: beads.TierBoth})
		if err != nil {
			return fmt.Errorf("looking up work held by retired session %s: %w", oldID, err)
		}
		held := func(item beads.Bead) bool {
			parked := item.SourceStatus != "" && item.SourceStatus != status
			return !parked && !session.IsSessionBeadOrRepairable(item)
		}
		if slices.ContainsFunc(work, held) {
			return frozenReassignOpenWorkAssignedToSession(store, oldID, newID)
		}
	}
	return nil
}

// frozenReassignOpenWorkAssignedToSession is main's
// reassignOpenWorkAssignedToSession.
func frozenReassignOpenWorkAssignedToSession(store beads.Store, oldID, newID string) error {
	if store == nil || strings.TrimSpace(oldID) == "" || strings.TrimSpace(newID) == "" {
		return nil
	}
	for _, status := range retiredSessionWorkStatuses {
		work, err := store.List(beads.ListQuery{Assignee: oldID, Status: status, Live: true, TierMode: beads.TierBoth})
		if err != nil {
			return fmt.Errorf("listing work assigned to retired session %s: %w", oldID, err)
		}
		for _, item := range work {
			if session.IsSessionBeadOrRepairable(item) {
				continue
			}
			if err := store.Update(item.ID, beads.UpdateOpts{Assignee: &newID}); err != nil {
				return fmt.Errorf("reassign work %s from retired session %s to %s: %w", item.ID, oldID, newID, err)
			}
		}
	}
	return nil
}
