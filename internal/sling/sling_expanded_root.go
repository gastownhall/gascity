package sling

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphroute"
)

// memberRoutePatch is one member's computed routing write.
type memberRoutePatch struct {
	beadID  string
	control bool
	meta    map[string]string
}

// otherRoutedMember is a member the plan left on another agent's route: the
// member is eligible for routing but already carries a route for someone
// other than the sling target, and member routing never moves in-flight work.
type otherRoutedMember struct {
	beadID string
	target string
}

// memberRoutePlan is the computed-but-unwritten routing plan for the members
// of an expanded workflow root a plain sling targets (issue #9039): `gc
// formula cook` leaves every executable member unrouted and the expanded root
// itself is never pool demand (#7497), so slinging the root must also fill its
// members' routes or an on-demand agent is never woken for the run. A nil plan
// routes nothing: the slung bead is not an expanded workflow root, the store
// does not hold it, or the plain path was not taken.
type memberRoutePlan struct {
	rootID  string
	target  string
	binding graphroute.GraphRouteBinding
	// controlPatches and stepPatches are the members to route, applied in that
	// order before the root's own route is stamped.
	controlPatches []memberRoutePatch
	stepPatches    []memberRoutePatch
	// unroutedMembers counts members the plan routes.
	unroutedMembers int
	// customQuery marks a plan that routes nothing because the target routes
	// through its own command: unroutedMembers then counts what was left
	// alone, and apply warns instead of writing.
	customQuery bool
	// otherRouted collects eligible members already routed to a different
	// agent: they get no patch, and apply reports them in one warning so the
	// operator can see the run did not move to the sling target whole.
	otherRouted []otherRoutedMember
}

// hasPatches reports whether the plan would write any member route.
func (p *memberRoutePlan) hasPatches() bool {
	return p != nil && (len(p.controlPatches) > 0 || len(p.stepPatches) > 0)
}

// hasControlPatches reports whether the plan routes any control members, so
// the caller knows to wake the control dispatcher.
func (p *memberRoutePlan) hasControlPatches() bool {
	return p != nil && len(p.controlPatches) > 0
}

// apply writes the plan's member patches, controls first, through the writer
// of the store that holds the root. The caller stamps the root's own route
// only after this returns, so a failed member write leaves the root unrouted
// and re-running the same sling completes the run instead of short-circuiting
// as idempotent. Members the plan found already routed to a different agent
// are reported in one bead warning whether or not anything was written: the
// run did not move to the target whole, and the operator should see which
// members stayed behind.
func (p *memberRoutePlan) apply(writer beads.Writer, result *SlingResult) error {
	if p == nil {
		return nil
	}
	if p.hasPatches() {
		if writer == nil {
			return fmt.Errorf("routing members of expanded workflow %s to %s: no store holds the workflow", p.rootID, p.target)
		}
		applied := 0
		for _, entry := range p.controlPatches {
			if err := writer.SetMetadataBatch(entry.beadID, entry.meta); err != nil {
				return fmt.Errorf("routing member %s of expanded workflow %s to %s: %w; re-run the same sling to complete routing the run", entry.beadID, p.rootID, p.target, err)
			}
			applied++
		}
		for _, entry := range p.stepPatches {
			if err := writer.SetMetadataBatch(entry.beadID, entry.meta); err != nil {
				return fmt.Errorf("routing member %s of expanded workflow %s to %s: %w; re-run the same sling to complete routing the run", entry.beadID, p.rootID, p.target, err)
			}
			applied++
		}
		if result != nil {
			result.RoutedMembers = applied
		}
	}
	if result != nil && p.customQuery && p.unroutedMembers > 0 {
		result.BeadWarnings = append(result.BeadWarnings, fmt.Sprintf(
			"warning: %d unrouted members of workflow %s were left unrouted: %s routes through its own sling_query, which receives only the root",
			p.unroutedMembers, p.rootID, p.target))
	}
	if result != nil && len(p.otherRouted) > 0 {
		named := make([]string, 0, len(p.otherRouted))
		for _, m := range p.otherRouted {
			named = append(named, fmt.Sprintf("%s (%s)", m.beadID, m.target))
		}
		result.BeadWarnings = append(result.BeadWarnings, fmt.Sprintf(
			"warning: members of workflow %s already routed elsewhere were left in place: %s; a sling fills missing routes, it never moves in-flight work",
			p.rootID, strings.Join(named, ", ")))
	}
	return nil
}

