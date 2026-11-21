package sling

import (
	"errors"
	"fmt"

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
	if deps.Router == nil || IsCustomSlingQuery(a) {
		return nil, nil
	}
	binding, err := graphroute.SlingTargetBinding(a, agentutil.RoutedToIdentity(&a), deps.Sessions, deps.CityName, deps.Cfg)
	if err != nil {
		return nil, fmt.Errorf("routing the members of workflow %s: %w", beadID, err)
	}
	members, err := beads.DirectMembersWithRoot(store, root)
	if err != nil {
		return nil, fmt.Errorf("listing the members of workflow %s: %w", beadID, err)
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
