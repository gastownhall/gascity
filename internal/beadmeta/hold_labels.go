package beadmeta

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
var DispatchHoldLabels = []string{HoldMayorLabel, HoldExternalLabel}
