package graphroute

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
)

// IsExpandedWorkflowRoot reports whether meta carries the shape of a fully
// expanded graph.v2 workflow root: gc.kind=workflow with
// gc.workflow_expanded=true. It mirrors the workflow-root arm of the
// demand-side guard (workflowTopologyIsNonExecutable in cmd/gc), which drops
// such a root from pool demand: whoever the guard drops, member routing is
// responsible for giving a route to. The two are separate copies; cmd/gc's
// TestSlingCookedRootCreatesDemandForTarget fails if the guard stops dropping
// the root a sling routes members for.
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

// MemberRouteEligible reports whether member routing considers b at all, the
// check UnroutedMemberPatch makes before it looks at any route. Workflow
// topology beads (root workflow, scope latch, formula spec) and cross-store
// gates (beadmeta.CrossStoreGateLabel, minted by a drain control at run time)
// never take a route, as a launch routes neither; a formula's own gate step
// carries no such label and is eligible like any step. Closed and assigned
// beads, and beads carrying a gc.session_id direct binding, are not a sling's
// to move. An eligible member may still take no patch because it already
// carries a route.
func MemberRouteEligible(b beads.Bead) bool {
	if IsWorkflowTopologyKind(strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey])) || slices.Contains(b.Labels, beadmeta.CrossStoreGateLabel) {
		return false
	}
	return b.Status != "closed" && strings.TrimSpace(b.Assignee) == "" && b.Metadata[beadmeta.SessionIDMetadataKey] == ""
}

// UnroutedMemberPatch computes the routing metadata a plain sling of an
// expanded workflow root writes on one member bead that carries no route
// (issue #9039: `gc formula cook` leaves every executable member unrouted, and
// the expanded root itself is never demand). It is the bead-level counterpart
// of the decoration ApplyGraphRouting performs at launch, so a cooked root
// slung by hand ends up routed exactly as `gc sling <agent> <formula>
// --formula` would have routed it:
//
//   - a member MemberRouteEligible rejects (workflow topology, a cross-store
//     gate, a closed or assigned bead, a gc.session_id direct binding) gets no
//     patch: routing never lands on it;
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
// The patch is judged on b as given, a snapshot. A route or claim that lands
// after b was read is invisible to it, and a blind write of the patch
// overwrites that route. So a caller computes the patch from a fresh read of
// the bead and writes it fenced on that read's revision
// (beads.ConditionalWriter's UpdateIfMatch), reading and judging the bead
// again when the fence refuses the write, as the sling's member routing does.
// On a store that cannot fence, the window left is between that read and the
// write.
//
// An ApplyGraphRouteBinding error is returned wrapped with the member's ID.
func UnroutedMemberPatch(b beads.Bead, binding GraphRouteBinding) (map[string]string, error) {
	if !MemberRouteEligible(b) {
		return nil, nil
	}
	kind := strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey])
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
		return nil, fmt.Errorf("routing member %s: %w", b.ID, err)
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
