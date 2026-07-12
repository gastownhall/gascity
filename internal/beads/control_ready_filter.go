package beads

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// ControlReadyFilter selects the ready control beads a control dispatcher cares
// about out of a federated ready set: beads assigned to one of Assignees, or
// unassigned beads routed — by run-target, routed-to, or execution-routed-to
// metadata — to one of Routes. PerGroupLimit bounds how many beads each assignee
// and each route may contribute (0 = unbounded); dedup is global across groups.
//
// It exists so GET /beads/ready?cached=true can apply the dispatcher's predicate
// and limit server-side, before serialization, instead of shipping the entire
// federated ready set across the API for the dispatcher to filter client-side.
// The dispatcher applies the same filter again on its side as idempotent
// defense-in-depth, so the server and client predicates cannot drift.
type ControlReadyFilter struct {
	Assignees     []string
	Routes        []string
	PerGroupLimit int
}

// Active reports whether the filter has any assignee or route to match. An
// inactive filter is a no-op: Apply returns its input unchanged so the generic
// /beads/ready callers (CLI, dashboard) keep the full federated ready set.
func (f ControlReadyFilter) Active() bool {
	return len(f.Assignees) > 0 || len(f.Routes) > 0
}

// Apply returns the ready control beads matching this filter. It preserves the
// input order within each group and applies PerGroupLimit per assignee and per
// route, deduping globally by bead ID. An inactive filter returns in unchanged.
//
// The grouped, per-group-limited selection mirrors the control dispatcher's
// historical scan exactly, and is idempotent — Apply(Apply(x)) == Apply(x) —
// because assignee groups are mutually disjoint (a bead has one assignee) and
// route groups only match unassigned beads, so re-applying on an already-filtered
// set reselects the same beads. That is what lets the server pre-filter and the
// dispatcher re-filter without changing the result.
func (f ControlReadyFilter) Apply(in []Bead) []Bead {
	if !f.Active() {
		return in
	}
	seen := make(map[string]struct{}, len(in))
	var out []Bead
	addMatches := func(match func(Bead) bool) {
		added := 0
		for _, b := range in {
			if f.PerGroupLimit > 0 && added >= f.PerGroupLimit {
				return
			}
			if !match(b) || !controlReadyEligible(b) {
				continue
			}
			if _, ok := seen[b.ID]; ok {
				continue
			}
			seen[b.ID] = struct{}{}
			out = append(out, b)
			added++
		}
	}
	for _, assignee := range f.Assignees {
		assignee := assignee
		addMatches(func(b Bead) bool { return b.Assignee == assignee })
	}
	// Match an unassigned bead by any routing key that names a control route.
	// This trusts the routing metadata without re-checking gc.kind: that is safe
	// because graph routing stamps a control-dispatcher route (run-target,
	// routed-to, or execution-routed-to) exclusively on control-kind steps —
	// DecorateGraphWorkflowRecipe gates the control binding on
	// graphroute.IsControlDispatcherKind, so a non-control bead never carries a
	// control-dispatcher route to match here (pinned by
	// TestAssignGraphStepRoute_NonControlStepOmitsControlDispatcherRoute). A bead
	// admitted here therefore has a control kind the dispatcher's ProcessControl
	// switch handles, not one it would quarantine.
	for _, route := range f.Routes {
		route := route
		addMatches(func(b Bead) bool {
			if strings.TrimSpace(b.Assignee) != "" {
				return false
			}
			return b.Metadata[beadmeta.RunTargetMetadataKey] == route ||
				b.Metadata[beadmeta.RoutedToMetadataKey] == route ||
				b.Metadata[beadmeta.ExecutionRoutedToMetadataKey] == route
		})
	}
	return out
}

// controlReadyEligible guards which ready beads a control dispatcher may claim:
// a non-empty ID, not an epic, and no transient gc.instantiating marker (which
// flags a workflow root whose molecule has not finished instantiating). Fenced
// workflow roots are already excluded by the ready projection; the marker check
// is defense-in-depth so a future change to the fencing path cannot let the
// dispatcher run control work before instantiation clears it.
func controlReadyEligible(b Bead) bool {
	return strings.TrimSpace(b.ID) != "" &&
		strings.TrimSpace(b.Type) != "epic" &&
		strings.TrimSpace(b.Metadata[beadmeta.InstantiatingMetadataKey]) == ""
}
