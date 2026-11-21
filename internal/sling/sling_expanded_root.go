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
// as idempotent.
func (p *memberRoutePlan) apply(writer beads.Writer, result *SlingResult) error {
	if !p.hasPatches() {
		return nil
	}
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
	return nil
}

// planExpandedRootMemberRoutes computes the member routing a plain sling of
// beadID needs when beadID is an expanded workflow root. It writes nothing:
// the returned plan is applied by finalizeInStore, members before root, on
// the store that holds the root. The member set is the workflow's
// gc.root_bead_id membership (closed beads included — the same membership
// dispatch's retry and fan-out spawns carry), so nested-scope members and
// already-spawned attempts are covered too.
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
	a := opts.Target
	routedTo := agentutil.NormalizePoolRouteTarget(deps.Cfg, agentutil.RoutedToIdentity(&a))
	binding, err := graphroute.SlingTargetBinding(a, routedTo, deps.Sessions, deps.CityName, deps.Cfg)
	if err != nil {
		return nil, fmt.Errorf("deriving the route for the members of %s: %w", root.ID, err)
	}
	members, err := beads.DirectMembersWithRoot(deps.Store, root)
	if err != nil {
		return nil, fmt.Errorf("listing the members of workflow %s: %w", root.ID, err)
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
