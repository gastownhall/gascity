package main

// The reconciler frame's census legs, resolved through internal/storeref.
//
// One question — "which stores does a controller-tick sweep read?" — asked once,
// for the session-iteration arm, the assigned-work arm and the unassigned-routed
// arm. Before this they shared coordClassStoreCandidates, which built the list
// from cfg alone and could name only two families: the store it was handed, and
// the configured rigs. It had no way to name a relocated class binding, so the
// binding reached the census only by being the store the caller HANDED it.
//
// # The E2 dual role, and why naming both legs is the whole slice
//
// buildDesiredState hands the census its SESSIONS-class store, which on a
// converged split IS the binding. So the census covered the binding by accident,
// as its "city" leg — and the city WORK store, which rigBeadStores() explicitly
// deletes, was then in no leg at all. A city-scope routed WORK bead was
// invisible to controller-tick demand: the pool never spawned for it, and a dead
// claim on one was captured and released by nothing (D6, ga-88mxz;
// split_topology_conformance_test.go pre-declared the flip rows for both halves).
//
// Plan(Census) names work AND the rigs AND every binding as DISTINCT ref'd legs,
// which is exactly the shape that was missing. The work leg comes from the
// runtime's own registration rather than from the store the arm was handed,
// because the store it was handed is the one that used to stand in for it.
//
// # The suspension frame is threaded, not re-derived
//
// A suspended rig is routinely dark, and Plan marks a dark FederationTail leg's
// failure PartialDegrade — which means retain-don't-reap. Left in, one suspended
// rig would mark every census Partial and pin the fleet against every drain and
// reap decision. The serving set is therefore computed by the caller that
// already knows it and handed in: told, not deciding.

