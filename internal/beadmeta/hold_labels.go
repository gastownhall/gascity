package beadmeta

import "strings"

// HoldMayorLabel and HoldExternalLabel are the two canonical hold:<value>
// bd label values (engdocs/contributors/hold-label-conventions.md,
// ga-tug8ry.1): "the required next actor is the mayor" and "the required
// next actor or condition is outside this bd instance's control",
// respectively. They are bd label *values* (data a bead carries in its
// Labels []string), not role names — a role-neutral dispatcher checks for
// their presence without knowing or caring who "mayor" is (ga-5736js).
const (
	HoldMayorLabel    = "hold:mayor"
	HoldExternalLabel = "hold:external"
)

// DispatchHoldLabels is the complete set of hold label values naming a bead
// whose required next actor or condition is, by construction, not the worker
// looking at it. It is the canonical hold taxonomy
// (engdocs/contributors/hold-label-conventions.md) and is fixed at the two
// values; the paths that decide what to DO read ParkedLabels, which adds bd's
// human label.
//
// Waking a session for assigned OPEN work is a "what to DO" decision: the only
// thing the woken session can do with the row is ask its hook for it, and the
// hook refuses held rows. So the controller's assigned-work WAKE readiness, its
// keep-awake probe and the drain-ack claimability classifier exclude open held
// work (HasDispatchHold), while the assignment itself stays visible to orphan
// release, pool accounting and the in_progress ownership tiers above.
var DispatchHoldLabels = []string{HoldMayorLabel, HoldExternalLabel}

// HumanLabel is bd's native label for a bead waiting on a person's decision
// (`bd human list` enumerates the beads that carry it). It is not a dispatch
// hold and is absent from DispatchHoldLabels: it is not a third hold value, bd
// sets it (not `bd set-state`), and the hold doctor checks do not look at it.
// It is parked all the same: ParkedLabels names it beside the holds, so every
// path that decides what to DO with a bead refuses it for the reason it refuses
// a hold. The dead-assignee sweep (releaseOrphanedPoolAssignments) skips it
// too: released, it would be served to no one, and its holder may be a person,
// whom no session answers to.
const HumanLabel = "human"

// ParkedLabels is the one list of labels that park a bead: the dispatch holds
// followed by HumanLabel. A parked bead is waiting on a required next actor or
// condition that is, by construction, not the worker looking at it. Two
// different questions consume it, and they answer oppositely; conflating them
// is what produced both ga-5736js and gas-kg6:
//
//   - "Is this bead WORK for whoever is asking?" Always no. Route-scoped,
//     unassigned automatic dispatch (Tier 3 pool-demand queries and the control
//     dispatcher's routed/run-target tiers) excludes these (ga-5736js), and so
//     does every path that serves a bead to an agent as work: the in_progress
//     crash-recovery tier and its wisp probes, the `gc hook` seam that every
//     hook path runs through (the claim, cross-store federation and plain
//     `gc hook`). A parked bead handed back as work cannot be advanced, is
//     never released, and so is re-served forever.
//     The assigned-ready tier (assignedReadyTierCommand and its bd-1.0.4 wisp
//     probe, #8010) drops the dispatch holds only: it stays transparent to
//     human, and its Go-side strip is the seam above.
//
//   - "Does a session still need to EXIST for this bead?" Parking is
//     irrelevant; the assignment is a real ownership fact either way. The
//     demand/liveness tier that answers this (filterReadyByAssignee) never
//     filters on this list (ga-5736js), or a parked bead's owner would go
//     invisible to the pool and to crash recovery.
//
// The short rule: filter on parked labels when deciding what to DO, never when
// deciding who EXISTS.
//
// It is built from DispatchHoldLabels without aliasing it, so appending the
// human label cannot reach the canonical hold list.
var ParkedLabels = append(append([]string(nil), DispatchHoldLabels...), HumanLabel)

// HasParkedLabel reports whether labels carry one of ParkedLabels: a dispatch
// hold (HasDispatchHold: trimmed, case-insensitive) or the human label
// (HasHumanLabel: trimmed, exact). It is the label comparison the hook's serve
// filter answers with.
func HasParkedLabel(labels []string) bool {
	return HasDispatchHold(labels) || HasHumanLabel(labels)
}

// HasHumanLabel reports whether labels carry HumanLabel. It compares the way
// the route-scoped serve filter does (bd's --exclude-label, mirrored by the
// controller's demand predicate): trimmed and exact, so a "Human" bead is
// served as ordinary work and is not a person's bead here either.
func HasHumanLabel(labels []string) bool {
	for _, label := range labels {
		if strings.TrimSpace(label) == HumanLabel {
			return true
		}
	}
	return false
}

// HasDispatchHold reports whether labels carry one of DispatchHoldLabels. It is
// the label comparison the hold-aware WORK-SERVING decisions answer with: the
// hook's serve filter (isHeldHookCandidate, through HasParkedLabel),
// continuation-group pre-assignment (preassignHookContinuationGroup), and three
// controller gates over OPEN rows —
// wake readiness and the keep-awake probe (both through assignedOpenWorkHeld)
// and the drain-ack claimability classifier (classifyDemandRowClaimability).
// So held OPEN assigned work is never wake readiness, keep-awake work or a
// drain-ack strand, which ended the wake/drain loop where an on-demand named
// session was re-woken every tick for assigned hold:external work its hook
// drained past as no_work. The controller's route-restore lanes consult it too
// (isDetachedHandoffOrphanCandidate, carriedPoolRoute): a held bead's empty
// gc.routed_to is a parking decision, not a lost route, so it is not re-stamped.
//
// Two demand paths do not consult it. Pool accounting
// (filterAssignedWorkBeadsForPoolDemandAt) still counts a held OPEN assigned
// row toward its template's desired seats, and held in_progress work still
// keeps its owner's session in demand and awake, a claimed ownership fact
// (ga-5736js). The route-scoped filters that mirror `bd ready --exclude-label`
// (filterReadyByRoute, the work query's jq hold clause) compare the values
// exactly rather than through this function.
//
// The comparison is trimmed and case-insensitive because the hook's filter is
// the last word on what a session is served, and it compares that way: a
// "Hold:External" row is stripped by the hook, so it is not work either.
func HasDispatchHold(labels []string) bool {
	for _, label := range labels {
		label = strings.TrimSpace(label)
		for _, hold := range DispatchHoldLabels {
			if strings.EqualFold(label, hold) {
				return true
			}
		}
	}
	return false
}
