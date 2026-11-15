package main

import (
	"github.com/gastownhall/gascity/internal/config"
)

// promptQueryTopology is the topology the four discovery placeholders of an
// agent's prompt render against. A cross-store-eligible agent's `gc hook`
// already fans out over the city and every rig store, so its query stays the
// single-store `bd` form and runs once per store. Its placeholders run
// verbatim in the agent's own shell with no such fan-out, so on a city with
// rigs they read through the federated `gc ready` instead of one bd store —
// the only reader that spans the stores a routed bead may live in.
//
// The trigger is CONFIGURED rigs, not serving rigs: suspension is runtime
// state, and keying on it would make the rendered prompt flip on every
// `gc rig suspend`/`resume`. `gc ready` skips suspended rigs at read time.
//
// FederatedReady is only ever set here, never cleared: a city that already
// federates (a relocated coordination class) keeps the topology its resolver
// answered, and every other shape — rig-scoped agents, rig-less cities — is
// untouched, so their prompts render byte-for-byte what base rendered.
//
// The cost is that these placeholders depend on every serving rig's store. `gc
// ready` fails the whole read when one leg cannot be opened or read — its answer
// is a bare array with nowhere to say it is short — and the generated query
// fails with it (config's workQueryReadFailure), naming the rig on stderr. So
// while one rig store is broken, a city-scoped agent's discovery placeholders
// exit non-zero rather than answer from the stores that still read, even for a
// claim the agent holds in the city store. That is deliberate: an answer short
// a store is the false "no work" the federated reader exists to rule out, and
// this failure is loud and names its rig. `gc hook` reads each store on its
// own and still serves work it finds in the stores it could read, so in that
// state the hook can serve what the placeholders cannot.
//
// The result is a per-agent topology for rendering the prompt, not a city fact.
// Nothing that reads FederatedReady as "this city is split" may be handed it —
// FederationBlindOverrides, singleStoreHookWorkQuery, the hook's store scoping
// — or it would take a city-scoped agent's rig city for a split one.
func promptQueryTopology(topo config.QueryTopology, a *config.Agent, rigs []config.Rig) config.QueryTopology {
	if !topo.FederatedReady && agentIsCrossStoreEligible(a) && len(rigs) > 0 {
		topo.FederatedReady = true
	}
	return topo
}