import (
	"path/filepath"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

// censusRefStyle names the store-ref vocabulary one census arm labels its rows
// with. The two exist because the arms record refs for different readers: the
// assigned-work arm's refs are matched against assignedWorkClaimRefs by the wake
// filter, which has always spelled the city store as the empty string; the
// unassigned-routed arm's are matched against gc.root_store_ref, which is
// canonically scoped ("city:<name>" / "rig:<name>").
type censusRefStyle int

const (
	// censusRefBare is the assigned-work arm's vocabulary: the city work store
	// under the EMPTY ref (the ref every pre-split census row already carries),
	// rigs under their bare name.
	censusRefBare censusRefStyle = iota

	// censusRefScoped is the session and unassigned-routed arms' vocabulary:
	// "city:<name>" and "rig:<name>", the canonical gc.root_store_ref spelling.
	censusRefScoped
)

// censusStoreCandidates resolves the WORK census leg set: the city work store,
// the serving rig stores, then every relocated class binding, each with the
// store-ref this arm labels its rows with.
//
// leading is the store the arm was handed — the sessions-class store on the
// controller path. It is used as the work leg only when this process has not
// registered one, which is every one-shot and test caller; a controller's own
// work store comes from the registration, so the binding it hands in as its
// leading store lands on the binding leg where it belongs instead of standing in
// for work.
//
// It errors only on a REFUSED city, where every infrastructure class answers
// with the refusal that names the remedy. A refused city must not get a
// work-only census: that is the answer that looks like success while reading the
// ledger the relocated classes were moved off.
func censusStoreCandidates(
	cityPath string,
	cfg *config.City,
	leading beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	style censusRefStyle,
) ([]classStoreCandidate, error) {
	return censusLegCandidates(storeref.Census{}, storeref.PlaneReconcile, cityPath, cfg, leading, rigStores, suspendedRigPaths, style)
}

// routedWorkStoreCandidates resolves the ROUTED-WORK leg set for a
// runtime-plane reader: Plan(RoutedWork) narrowed to the infra bindings.
//
// Two things make it a different question from censusStoreCandidates rather
// than a cheaper version of it.
//
// The INTENT is RoutedWork, which is the surface `gc ready` and the hook claim
// through. Sharing it is the reader-agreement property (internal/storeref's
// TestSweepAndDemandReadTheSameStores): a bead the controller counts as demand
// must be a bead a worker can claim, and the fastest way to break that is for
// one of them to resolve its own leg list.
//
// The PLANE is runtime, and it narrows to the bindings because the operator
// ruling is that routed work lives only in the graph store — "gc ready work will
// never be in the work db" (ga-4qdfn) — so the ledger leg this arm used to read
// SEQUENTIALLY, once per census leg, could not hold the answer. It was 8.1s of a
// 24.2s demand leg on maintainer-city.
//
// The narrowing is the resolver's, not this file's: storeref.Narrow keeps the
// plan's order, roles and per-leg error policies and only drops legs, so a
// consumer cannot accidentally answer the latency question by reordering the
// plan (#5148 co-residence — binding-LAST is deliberate).
//
// A city that relocates nothing narrows to its one work store, which there IS
// its infra store.
func routedWorkStoreCandidates(
	cityPath string,
	cfg *config.City,
	leading beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	style censusRefStyle,
) ([]classStoreCandidate, error) {
	return censusLegCandidates(storeref.RoutedWork{}, storeref.PlaneRuntime, cityPath, cfg, leading, rigStores, suspendedRigPaths, style)
}

// sessionCensusStoreCandidates resolves the SESSION census leg set. It differs
// from the work census in one way that matters: the sessions binding LEADS.
//
// The binding is the authority for the session class, so on a co-resident
// session bead — the steady state of a migrated city, where `gc storage migrate`
// preserves ids and never deletes back — the first-leg-wins fold must resolve to
// the binding's row, not to the pre-relocation residue in the work ledger. That
// is also the leg this arm has always read first, so a converged split's
// leading answer is unchanged and what it gains is the work and rig legs behind
// it (#5187's session-verb blindness: graph-run session beads in work stores are
// real).
func sessionCensusStoreCandidates(
	cityPath string,
	cfg *config.City,
	leading beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
) ([]classStoreCandidate, error) {
	return censusLegCandidates(storeref.Session{}, storeref.PlaneReconcile, cityPath, cfg, leading, rigStores, suspendedRigPaths, censusRefScoped)
}

func censusLegCandidates(
	intent storeref.Intent,
	plane storeref.Plane,
	cityPath string,
	cfg *config.City,
	leading beads.Store,
	rigStores map[string]beads.Store,
	suspendedRigPaths map[string]bool,
	style censusRefStyle,
) ([]classStoreCandidate, error) {
	work := censusWorkLeg(cityPath, leading)
	if work == nil {
		// Not a topology the resolver can plan over, and not an error either:
		// the pre-resolver builder returned one nil candidate that every arm
		// then skipped, so returning no legs is the same answer without the nil.
		return nil, nil
	}
	topo := residencyTopologyForCity(cityPath, cfg, work, servingRigStores(cfg, rigStores, suspendedRigPaths))
	plan, err := storeref.Plan(intent, topo)
	if err != nil {
		return nil, err
	}
	// The plane is the CALLER's contract, applied after the residency question is
	// answered: the census sweep converges and never narrows, the tick's
	// routed-demand read is a latency contract and narrows to the infra binding.
	if plan, err = storeref.Narrow(plan, plane); err != nil {
		return nil, err
	}
	// Enumerated through the resolver rather than executed through it: the arms
	// read every leg CONCURRENTLY and fold their own per-arm partials, which is
	// a fan-out shape Union cannot express. What they must not supply is the leg
	// order or the error policy, and EachLeg hands over both.
	var candidates []classStoreCandidate
	storeref.EachLeg(plan, func(leg storeref.Leg, _ storeref.Role, onError storeref.ErrPolicy) {
		candidates = append(candidates, classStoreCandidate{
			store:   leg.Store,
			ref:     censusRef(cfg, leg.Ref, style),
			onError: onError,
		})
	})
	return candidates, nil
}

// censusWorkLeg picks the store the CENSUS plans over as its work leg: the one
// the runtime serving this city registered, and otherwise the store the caller
// was handed.
//
// It is a named function with one caller too many on purpose. The refs the
// census EMITS and the refs the wake filter ACCEPTS
// (assignedWorkClaimRefs → Topology.ClaimRefs) have to be derived from the same
// work leg, and they are resolved on different call paths with different
// arguments. Resolve them differently and the two vocabularies diverge by one
// entry: the census labels a binding-resident claim "class:*" while the filter's
// accepted set collapsed the binding onto the work ref, so the claim is
// collected and then dropped and its holder drains with no wake reason —
// ga-whzrt, arriving through the fix for ga-whzrt.
// TestSessionWakeFilterKeepsABindingRefClaimOnTheReconcilerPlane is the guard.
func censusWorkLeg(cityPath string, leading beads.Store) beads.Store {
	if registered := registeredCityWorkStore(cityPath); registered != nil {
		return registered
	}
	return leading
}

// censusRef spells one plan leg in the vocabulary of one census arm.
//
// A binding keeps its own "class:*" ref in BOTH vocabularies. It is a city-scope
// store, so every scope comparison folds it onto the city
// (storeref.ScopeRigContext, and the demand/idle/continuation normalizers that
// read it back) — but it is a DISTINCT store, and giving it the city's own ref
// would recreate the substitution this slice exists to end: two legs, one name,
// and no way for a wake filter or a partial-result diagnostic to say which one
// answered.
func censusRef(cfg *config.City, ref storeref.StoreRef, style censusRefStyle) string {
	if storeref.IsClassRef(string(ref)) {
		return string(ref)
	}
	if rig, ok := storeref.ScopeRigContext(string(ref)); ok && rig != "" {
		if style == censusRefBare {
			return rig
		}
		return string(ref)
	}
	if style == censusRefBare {
		return ""
	}
	return "city:" + censusCityName(cfg)
}

// censusCityName is the workspace name the scoped vocabulary spells the city
// leg with, defaulting to "city" exactly as the pre-resolver arm did.
func censusCityName(cfg *config.City) string {
	if cfg == nil {
		return "city"
	}
	if name := strings.TrimSpace(cfg.Workspace.Name); name != "" {
		return name
	}
	return "city"
}

// servingRigStores is the suspension FRAME: the rig legs a census may read.
//
// It keeps the pre-resolver rule exactly — a rig is a leg when it is DECLARED in
// cfg.Rigs, its store is open, and its path is not suspended — so an entry in
// the store map that config no longer declares stays out. What it does not keep
// is cfg.Rigs ORDER: the resolver sorts rig legs by name, which is the order
// `gc ready` and the API's fan-out already agree on, and having the census
// disagree with them was one leg-ordering fact maintained in three places.
//
// residency:allow — a constructor INPUT, not a residency answer. It hands the
// topology the frame it is TOLD (which rigs are serving) by filtering the
// caller's own map; it consults no binding, no namespace and no leg order, and
// its result goes nowhere but residencyTopologyForCity.
func servingRigStores(cfg *config.City, rigStores map[string]beads.Store, suspendedRigPaths map[string]bool) map[string]beads.Store {
	if cfg == nil || len(rigStores) == 0 {
		return nil
	}
	serving := make(map[string]beads.Store, len(rigStores))
	for _, rig := range cfg.Rigs {
		if suspendedRigPaths[filepath.Clean(rig.Path)] {
			continue
		}
		if store, ok := rigStores[rig.Name]; ok && store != nil {
			serving[rig.Name] = store
		}
	}
	if len(serving) == 0 {
		return nil
	}
	return serving
}
