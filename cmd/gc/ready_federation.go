package main

// The leg assembly behind `gc ready`. The reader over the legs is
// internal/readywork, shared with the supervisor's GET
// /v0/city/{cityName}/work/ready.
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
//     relocated graph store LAST. (Here the rigs are the SERVING rigs; see the
//     serving-frame divergence below.)
//   - Within a leg: whatever order that leg's own Ready reader emits. That is
//     canonical (priority, created_at, id) for a caching-wrapped work store and
//     for the canonical relocated binding (beads.SQLiteStore) alike.
//   - Dedupe: the FIRST leg to return an id wins. The graph leg is merged last,
//     so a bead co-resident in the work store and the binding — the documented
//     steady state of a migrated city, where `gc storage migrate` preserves ids
//     and never deletes back — resolves to the work store's row on both
//     surfaces.
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
// at BOTH ends. readywork.Read is the read end. readyRigLegStores is the open
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
// # Where this deliberately diverges: the serving frame
//
// The rig legs are the SERVING rigs only — the frame the controller's census
// reads (servingRigStores) — so a suspended rig is neither opened nor read. The
// API still federates every rig its controller holds open, suspended ones
// included (ga-h07n0z). Here the census side of reader agreement wins: a row in
// a suspended rig is one no demand counts and no agent of that rig may claim,
// so serving it made the claim reader and the census disagree (ga-028k7), and
// opening the rig woke the Dolt the controller had retired for it, on every
// claim. See readyServingRigs for why nothing routed is stranded by it.
//
// # Legs are opened and read concurrently, and merged by position
//
// The serving rigs are opened at most readyLegFanOut at a time, then the legs
// read at most readywork.LegFanOut at a time. Neither changes
// an answer: every result is kept at its leg's index and merged in leg order,
// so first-leg-wins dedupe, the merged order, and which failing leg an error
// names are exactly what a leg-after-leg reader produced. What changes is the
// cost: a federated read waits for its slowest leg instead of the sum of them.
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
// The leg set is Plan(RoutedWork) over the city's topology, and a city that
// relocates nothing has no binding in that topology — so it gets no graph leg at
// all and its answer is exactly the one leg it always had. The gate is STORE
// IDENTITY, the same rule the API's relocatedGraphStore and cmd/gc's
// resolveClassStore use: a binding that resolved back to the work store is
// deduped by the plan rather than federated twice.

import (
	"errors"
	"fmt"
	"path/filepath"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/gastownhall/gascity/internal/storeref"
)

// readyFederationLegs assembles the ordered leg list from Plan(RoutedWork): the
// city store, the rig stores by name ascending, then the relocated binding.
//
// The order is not restated here — it IS the plan's, which is the same plan the
// controller's demand census resolves over one topology. That is what makes
// "the reader the hook claims through and the reader the controller counts
// demand with agree about which stores exist" true by construction rather than
// by two lists kept in step (the leg-set half of reader agreement; the row half
// is demand_serve_agreement_test.go).
//
// A rig whose name equals the city name is skipped, mirroring the API exactly:
// State.BeadStores() keys the city store under CityName(), so the API's rig loop
// skips that key and the collision resolves to one leg there. Skipping it here
// keeps the two leg sequences identical by construction rather than by
// coincidence.
//
// A nil store is dropped by the plan rather than federated: a nil entry would
// panic on first read, and by the time the legs are assembled a store that could
// not be opened has already failed the whole query at readyRigLegStores.
func readyFederationLegs(cityPath, cityName string, cfg *config.City, cityStore beads.Store, rigStores map[string]beads.Store) ([]readywork.Leg, error) {
	return readyLegsForTopology(cliResidencyTopology(cityPath, cfg, cityStore, rigLegsExcludingCityName(cityName, rigStores)))
}

// readyFederationLegsOverBinding assembles the legs for an EXPLICITLY supplied
// binding rather than one resolved from the city's routes.
//
// It is the seam relocatedGraphLegFrom was — "a caller that resolved the binding
// from routes it already holds applies the SAME rule instead of restating it" —
// carried forward now that the leg list comes from a plan. The identity gate is
// unchanged: a binding that resolved back to the city's own work store is the
// store already federated as the city leg, so there is no second store.
func readyFederationLegsOverBinding(cityName string, cityStore beads.Store, rigStores map[string]beads.Store, binding beads.Store) ([]readywork.Leg, error) {
	var bindings []storeref.ClassBinding
	if leg := relocatedGraphLegFrom(binding, binding != nil, cityStore); leg != nil {
		classes := infrastructureClasses()
		bindings = []storeref.ClassBinding{{
			Classes:  classes,
			Prefixes: storeref.ReservedPrefixesFor(classes),
			Leg:      storeref.Leg{Ref: storeref.ClassRef(classes), Store: leg},
		}}
	}
	return readyLegsForTopology(assembleResidencyTopology(nil, cityStore, rigLegsExcludingCityName(cityName, rigStores), bindings, nil))
}

