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
//     work — including the assignee-scoped crash-recovery tier and the
//     `gc hook --claim` result (gas-kg6). A held bead handed back as work
//     cannot be advanced, is never released, and so is re-served forever.
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
// hook refuses held rows. So the controller's assigned-work WAKE demand and the
// drain-ack claimability classifier exclude open held work (HasDispatchHold),
// while the assignment itself stays visible to orphan release, pool accounting
// and the in_progress ownership tiers above.
var DispatchHoldLabels = []string{HoldMayorLabel, HoldExternalLabel}

// HasDispatchHold reports whether labels carry one of DispatchHoldLabels. It is
// the single label comparison every WORK-SERVING decision answers with — the
// hook's serve filter (isHeldHookCandidate), session wake demand from assigned
// open work, and the drain-ack claimability classifier — so the controller can
// never treat as wake demand a row the session's own hook will refuse to serve
// (the wake/drain loop where an on-demand named session was re-woken every tick
// for assigned hold:external work its hook drained past as no_work).
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
