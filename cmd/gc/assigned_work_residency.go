package main

// The assigned-work spine's residency, resolved through internal/storeref.
//
// One question — "which stores can hold work assigned to this identity?" — asked
// once, for the wake filter, the drain and close gates, the retired-session
// sweeps, the drain-ack release and the orphan-release ownership index. Before
// this, each of them built its own list from workAssignmentStores plus, at the
// split-aware minority, a per-site re-derivation of the class leg. Two of those
// re-derivations were the incidents this slice is named for.
//
// # The leg set is SCOPE plus RESIDENCY, and only residency is the resolver's
//
// Which WORK legs an identity can reach is its agent's scope: a rig-scoped agent
// claims in its rig, a city-scoped one federates across every store (vp-kvp).
// That is a config fact, and the resolver never re-invents it — the scope
// decides which work legs the topology is built WITH, exactly the "it is told,
// it does not decide" contract the topology constructors carry. What the
// resolver owns is the part every site was getting wrong: every relocated class
// binding is a leg, LAST, on every one of these scans.
//
// # What un-shape-gating changed
//
// The pre-S2 class leg was `classBindingLegForSessionScan(cfg, store)`: gated on
// cfg's [storage] shape, and returning the store the caller already held. Both
// halves were wrong in a way that showed up in production.
//
//   - Reading CFG rather than the opened ROUTES means a city whose [storage]
//     section was DELETED after it had served a split answers "no split", so the
//     scan silently reads the work ledger its infrastructure beads were moved
//     off. Through the routes the same city fails LOUD with the refusal that
//     names the remedy.
//   - Returning the CALLER'S store makes the leg a no-op for every scan that
//     leads with the work store — `gc session close`, drain-ack, the
//     stranded-repair sweep. Those are the RELEASE paths, so a claim claim-time
//     routing wrote into the binding had no automatic reopen lane at all and was
//     hand-released by an operator (ga-j4ob9).
//
// # Widening the release path's view may never widen what it releases
//
// A release-side false positive is claim LOSS, not a missed wake. So the legs
// this file adds to the release scans are matched by two counterweights that
// stay exactly as they were: the ownership index now grants a holder the same
// leg set (so a LIVE holder's binding-resident claim is owned, not orphaned),
// and releaseOrphanedPoolAssignments keeps the #5242 owner-store liveness probe
// after the primary one. TestOrphanReleaseSparesALiveHoldersBindingResidentClaim
// is the control, and it fails as a lost claim rather than as a strand.

import (
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// assignedWorkClaimRefs returns the store-refs a claim held by ONE session can
// be recorded under, whatever the holder's rig scope: the leading work arm and
// every relocated class binding.
//
// This replaces the classBindingAssignedWorkStoreRef constant. The constant was
// the empty string because on a converged split the reconciler's LEADING arm is
// the binding, so the census records a binding-resident claim under the city
// ref — true today and an accident of the E2 dual role rather than a fact about
// residency. Taking the refs from the topology keeps that answer where it is
// still true (a leading binding collapses onto the work ref) and adds the
// binding's own ref where it is not, which is what lets one filter read a city
// mid-rollout: old census rows under "" and new ones under "class:*".
//
// It cannot fail. A refused city still records claims under its refusing
// binding's ref, and answering "no refs" there would drop every claim-holder on
// that city into the no-wake-reason drain — the failure the refusal is supposed
// to prevent, delivered by the guard against it.
//
// The work leg comes from censusWorkLeg, NOT from the caller's leading store,
// and that is load-bearing: this set is matched against refs the CENSUS emitted,
// and the census resolves its work leg the same way. Taking the caller's leading
// store here would, on the reconciler's plane, make the work leg and the binding
// the same store — collapsing their two refs into one — while the census (which
// has the runtime's real work store) emitted both. Every binding-resident claim
// would then be collected and rejected.
func assignedWorkClaimRefs(cityPath string, cfg *config.City, leading beads.Store) []string {
	refs := residencyTopologyForCity(cityPath, cfg, censusWorkLeg(cityPath, leading), nil).ClaimRefs()
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, string(ref))
	}
	return out
}

// assignedWorkRelocatedClaimRefs is assignedWorkClaimRefs for the callers that
// may only widen where a class was actually RELOCATED: it answers empty for a
// single-store city.
//
// The wake filter can take the unconditional set because it matches a session's
// own exact assignee identity, so a wider ref set still admits only that
// session's own claims. Pool demand matches a routed TEMPLATE, so the same set
// would make city-store work resume a rig-scoped pool session on a city that
// relocates nothing — the reachability rule
// TestBuildDesiredState_RigPoolIgnoresAssignedWorkInUnreachableStore pins, and
// the reason this is a second function rather than a second caller of the first.
//
// On a city that DOES relocate, the collapse inside ClaimRefs is what makes the
// answer right on both planes: a leading arm that is itself the binding reports
// the single ref its census records, and a distinct binding reports its own.
func assignedWorkRelocatedClaimRefs(cityPath string, cfg *config.City, leading beads.Store) []string {
	topology := residencyTopologyForCity(cityPath, cfg, censusWorkLeg(cityPath, leading), nil)
	if topology.IsSingleStore() {
		return nil
	}
	refs := topology.ClaimRefs()
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, string(ref))
	}
	return out
}