// readyLegsForTopology is the assembly itself: Plan(RoutedWork), enumerated
// through the resolver and labeled.
//
// The leg set is resolved ONCE and read by every arm of the reader (ready and
// the --status lists), which is why this enumerates rather than executing: the
// plan's policy travels with each leg to the reader, which escalates it (see
// readywork.Leg). A refused city produces no legs at all — Plan refuses first.
//
// A class binding is marked as one, with the reserved namespaces it holds: it
// is the one leg whose work-shaped ids an earlier leg can also hold (migration
// relics), which the reader's narrowed reads must know (readywork.Leg).
func readyLegsForTopology(topo storeref.Topology) ([]readywork.Leg, error) {
	plan, err := storeref.Plan(storeref.RoutedWork{}, topo)
	if err != nil {
		return nil, err
	}
	var legs []readywork.Leg
	storeref.EachLeg(plan, func(leg storeref.Leg, _ storeref.Role, onError storeref.ErrPolicy) {
		classBinding := storeref.IsClassRef(string(leg.Ref))
		legs = append(legs, readywork.Leg{
			Label:            readyLegLabel(leg.Ref),
			Store:            leg.Store,
			OnError:          onError,
			ClassBinding:     classBinding,
			ReservedPrefixes: readyLegReservedPrefixes(topo, leg, classBinding),
		})
	})
	return legs, nil
}

// readyLegReservedPrefixes is the reserved id namespaces a class-binding leg
// holds: the prefixes of every binding the topology serves from that store. A
// work leg holds none. It does not yet leave out a prefix that a work leg's
// prefix shadows (readywork's Co-resident ids; ga-xdtyo2).
func readyLegReservedPrefixes(topo storeref.Topology, leg storeref.Leg, classBinding bool) []string {
	if !classBinding {
		return nil
	}
	var prefixes []string
	for _, binding := range topo.Bindings {
		if binding.Leg.Store == leg.Store { // residency:allow — matches the plan's chosen leg to the binding the topology resolved for it, to read its namespaces; no store is ordered, opened or read
			prefixes = append(prefixes, binding.Prefixes...)
		}
	}
	return prefixes
}

// rigLegsExcludingCityName drops a rig keyed under the city's own name; see
// readyFederationLegs.
//
// residency:allow — a constructor INPUT, not a residency answer. It drops a rig
// keyed under the city's own name so the CLI and the API build the same
// topology; it consults no binding, no namespace and no leg order, and its
// result goes nowhere but a topology constructor.
func rigLegsExcludingCityName(cityName string, rigStores map[string]beads.Store) map[string]beads.Store {
	if _, collides := rigStores[cityName]; !collides {
		return rigStores
	}
	out := make(map[string]beads.Store, len(rigStores)-1)
	for name, store := range rigStores {
		if name == cityName {
			continue
		}
		out[name] = store
	}
	return out
}

// readyLegLabel is the name a failing leg reports itself by. The three spellings
// are the ones this command's diagnostics have always used, kept because they
// are what an operator greps for and what the API's partial_errors say.
func readyLegLabel(ref storeref.StoreRef) string {
	switch {
	case ref == storeref.WorkRef:
		return "city"
	case storeref.IsClassRef(string(ref)):
		// Every binding this build serves carries the graph class (storage boot
		// admits the whole split or nothing), and "graph" is the name the
		// federation contract and the API's graphPlaneUnavailable already use.
		return "graph"
	default:
		rig, _ := storeref.ScopeRigContext(string(ref))
		return "rig " + rig
	}
}

// readyLegFanOut bounds how many rig stores `gc ready` opens at once. The legs
// are then read at most readywork.LegFanOut at once.
//
// Both phases are latency-bound, not CPU-bound: a rig open is a `bd context`
// subprocess plus a round of schema probes against its server, and a leg read
// is one query. A walk that does them one after another makes every work-query
// tier pay their SUM; on maintainer-city that was six rig opens at ~1.75s each
// before the first read, on every one of the five-to-eight `gc ready` calls a
// fresh seat's claim makes. The bound keeps a many-rig city from opening a
// connection storm against one Dolt server: legs past it wait for a slot.
const readyLegFanOut = 8

// readyRigStoreOpener builds the opener `gc ready` opens each serving rig's
// store with: the one-shot opener over the config this invocation loaded. A
// variable so tests can see which rig stores a read opens, and how often.
var readyRigStoreOpener = oneShotRigStoreOpener

