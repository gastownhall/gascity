package graphroute

import (
	"fmt"
	"maps"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// IsExpandedWorkflowRoot reports whether meta carries the shape of a fully
// expanded graph.v2 workflow root: gc.kind=workflow with
// gc.workflow_expanded=true. This is the same predicate the demand-side guard
// (workflowTopologyIsNonExecutable in cmd/gc) uses to exclude the root from
// pool demand, so the two can never drift apart: whoever the guard drops,
// member routing is responsible for giving a route to.
func IsExpandedWorkflowRoot(meta map[string]string) bool {
	return strings.TrimSpace(meta[beadmeta.KindMetadataKey]) == beadmeta.KindWorkflow &&
		strings.TrimSpace(meta[beadmeta.WorkflowExpandedMetadataKey]) == "true"
}

// SlingTargetBinding derives the default route binding for a sling target,
// exactly as ApplyGraphRouting does for a --formula launch: pool targets stay
// metadata-only, single-session targets additionally resolve a session name.
// routedTo is the caller's already-normalized persisted identity; it is
// preserved rather than recomputed so member routing and the root's own
// gc.routed_to stamp can never disagree.
func SlingTargetBinding(a config.Agent, routedTo string, store beads.SessionStore, cityName string, cfg *config.City) (GraphRouteBinding, error) {
	binding := GraphRouteBindingForAgent(a)
	binding.QualifiedName = routedTo
	if !binding.MetadataOnly {
		binding.SessionName = agentutil.LookupSessionName(store, cityName, a.QualifiedName(), cfg.Workspace.SessionTemplate)
		if binding.SessionName == "" {
			return GraphRouteBinding{}, fmt.Errorf("could not resolve session name for %q", a.QualifiedName())
		}
	}
	return binding, nil
}

// UnroutedMemberPatch computes the routing metadata a plain sling of an
// expanded workflow root writes on one member bead that carries no route
// (issue #9039: `gc formula cook` leaves every executable member unrouted, and
// the expanded root itself is never demand). It is the bead-level counterpart
// of the decoration ApplyGraphRouting performs at launch, so a cooked root
// slung by hand ends up routed exactly as `gc sling <agent> <formula>
// --formula` would have routed it:
//
//   - workflow topology beads (root workflow, scope latch, formula spec),
//     closed beads, beads with an assignee, and beads carrying a gc.session_id
//     direct binding get no patch: routing never lands on them;
//   - a control bead (check, ralph, fanout, drain, …) gets the execution
//     route a launch stamps and cook leaves empty: gc.execution_routed_to set
//     to the sling target, plus gc.execution_rig_context for a rig-scoped
//     one. Its own gc.routed_to (the control dispatcher, stamped at cook
//     time) is never touched — a control member never routes to the sling
//     target itself;
//   - any other member with an empty gc.routed_to gets what
//     ApplyGraphRouteBinding stamps on a step view of the bead —
//     gc.routed_to, plus gc.session_name for a single-session target, plus
//     the group/affinity pair when a formula-declared continuation group is
//     carried. The group capture mirrors launch decoration (DecorateGraph
//     WorkflowRecipe): the bead's own gc.continuation_group is carried into
//     the binding before stamping when it is not "drain:"-prefixed, so a
//     formula-declared group is honored — never silently dropped — while a
//     drain group, the drain router's own transient bookkeeping, is never
//     upgraded to a pool opt-in. The patch never deletes keys, so a drain
//     group the router left stays as it is;
//   - a member that already carries a route gets no patch: the sling fills
//     missing routes, it never moves in-flight work between agents.
//
// Errors pass through from ApplyGraphRouteBinding unchanged.
func UnroutedMemberPatch(b beads.Bead, binding GraphRouteBinding) (map[string]string, error) {
	kind := strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey])
	if IsWorkflowTopologyKind(kind) {
		return nil, nil
	}
	if b.Status == "closed" || strings.TrimSpace(b.Assignee) != "" || b.Metadata[beadmeta.SessionIDMetadataKey] != "" {
		return nil, nil
	}
	if IsControlDispatcherKind(kind) {
		// The control arm of AssignGraphStepRoute: the execution binding names
		// who runs the work a control spawns, while the control's own route
		// stays the dispatcher cook already stamped.
		if strings.TrimSpace(b.Metadata[beadmeta.ExecutionRoutedToMetadataKey]) != "" {
			return nil, nil
		}
		if strings.TrimSpace(binding.QualifiedName) == "" {
			return nil, nil
		}
		patch := map[string]string{beadmeta.ExecutionRoutedToMetadataKey: binding.QualifiedName}
		if rigContext := graphBindingRigContext(binding); rigContext != "" {
			patch[beadmeta.ExecutionRigContextMetadataKey] = rigContext
		}
		return patch, nil
	}
	if strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != "" {
		return nil, nil
	}
	meta := b.Metadata
	if meta == nil {
		meta = map[string]string{}
	}
	step := &formula.RecipeStep{ID: b.ID, Title: b.Title, Metadata: maps.Clone(meta)}
	member := binding
	member.ContinuationGroup = ""
	if group := strings.TrimSpace(step.Metadata[beadmeta.ContinuationGroupMetadataKey]); group != "" && !strings.HasPrefix(group, "drain:") {
		member.ContinuationGroup = group
	}
	if err := ApplyGraphRouteBinding(step, member); err != nil {
		return nil, err
	}
	patch := make(map[string]string)
	if v := step.Metadata[beadmeta.RoutedToMetadataKey]; strings.TrimSpace(v) != "" {
		patch[beadmeta.RoutedToMetadataKey] = v
	}
	if v := step.Metadata[beadmeta.SessionNameMetadataKey]; strings.TrimSpace(v) != "" && v != meta[beadmeta.SessionNameMetadataKey] {
		patch[beadmeta.SessionNameMetadataKey] = v
	}
	if strings.TrimSpace(step.Metadata[beadmeta.SessionAffinityMetadataKey]) != "" && strings.TrimSpace(step.Metadata[beadmeta.ContinuationGroupMetadataKey]) != "" {
		patch[beadmeta.ContinuationGroupMetadataKey] = step.Metadata[beadmeta.ContinuationGroupMetadataKey]
		patch[beadmeta.SessionAffinityMetadataKey] = step.Metadata[beadmeta.SessionAffinityMetadataKey]
	}
	if len(patch) == 0 {
		return nil, nil
	}
	return patch, nil
}
