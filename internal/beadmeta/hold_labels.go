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

// DispatchHoldLabels is the complete set of hold label values that must
// exclude a bead from route-scoped, unassigned automatic dispatch (Tier 3
// pool-demand queries and the control dispatcher's routed/run-target
// tiers). Assignee-scoped queries (Tier 1 crash recovery, Tier 2 assigned-
// ready) are hold-transparent by design and must never filter on this list
// (ga-5736js).
var DispatchHoldLabels = []string{HoldMayorLabel, HoldExternalLabel}
