package api

import (
	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
)

// graphFederationLegKey returns the synthetic fan-out key the bead-list
// federation files the relocated graph store under.
//
// The list fan-out is keyed by rig name, so the graph leg needs a key of its
// own. ':' cannot appear in a rig name, so the key can never collide with a
// real one — and the merge that mints it is skipped for rig-scoped requests, so
// the key is not addressable through ?rig= either.
func graphFederationLegKey(cityName string) string {
	return "infra:" + cityName
}

// relocatedGraphStore returns the graph-class store when — and only when — the
// city has actually relocated it.
//
// Routing is keyed on STORE IDENTITY, the same rule handler_beads.go's by-id
// class resolver and handler_convoy_dispatch.go's workflow scan use: on a
// default city GraphBeadStore() returns the city store itself, so there is
// nothing to federate and every caller's extra arm stays dead. That identity
// guard is what keeps a single-store city byte-identical.
func relocatedGraphStore(state State) beads.Store {
	graph := state.GraphBeadStore().Store
	if graph == nil || graph == state.CityBeadStore() {
		return nil
	}
	return graph
}

// graphPlaneUnavailable is the authoritative failure a dead graph leg produces.
//
// This is the half of the federation that is NOT a partial degradation. A rig
// going dark is one scope reporting a hole, and Partial/partial_errors says so
// honestly. The graph plane going dark means the execution DAG — molecule
// roots, step beads, control beads — is gone from the answer, and a work-only
// 200 is indistinguishable from "the DAG finished". So the graph leg either
// answers or the request fails loud, including when it answers PARTIALLY: a
// partial graph read leaves an unnamed hole in a dependency graph, and the
// response has no way to say which part is missing.
func graphPlaneUnavailable(op string, err error) error {
	return apierr.StoreUnavailable.Msg("graph store " + op + " read failed (graph plane unreadable): " + err.Error())
}