// planExpandedRootMemberRoutes computes the member routing a plain sling of
// beadID needs when beadID is an expanded workflow root. It writes nothing:
// the returned plan is applied by finalizeInStore, members before root, on
// the store that holds the root. The member set is the workflow's
// gc.root_bead_id membership (closed beads included — the same membership
// dispatch's retry and fan-out spawns carry), so nested-scope members and
// already-spawned attempts are covered too.
//
// A run still carrying gc.instantiating — on the root or any member — is
// refused: sequential fallback is wiring its dependency graph, and routing
// its beads out from under that would race the instantiation. The refusal
// fires before any write, so a retried sling once instantiation completes
// starts from the run's untouched state.
func planExpandedRootMemberRoutes(opts SlingOpts, deps SlingDeps, beadID string) (*memberRoutePlan, error) {
	if strings.TrimSpace(beadID) == "" || deps.Store == (beads.WorkStore{}) {
		return nil, nil
	}
	root, err := deps.Store.Get(beadID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			// A forced sling of a bead its store does not hold keeps today's
			// best-effort shape: there is no membership to route.
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s to plan member routing: %w", beadID, err)
	}
	if !graphroute.IsExpandedWorkflowRoot(root.Metadata) {
		return nil, nil
	}
	members, err := beads.DirectMembersWithRoot(deps.Store, root)
	if err != nil {
		return nil, fmt.Errorf("listing the members of workflow %s: %w", root.ID, err)
	}
	for _, b := range append([]beads.Bead{root}, members...) {
		if strings.TrimSpace(b.Metadata[beadmeta.InstantiatingMetadataKey]) != "" {
			return nil, fmt.Errorf(
				"routing workflow %s: %s is mid-instantiation (gc.instantiating set); sling it again once instantiation completes",
				root.ID, b.ID)
		}
	}
	a := opts.Target
	routedTo := agentutil.NormalizePoolRouteTarget(deps.Cfg, agentutil.RoutedToIdentity(&a))
	// Member routing is a built-in feature: when the target routes through
	// its own sling_query command, or the caller wired no typed router at
	// all, that command receives the root alone and defines what routing
	// means for the agent — stamping gc.routed_to on its members behind the
	// command's back would bypass it. The plan still counts the members that
	// would have been routed, so apply can warn instead of staying silent.
	if deps.Router == nil || IsCustomSlingQuery(a) {
		return countExpandedRootMembers(root, routedTo, members)
	}
	binding, err := graphroute.SlingTargetBinding(a, routedTo, deps.Sessions, deps.CityName, deps.Cfg)
	if err != nil {
		return nil, fmt.Errorf("deriving the route for the members of %s: %w", root.ID, err)
	}
	plan := &memberRoutePlan{rootID: root.ID, target: routedTo, binding: binding}
	for _, member := range members {
		if member.ID == root.ID {
			continue
		}
		patch, err := graphroute.UnroutedMemberPatch(member, binding)
		if err != nil {
			return nil, err
		}
		if len(patch) == 0 {
			// No write, but an eligible member already routed to a different
			// agent is worth reporting: the run the operator just slung did
			// not move whole, and nothing else would say so.
			if memberRouteEligible(member) {
				if route := memberRouteTarget(member); route != "" && route != routedTo {
					plan.otherRouted = append(plan.otherRouted, otherRoutedMember{beadID: member.ID, target: route})
				}
			}
			continue
		}
		entry := memberRoutePatch{beadID: member.ID, meta: patch}
		if graphroute.IsControlDispatcherKind(strings.TrimSpace(member.Metadata[beadmeta.KindMetadataKey])) {
			entry.control = true
			plan.controlPatches = append(plan.controlPatches, entry)
		} else {
			plan.stepPatches = append(plan.stepPatches, entry)
		}
		plan.unroutedMembers++
	}
	return plan, nil
}

// countExpandedRootMembers is the plan for a target member routing must not
// touch: the target routes through its own command (a custom sling_query, or
// no typed router at all), so no binding is derived and no patch is built —
// the members the run still needs routed are counted so the sling can say
// they were left alone. The probe binding is metadata-only with the target's
// persisted identity: counting asks only "would built-in routing have
// stamped this member", never a session name or group.
func countExpandedRootMembers(root beads.Bead, routedTo string, members []beads.Bead) (*memberRoutePlan, error) {
	plan := &memberRoutePlan{rootID: root.ID, target: routedTo, customQuery: true}
	probe := graphroute.GraphRouteBinding{QualifiedName: routedTo, MetadataOnly: true}
	for _, member := range members {
		if member.ID == root.ID {
			continue
		}
		patch, err := graphroute.UnroutedMemberPatch(member, probe)
		if err != nil {
			return nil, err
		}
		if len(patch) > 0 {
			plan.unroutedMembers++
		}
	}
	return plan, nil
}

// memberRouteTarget is the route key a member's routing turns on: a control
// member runs its spawned work through gc.execution_routed_to while its own
// gc.routed_to stays the control dispatcher, so that is the route to compare;
// every other member's route is gc.routed_to.
func memberRouteTarget(b beads.Bead) string {
	key := beadmeta.RoutedToMetadataKey
	if graphroute.IsControlDispatcherKind(strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey])) {
		key = beadmeta.ExecutionRoutedToMetadataKey
	}
	return strings.TrimSpace(b.Metadata[key])
}

// memberRouteEligible mirrors the members UnroutedMemberPatch considers for
// routing at all: workflow topology beads never take a route, and closed,
// assigned, or session-bound beads are not the sling's to move. Only eligible
// members count as "routed elsewhere" — a closed bead that once ran on
// another agent is settled, not left behind.
func memberRouteEligible(b beads.Bead) bool {
	if graphroute.IsWorkflowTopologyKind(strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey])) {
		return false
	}
	return b.Status != "closed" && strings.TrimSpace(b.Assignee) == "" && b.Metadata[beadmeta.SessionIDMetadataKey] == ""
}
