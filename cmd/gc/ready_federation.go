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
// What the API actually does, leg by leg, because the divergence is only
// meaningful stated against the real thing:
//
//   - A RIG leg that fails — whether it failed to OPEN, in which case
//     controllerState.buildStores puts an unavailableStore in the map and every
//     read of it returns that open error, or failed to READ — degrades to a
//     Partial 200 whose partial_errors name the rig.
//   - The GRAPH leg does not degrade: humaHandleBeadReady returns 503
//     (graphPlaneUnavailable), carrying any rig errors recorded before it.
//   - A rig declared in city.toml with no .gc/site.toml binding has an empty
//     rig.Path and is SKIPPED, with no store and no message.
//
// A CLI work query has no `partial_errors` field: its whole output is the array,
// and a short array is indistinguishable from "no work". So where the API can be
// honestly partial, this command cannot, and every leg it federates fails LOUD —
// at BOTH ends. federateBeadLegs is the read end. readyRigLegStores is the open
// end, and it is the end that was the hole: leg assembly runs before any read, so
// a rig store that could not be opened used to be dropped by the builder, warned
// about on a stream no work query parses, and answered with exit 0 and a
// valid-looking short array. That is the exact fail-open this command exists to
// close, re-created one layer up.
//
// The UNBOUND rig is left skipped, matching the API. It is not a leg the city
// failed to reach — it is a rig with no store at all, so no claimable row is
// hiding behind it, and the acceptance criterion for this whole slice is CLI ==
// API. Promoting it to an error is a defensible product decision, but it is a
// behavior change that has to land on BOTH surfaces at once.
//
// # Every leg answers the SAME question
//
// The legs are not wrapped alike. A work store sits behind cmd/gc's bead-policy
// layer, which rewrites a TierIssues read to TierBoth before it reaches the
// backend; a relocated class store has no such layer, because openStorageRoutes
// keys the class map straight to the engine value the provider returned. So a
// query that leaves TierMode at its zero value asks the work legs one question
// and the class leg a narrower one, and the merge presents the two as one
// answer. The class store's ephemeral tier — the wisps an orchestration step
// runs as — drops out with no error and no short-array signal (ga-8lyxc).
//
// Every read below therefore states beads.FederatedReadTier explicitly on every
// leg. That is the tier the policy-wrapped legs have always answered at, so it
// changes nothing for a city that relocates no class.
//
// # Single-store cities take none of this
//
// relocatedGraphLegStore gates on STORE IDENTITY, the same rule the API's
// relocatedGraphStore and cmd/gc's resolveClassStore use. A city that relocates
// nothing gets no graph leg at all, so its answer is exactly the one leg it
// always had.

import (
	"errors"
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
// A nil store is dropped rather than federated: a nil entry would panic on first
// read, and by the time the legs are assembled a store that could not be opened
// has already failed the whole query at readyRigLegStores. Dropping here is
// therefore a guard against a nil, not a silent degrade.
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

// readyRigLegStores opens the rig legs, failing the whole query when any BOUND
// rig's store cannot be opened.
//
// This is the OPEN end of the fail-loud rule federateBeadLegs applies at the read
// end, and the two are the same rule for the same reason: a leg missing from the
// federation is a leg whose claimable rows are missing from the array, and the
// array is the entire answer. The controller's opener,
// buildStandaloneRigStores, warns and continues instead — correctly, for a
// process whose job is to keep supervising the rigs that are up. Wiring `gc
// ready` to that policy is what let a dead rig produce exit 0 and a short array.
//
// Every dead rig is named, not just the first: unlike a read, the opens have all
// already happened by here, so reporting one and discarding the rest would throw
// away diagnosis already paid for.
func readyRigLegStores(cfg *config.City, cityPath string) (map[string]beads.Store, error) {
	stores, failures := openStandaloneRigStores(cfg, cityPath)
	if len(failures) == 0 {
		return stores, nil
	}
	errs := make([]error, 0, len(failures))
	for _, f := range failures {
		errs = append(errs, fmt.Errorf("rig %q store: %w", f.rig, f.err))
	}
	return nil, errors.Join(errs...)
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
// reason. The caller states the tier it wants (readReadyCandidates), so nothing
// here has to guess it.
func federateListBeads(legs []readyLeg, q beads.ListQuery) ([]beads.Bead, error) {
	return federateBeadLegs(legs, func(store beads.Store) ([]beads.Bead, error) {
		return store.List(q)
	})
}

// federateListBeadsWithOwner is federateListBeads plus a record of which leg
// served each merged row.
//
// It is separate rather than a widened federateListBeads because only the
// crash-recovery arm needs the ownership map, and every other caller would then
// carry a map it discards. The merge rule is the same one federateBeadLegs
// applies — first leg to return an id wins — restated here rather than shared,
// because sharing it would mean threading a per-leg callback through the read
// closure that federateBeadLegs deliberately keeps store-shaped.
func federateListBeadsWithOwner(legs []readyLeg, q beads.ListQuery) ([]beads.Bead, map[string]readyLeg, error) {
	var merged []beads.Bead
	owner := make(map[string]readyLeg)
	for _, leg := range legs {
		rows, err := leg.store.List(q)
		if err != nil {
			return nil, nil, fmt.Errorf("%s store: %w", leg.label, err)
		}
		for _, b := range rows {
			if _, seen := owner[b.ID]; seen {
				continue
			}
			owner[b.ID] = leg
			merged = append(merged, b)
		}
	}
	return merged, owner, nil
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
