package main

// The leg assembly and fan-out behind `gc ready`.
//
// # Why this exists
//
// On a split city the graph-class DAG — `gcg-` molecule roots, step beads,
// control beads — lives in the relocated graph store, and `bd ready` in the work
// directory cannot reach it. Measured on a live city at one moment: `gc bd ready`
// answered with 5 beads and ZERO `gcg-`, while GET /v0/beads/ready answered with
// 22 beads, 14 of them `gcg-`. The CLI was blind in bulk behind an answer that
// looked authoritative.
//
// # The contract this implements is the API's, not a new one
//
// internal/api's humaHandleBeadReady carries an executable FEDERATION CONTRACT
// (#5148) that this file is specified against, so the conformance assertion is
// CLI == API rather than an invented oracle:
//
//   - Legs, in order: the city store, then the rigs by name ascending, then the
//     relocated graph store LAST.
//   - Within a leg: whatever order that leg's own Ready reader emits. That is
//     canonical (priority, created_at, id) for a caching-wrapped work store, but
//     NOT for the graph leg — the canonical relocated binding is a
//     beads.SQLiteStore whose ready SQL orders by (created_at, id) with no
//     priority term. Per-leg order is deterministic, not canonical.
//   - Dedupe: the FIRST leg to return an id wins. The graph leg runs last, so a
//     bead co-resident in the work store and the binding — the documented steady
//     state of a migrated city, where `gc storage migrate` preserves ids and
//     never deletes back — resolves to the work store's row on both surfaces.
//
// # Where this deliberately diverges: no partial tier
//
// The API degrades a dead RIG leg to a Partial 200 because an HTTP response has
// a `partial_errors` field to say so honestly. A CLI work query has no such
// field: its whole output is the array, and a short array is indistinguishable
// from "no work". Every leg here therefore fails LOUD. Degrading would
// re-create the exact fail-open this command exists to close, one layer up.
//
// # Single-store cities take none of this
//
// relocatedGraphLegStore gates on STORE IDENTITY, the same rule the API's
// relocatedGraphStore and cmd/gc's resolveClassStore use. A city that relocates
// nothing gets no graph leg at all, so its answer is exactly the one leg it
// always had.

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
)

// readyLeg is one federated source: the store plus the name a failure reports
// it by. The label is what turns "the federation failed" into "the graph store
// failed", which is the difference between a diagnosable outage and a silent
// short answer.
type readyLeg struct {
	label string
	store beads.Store
}

// readyFederationLegs assembles the ordered leg list: the city store, the rig
// stores by name ascending, then the relocated graph store.
//
// A rig whose name equals the city name is skipped, mirroring the API exactly:
// State.BeadStores() keys the city store under CityName(), so the API's rig loop
// skips that key and the collision resolves to one leg there. Skipping it here
// keeps the two leg sequences identical by construction rather than by
// coincidence.
//
// A nil store is dropped rather than federated: a leg that cannot be opened is
// reported by whoever failed to open it, and a nil entry here would panic on
// first read.
func readyFederationLegs(cityName string, cityStore beads.Store, rigStores map[string]beads.Store, graph beads.Store) []readyLeg {
	legs := make([]readyLeg, 0, len(rigStores)+2)
	if cityStore != nil {
		legs = append(legs, readyLeg{label: "city", store: cityStore})
	}
	rigNames := make([]string, 0, len(rigStores))
	for name := range rigStores {
		if name == cityName {
			continue
		}
		rigNames = append(rigNames, name)
	}
	sort.Strings(rigNames)
	for _, name := range rigNames {
		if store := rigStores[name]; store != nil {
			legs = append(legs, readyLeg{label: "rig " + name, store: store})
		}
	}
	if graph != nil {
		legs = append(legs, readyLeg{label: "graph", store: graph})
	}
	return legs
}

// relocatedGraphLegStore returns the graph-class store to federate as the final
// leg, or nil when the city has not relocated the class.
//
// The gate is store IDENTITY, not a marker file and not a migration flag: the
// one-shot storage funnel reports whether it relocates the graph class at all,
// and a binding that resolved back to the city's own work store is the same
// store already federated as the city leg. Both answers mean "there is no second
// store", which is what keeps a legacy city's bytes untouched.
func relocatedGraphLegStore(cityPath string, cityStore beads.Store) beads.Store {
	binding, relocated := graphClassBinding(cliStorageRoutes(cityPath))
	return relocatedGraphLegFrom(binding, relocated, cityStore)
}

// relocatedGraphLegFrom is the identity gate itself, over an already-resolved
// binding. It is separate from relocatedGraphLegStore so a caller that resolved
// the binding from routes it already holds — the conformance fixture, and any
// future in-process caller — applies the SAME rule instead of restating it.
func relocatedGraphLegFrom(binding beads.Store, relocated bool, cityStore beads.Store) beads.Store {
	if !relocated || binding == nil || binding == cityStore {
		return nil
	}
	return binding
}

// federateReadyBeads reads the ready set from every leg and merges it.
//
// The per-leg read goes through the LIVE handle, which is what the API's ready
// arm does, so a caching-wrapped leg answers from its backing store rather than
// from a cache the CLI process never primed.
func federateReadyBeads(legs []readyLeg, q beads.ReadyQuery) ([]beads.Bead, error) {
	return federateBeadLegs(legs, func(store beads.Store) ([]beads.Bead, error) {
		return beads.HandlesFor(store).Live.Ready(q)
	})
}

// federateListBeads reads a status-scoped list from every leg and merges it. It
// backs the --status arm, where a graph step assigned to a worker that died
// lives in the relocated store.
//
// The read is a direct store.List rather than the live handle's, because that
// handle overrides TierMode to TierBoth and would silently discard the caller's
// tier selection. The API's list arm reads the store directly for the same
// reason.
func federateListBeads(legs []readyLeg, q beads.ListQuery) ([]beads.Bead, error) {
	return federateBeadLegs(legs, func(store beads.Store) ([]beads.Bead, error) {
		return store.List(q)
	})
}

// federateBeadLegs runs read against every leg in order and merges the results,
// deduped by id with the FIRST leg to return an id winning.
//
// Any leg error — including a partial read, which beads reports as an error
// carrying rows — aborts the whole federation. See the file header: a CLI array
// has nowhere to say "this is short".
func federateBeadLegs(legs []readyLeg, read func(beads.Store) ([]beads.Bead, error)) ([]beads.Bead, error) {
	var merged []beads.Bead
	seen := make(map[string]bool)
	for _, leg := range legs {
		rows, err := read(leg.store)
		if err != nil {
			return nil, fmt.Errorf("%s store: %w", leg.label, err)
		}
		for _, b := range rows {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			merged = append(merged, b)
		}
	}
	return merged, nil
}
