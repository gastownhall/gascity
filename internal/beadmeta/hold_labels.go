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
//     assigned-ready tier (assignedReadyTierCommand and its bd-1.0.4 wisp
//     probe ephemeralAssignedReadyProbeScript, #8010), the `gc hook --claim`
//     result (gas-kg6) and the continuation-group siblings a claim pre-assigns
//     (preassignHookContinuationGroup, #6026). A held bead handed back as work
//     cannot be advanced, is never released, and so is re-served forever.
//
//   - "Does a session still need to EXIST for this bead?" — hold is
//     irrelevant; the assignment is a real ownership fact either way. The
//     demand/liveness tier that answers this (filterReadyByAssignee) stays
//     hold-transparent by design and must never filter on this list
//     (ga-5736js), or a parked bead's owner would go invisible to the pool and
//     to crash recovery.
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
// time (ga-7yfnyv). The assignee-scoped tiers stay transparent to it. The
// dead-assignee sweep (releaseOrphanedPoolAssignments) skips it too: released,
// it would be served to no one, and its holder may be a person, whom no session
// answers to.
const HumanLabel = "human"

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

// OperatorIdentity is the reserved identity of the person operating the city.
// gc already treats it as a person rather than a session in several places:
// mail resolves it as a recipient (mail.ResolveRecipient), session.ValidateAlias
// refuses to mint it as a session alias, and the mail command defaults its
// sender and recipient to it. No [[agent]] defines it, every city has one, and
// the core orphan-sweep order exempts it by the same exact match (#3440).
//
// It is spelled like HumanLabel but is a different bead field with a different
// meaning: HumanLabel says a bead WAITS ON a person; OperatorIdentity says a
// person HOLDS it. They stay separate constants so a change to one cannot
// silently change the other.
const OperatorIdentity = "human"

// IsOperatorIdentity reports whether identity is the reserved operator
// identity. The comparison is exact and case-sensitive, like the shell
// orphan-sweep's is_known_agent and like HasHumanLabel, and it trims because
// the dead-assignee sweep trims every assignee before testing it. "Human",
// "humanoid", "human-1" and "human/alice" stay ordinary identities.
func IsOperatorIdentity(identity string) bool {
	return strings.TrimSpace(identity) == OperatorIdentity
}

// HasDispatchHold reports whether labels carry one of DispatchHoldLabels. It is
// the label comparison the hold-aware WORK-SERVING decisions answer with: the
// hook's serve filter (isHeldHookCandidate), continuation-group pre-assignment
// (preassignHookContinuationGroup), and three controller gates over OPEN rows —
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
