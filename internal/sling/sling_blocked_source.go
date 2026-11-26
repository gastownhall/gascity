package sling

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/molecule"
)

// GraphWorkflowBlockers returns the blockers of sourceID a graph workflow
// attached to it waits for (graphv2.SourceBlockers), as edges
// deps.GraphStore can hold; it only reads. Where the graph store is the work
// store the blockers are wired as read; where it is relocated, each must be
// held by the graph store (graphv2.GraphHeldBlockers), or it fails with a
// *graphv2.CrossStoreBlockersError, as the launch would before creating
// anything.
func GraphWorkflowBlockers(deps SlingDeps, sourceID string) ([]molecule.Blocker, error) {
	blockers, err := graphv2.SourceBlockers(deps.Store, sourceID)
	if err != nil {
		return nil, fmt.Errorf("reading blockers of %s: %w", sourceID, err)
	}
	if len(blockers) == 0 || beads.SameStore(deps.GraphStore, deps.Store) {
		return blockers, nil
	}
	return graphv2.GraphHeldBlockers(deps.GraphStore, sourceID, blockers)
}

// graphLaunchBlockedBy returns the blockers a graph workflow launched from
// sourceID waits for. The workflow routes its own steps, not the source bead,
// so it carries the source bead's blockers itself: blockedBy when the caller
// read them where it knew the source (the convoy-first launch, which passes no
// source id), else GraphWorkflowBlockers of sourceID.
func graphLaunchBlockedBy(deps SlingDeps, sourceID string, blockedBy []molecule.Blocker) ([]molecule.Blocker, error) {
	if blockedBy != nil || sourceID == "" {
		return blockedBy, nil
	}
	return GraphWorkflowBlockers(deps, sourceID)
}

// IsUnholdableBlockersError reports whether err refused a graph launch only
// because the workflow's store cannot hold the source bead's blockers
// (*graphv2.CrossStoreBlockersError). The source bead itself is still
// routable: its own edges hold it, so a launch the caller never asked for (a
// target's default_sling_formula) routes it plainly instead.
func IsUnholdableBlockersError(err error) bool {
	var crossErr *graphv2.CrossStoreBlockersError
	return errors.As(err, &crossErr)
}

// reportWaitingOn makes launch report waitingOn, the blockers the workflow
// was created waiting for (molecule.Result.BlockedBy), as
// SlingResult.WaitingOn once it starts.
func reportWaitingOn(launch pendingSourceWorkflowLaunch, waitingOn []molecule.Blocker) pendingSourceWorkflowLaunch {
	finalize := launch.finalize
	launch.finalize = func() (SlingResult, error) {
		result, err := finalize()
		result.WaitingOn = molecule.BlockerIDs(waitingOn)
		return result, err
	}
	return launch
}
