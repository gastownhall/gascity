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
// looking at it. Two different questions consume this list, and they answer
// oppositely — conflating them is what produced both ga-5736js and gas-kg6:
//
//   - "Is this bead WORK for whoever is asking?" — always no. Route-scoped,
//     unassigned automatic dispatch (Tier 3 pool-demand queries and the
//     control dispatcher's routed/run-target tiers) must exclude these
//     (ga-5736js), and so must every path that serves a bead to an agent as
//     work — including the assignee-scoped crash-recovery tier, the
//     `gc hook --claim` result (gas-kg6) and the continuation-group siblings
//     a claim pre-assigns (preassignHookContinuationGroup, #6026). A held bead
//     handed back as work cannot be advanced, is never released, and so is
//     re-served forever.
//
//   - "Does a session still need to EXIST for this bead?" — hold is
//     irrelevant; the assignment is a real ownership fact either way. The
//     demand/liveness tiers that answer this (filterReadyByAssignee,
//     ephemeralAssignedReadyProbeScript) stay hold-transparent by design and
//     must never filter on this list (ga-5736js), or a parked bead's owner
//     would go invisible to the pool and to crash recovery.
//
// The short rule: filter on holds when deciding what to DO, never when
// deciding who EXISTS.
//
// Waking a session for assigned OPEN work is a "what to DO" decision: the only
// thing the woken session can do with the row is ask its hook for it, and the
// hook refuses held rows. So the controller's assigned-work WAKE readiness, its
// keep-awake probe and the drain-ack claimability classifier exclude open held
// work (HasDispatchHold), while the assignment itself stays visible to orphan
// release, pool accounting and the in_progress ownership tiers above.
var DispatchHoldLabels = []string{HoldMayorLabel, HoldExternalLabel}

// HumanLabel is bd's native label for a bead waiting on a person's decision
// (`bd human list` enumerates the beads that carry it). It is deliberately NOT a
// dispatch hold and is absent from DispatchHoldLabels: that list is fixed at the
// two canonical holds, and the hook's held-candidate filter and the in_progress
// serve gate iterate it, so adding human there would also park beads already
// assigned to an agent. Route-scoped, unassigned dispatch excludes it all the
// same, for the reason it excludes a hold: a worker cannot advance a bead that
// is waiting on a person, and serving it back on every tick burns a turn each
// time (ga-7yfnyv). The assignee-scoped tiers stay transparent to it.
const HumanLabel = "human"

// HasDispatchHold reports whether labels carry one of DispatchHoldLabels. It is
// the label comparison the hold-aware WORK-SERVING decisions answer with: the
// hook's serve filter (isHeldHookCandidate), continuation-group pre-assignment
// (preassignHookContinuationGroup), and three controller gates over OPEN rows —
// wake readiness and the keep-awake probe (both through assignedOpenWorkHeld)
// and the drain-ack claimability classifier (classifyDemandRowClaimability).
// So held OPEN assigned work is never wake readiness, keep-awake work or a
// drain-ack strand, which ended the wake/drain loop where an on-demand named
// session was re-woken every tick for assigned hold:external work its hook
// drained past as no_work.
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
