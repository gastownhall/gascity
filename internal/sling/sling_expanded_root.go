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

// expandedRootMemberRoutes is the fill-only routing plan a plain sling of an
// expanded graph.v2 workflow root applies to the run's members. The root is a
// structural latch that never counts as demand (#7497), so a run whose steps
// carry no route of their own (the shape `gc formula cook` leaves) is only
// executable once its steps carry the sling target, as a --formula launch
// stamps them at creation (#9039).
type expandedRootMemberRoutes struct {
	rootID string
	// controls fill the execution route of control beads; they are written
	// before steps so a spawned attempt never reads an empty execution route.
	controls []memberRoutePatch
	steps    []memberRoutePatch
	// elsewhere names the open members already routed to another target,
	// which the fill-only plan leaves on their routes.
	elsewhere []string
	// customQueryUnrouted counts the members a custom sling_query target
	// leaves unrouted: the custom command owns routing, so Go writes none.
	customQueryUnrouted int
}

// customQueryMemberRoutes is the plan for a target whose custom sling_query
// owns routing: it writes nothing and counts the members that carry no route
// of their own, so the sling can say they were not routed.
func customQueryMemberRoutes(rootID, target string, members []beads.Bead) (*expandedRootMemberRoutes, error) {
	plan := &expandedRootMemberRoutes{rootID: rootID}
	probe := graphroute.GraphRouteBinding{QualifiedName: target, MetadataOnly: true}
	for _, member := range members {
		if member.ID == rootID {
			continue
		}
		patch, err := graphroute.UnroutedMemberPatch(member, probe)
		if err != nil {
			return nil, fmt.Errorf("reading the members of workflow %s: %w", rootID, err)
		}
		if patch != nil {
			plan.customQueryUnrouted++
		}
	}
	return plan, nil
}

// memberRoutePatch is the metadata one member gains.
type memberRoutePatch struct {
	beadID string
	meta   map[string]string
}

// hasPatches reports whether the plan writes anything.
func (p *expandedRootMemberRoutes) hasPatches() bool {
	return p != nil && len(p.controls)+len(p.steps) > 0
}

// planExpandedRootMemberRoutes builds the member routing plan for slinging
// beadID from the work store to opts.Target. It returns nil when beadID is not an
// expanded workflow root, when the store does not hold it, or when routing
// runs through a custom sling_query whose meaning Go cannot know. It writes
// nothing.
func planExpandedRootMemberRoutes(opts SlingOpts, deps SlingDeps, store beads.WorkStore, beadID string) (*expandedRootMemberRoutes, error) {
	if store == (beads.WorkStore{}) {
		return nil, nil
	}
	root, err := store.Get(beadID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s to route its workflow members: %w", beadID, err)
	}
	if !graphroute.IsExpandedWorkflowRoot(root.Metadata) {
		return nil, nil
	}
	a := opts.Target
	customQuery := IsCustomSlingQuery(a)
	if deps.Router == nil && !customQuery {
		return nil, nil
	}
	members, err := beads.DirectMembersWithRoot(store, root)
	if err != nil {
		return nil, fmt.Errorf("listing the members of workflow %s: %w", beadID, err)
	}
	if customQuery {
		return customQueryMemberRoutes(root.ID, agentutil.RoutedToIdentity(&a), members)
	}
	binding, err := graphroute.SlingTargetBinding(a, agentutil.RoutedToIdentity(&a), deps.Sessions, deps.CityName, deps.Cfg)
	if err != nil {
		return nil, fmt.Errorf("routing the members of workflow %s: %w", beadID, err)
	}
	plan := &expandedRootMemberRoutes{rootID: root.ID}
	for _, member := range members {
		if member.ID == root.ID {
			continue
		}
		patch, err := graphroute.UnroutedMemberPatch(member, binding)
		if err != nil {
			return nil, fmt.Errorf("routing the members of workflow %s: %w", beadID, err)
		}
		if patch == nil {
			if route := otherMemberRoute(member, binding.QualifiedName); route != "" {
				plan.elsewhere = append(plan.elsewhere, fmt.Sprintf("%s (%s)", member.ID, route))
			}
			continue
		}
		entry := memberRoutePatch{beadID: member.ID, meta: patch}
		if graphroute.IsControlDispatcherKind(member.Metadata[beadmeta.KindMetadataKey]) {
			plan.controls = append(plan.controls, entry)
		} else {
			plan.steps = append(plan.steps, entry)
		}
	}
	return plan, nil
}

// apply writes the plan through writer, controls first, then steps. The
// caller routes the root only after apply succeeds, so a failed write leaves
// the root unrouted and re-running the same sling completes the run.
func (p *expandedRootMemberRoutes) apply(writer beads.Writer) error {
	if !p.hasPatches() {
		return nil
	}
	if writer == nil {
		return fmt.Errorf("routing the members of workflow %s: no store holds the run", p.rootID)
	}
	for _, group := range [][]memberRoutePatch{p.controls, p.steps} {
		for _, patch := range group {
			if err := writer.SetMetadataBatch(patch.beadID, patch.meta); err != nil {
				return fmt.Errorf("routing member %s of workflow %s: %w; re-run the same gc sling to finish routing the run", patch.beadID, p.rootID, err)
			}
		}
	}
	return nil
}

// routedMembers is the number of members the plan routes.
func (p *expandedRootMemberRoutes) routedMembers() int {
	if p == nil {
		return 0
	}
	return len(p.controls) + len(p.steps)
}

// routesControls reports whether the plan gives any control bead an
// execution route, so the control dispatcher should look again.
func (p *expandedRootMemberRoutes) routesControls() bool {
	return p != nil && len(p.controls) > 0
}

// otherMemberRoute returns the route an open executable member carries when
// it names a target other than target: the execution route of a control, the
// delivery route of any other member. It returns "" otherwise.
func otherMemberRoute(member beads.Bead, target string) string {
	kind := member.Metadata[beadmeta.KindMetadataKey]
	if graphroute.IsWorkflowTopologyKind(kind) || member.Status == "closed" {
		return ""
	}
	key := beadmeta.RoutedToMetadataKey
	if graphroute.IsControlDispatcherKind(kind) {
		key = beadmeta.ExecutionRoutedToMetadataKey
	}
	route := strings.TrimSpace(member.Metadata[key])
	if route == "" || route == target {
		return ""
	}
	return route
}

// warnings names the members the plan leaves unrouted by the target: those
// already on another route, and those a custom sling_query target owns.
func (p *expandedRootMemberRoutes) warnings() []string {
	if p == nil {
		return nil
	}
	var out []string
	if len(p.elsewhere) > 0 {
		out = append(out, fmt.Sprintf("warning: workflow %s members already routed to other agents were left on their routes: %s",
			p.rootID, strings.Join(p.elsewhere, ", ")))
	}
	if p.customQueryUnrouted > 0 {
		out = append(out, fmt.Sprintf("warning: workflow %s has %d unrouted steps that were not routed: the target's custom sling_query owns routing; route them with the same command or gc sling --formula",
			p.rootID, p.customQueryUnrouted))
	}
	return out
}