// readyRigLegStores opens the rig legs of the serving frame (readyServingRigs),
// at most readyLegFanOut at once, failing the whole query when any of those
// rigs' stores cannot be opened.
//
// This is the OPEN end of the fail-loud rule federateBeadLegs applies at the read
// end, and the two are the same rule for the same reason: a leg missing from the
// federation is a leg whose claimable rows are missing from the array, and the
// array is the entire answer. The controller's opener,
// buildStandaloneRigStores, warns and continues instead — correctly, for a
// process whose job is to keep supervising the rigs that are up. Wiring `gc
// ready` to that policy is what let a dead rig produce exit 0 and a short array.
//
// Every dead rig is named, in config order, not just the first: unlike a read,
// the opens have all already happened by here, so reporting one and discarding
// the rest would throw away diagnosis already paid for. The stores that did
// open are closed, since a failed query hands back none of them.
//
// A suspended rig is not in the frame, so it is never opened and its store
// being down cannot fail the query; see readyServingRigs.
func readyRigLegStores(cfg *config.City, cityPath string) (map[string]beads.Store, error) {
	rigs, err := readyServingRigs(cfg, cityPath)
	if err != nil {
		return nil, err
	}
	stores, failures := openStandaloneRigStores(rigs, cityPath, readyRigStoreOpener(cfg), readyLegFanOut)
	if len(failures) == 0 {
		return stores, nil
	}
	for _, store := range stores {
		closeBeadStoreHandle(store) //nolint:errcheck // best-effort: the failed opens are the error
	}
	errs := make([]error, 0, len(failures))
	for _, f := range failures {
		errs = append(errs, fmt.Errorf("rig %q store: %w", f.rig, f.err))
	}
	return nil, errors.Join(errs...)
}

// readyServingRigs is the rig half of the frame `gc ready` federates: the
// configured rigs that are not suspended, in config order. Over a readable
// suspension state it is the frame the controller's census reads
// (servingRigStores), by the same rule — suspendedRigPathsWithState over the
// effective suspension state, the runtime `gc rig suspend` / `gc rig resume`
// override layered on suspended_on_start — so the reader a seat claims through
// and the census that minted the seat agree about which rigs exist
// (TestReadyServingFrameIsTheCensusFrame). Over an unreadable one they part:
// the census's loader (buildSuspendedRigPathsForCity) drops the error and
// falls back to the authored defaults, where this fails the query (below).
//
// # Why a suspended rig is outside the frame
//
// Suspension is quiescence (beads_quiescence.go): a suspended rig's agents are
// not started and `gc hook` refuses them, the controller counts no demand in
// its store and stops the Dolt it owns, and every periodic reader — rig
// caches, demand, order tracking, the convoy check, /status — skips it. So no
// seat is ever minted for a row in it, and the rows a running seat would see
// there are routed to agents that cannot run. Reading it anyway made every
// claim and every scale_check probe pay for its open, and woke a retired
// scope's proxy and Dolt on each of them.
//
// Nothing routed is stranded by the skip. The city store and every relocated
// binding are not rigs and are always read, so routed graph work — every
// rig's steps, a suspended rig's included — stays visible. What a suspended
// rig's own store holds is paused with it and comes back on `gc rig resume`
// (TestReadyServesNoWorkFromASuspendedRigUntilItResumes): the same work the
// census already leaves alone, which the reader now leaves alone too.
//
// # An unreadable suspension state fails the query
//
// Guessing the frame is wrong in both directions: reading every rig wakes the
// suspended ones, and falling back to the authored suspended_on_start defaults
// drops a rig resumed at runtime from an array that has nowhere to say it is
// short. `gc hook` refuses on the same file for the same reason.
func readyServingRigs(cfg *config.City, cityPath string) ([]config.Rig, error) {
	if cfg == nil || len(cfg.Rigs) == 0 {
		return nil, nil
	}
	st, err := loadSuspensionState(fsys.OSFS{}, cityPath)
	if err != nil {
		return nil, fmt.Errorf("reading the rig suspension state: %w", err)
	}
	suspended := suspendedRigPathsWithState(cfg, st)
	serving := make([]config.Rig, 0, len(cfg.Rigs))
	for _, rig := range cfg.Rigs {
		if suspended[filepath.Clean(rig.Path)] {
			continue
		}
		serving = append(serving, rig)
	}
	return serving, nil
}

// relocatedGraphLegFrom is the identity gate over an already-resolved binding:
// a binding that resolved back to the city's own work store is the same store
// already federated as the city leg, so there is no second store to add.
//
// The `==` is safe for the same reason residencyBindingsFromRoutes's map key is:
// both operands are stores a constructor opened, and every one of those is
// pointer-typed. An interface == on a value-typed store with an uncomparable
// dynamic type panics — see storeref.storeSet, which is where that stopped being
// hypothetical.
//
// Production no longer calls it — readyFederationLegs resolves the binding from
// the city's routes and the plan's own dedupe applies the gate — but the
// EXPLICIT-binding seam (readyFederationLegsOverBinding) still needs it, so a
// caller that resolved the binding itself applies the same rule rather than
// restating it.
func relocatedGraphLegFrom(binding beads.Store, relocated bool, cityStore beads.Store) beads.Store {
	if !relocated || binding == nil || binding == cityStore {
		return nil
	}
	return binding
}
