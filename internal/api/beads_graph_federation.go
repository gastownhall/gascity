package api

import (
	"strings"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/storeref"
)

// relocatedGraphStore returns the graph-class handle when — and only when —
// the city has actually relocated it.
//
// Routing is keyed on STORE IDENTITY (beads.SameStore), the same rule
// Stores.Plan and handler_convoy_dispatch.go's workflow scan use: on a default
// city Stores().Graph() wraps the city's work store itself, so there is
// nothing to federate and every caller's extra arm stays dead. That identity
// guard is what keeps a single-store city byte-identical. Under the
// split-strict gate the graph handle reads the work store's class view, a
// store of its own, so a gate fixture keeps its split shape.
func relocatedGraphStore(state State) (beads.GraphStore, bool) {
	st := state.Stores()
	graph := st.Graph()
	if beads.IsNilStore(graph) || beads.SameStore(graph, st.Work()) {
		return beads.GraphStore{}, false
	}
	return graph, true
}

// graphBindingPrefixes is the reserved id namespaces of the binding st serves
// the graph class from, as cmd/gc's readyLegReservedPrefixes reads them for
// `gc ready`, or nil when the graph class is not relocated. The cross-store
// ready rule takes the binding's own verdict on a blocker in one of them,
// which no other store can hold, and reads every copy of any other.
func graphBindingPrefixes(st storeref.Stores) []string {
	binding, ok := st.Topology().BindingFor(coordclass.ClassGraph)
	if !ok {
		return nil
	}
	return append([]string(nil), binding.Prefixes...)
}

// queryTopology answers the city facts a generated work_query or MCP catalog has
// to be built against, from the API's own view of the city.
//
// FederatedReady rides on relocatedGraphStore — the SAME identity gate this file
// already uses — rather than on a second reading of [storage], so the command
// this surface renders names the same reader the CLI's cityQueryTopology picks
// for the same city.
func queryTopology(state State) config.QueryTopology {
	_, federated := relocatedGraphStore(state)
	topo := config.QueryTopology{FederatedReady: federated}
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
