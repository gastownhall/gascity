package api

import (
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

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

// queryTopology answers the city facts a generated work_query or MCP catalog has
// to be built against, from the API's own view of the city.
//
// FederatedReady rides on relocatedGraphStore — the SAME identity gate this file
// already uses — rather than on a second reading of [storage], so the command
// this surface renders names the same reader the CLI's cityQueryTopology picks
// for the same city.
func queryTopology(state State) config.QueryTopology {
	topo := config.QueryTopology{FederatedReady: relocatedGraphStore(state) != nil}
	if cfg := state.Config(); cfg != nil {
		topo.Beads = cfg.Beads
	}
	return topo
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
//
// workLegErrors are the degraded work legs recorded before the graph leg failed.
// They ride along because this 503 replaces the Partial 200 that would have
// carried them: without them "the graph plane is down" and "every store in the
// city is down" are byte-identical responses, and the operator loses the
// work-side diagnosis the response had already collected.
func graphPlaneUnavailable(op string, err error, workLegErrors ...string) error {
	detail := "graph store " + op + " read failed (graph plane unreadable): " + err.Error()
	if len(workLegErrors) > 0 {
		detail += "; work legs also degraded: " + strings.Join(workLegErrors, "; ")
	}
	return apierr.StoreUnavailable.Msg(detail)
}
