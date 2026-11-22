package sling

import (
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphv2"
)

// sourceBlockedBy reads the blockers a graph workflow launched from sourceID
// must wait for, from the work store that holds sourceID.
func sourceBlockedBy(deps SlingDeps, sourceID string) ([]string, error) {
	blockers, err := graphv2.SourceBlockers(deps.Store, sourceID)
	if err != nil {
		return nil, fmt.Errorf("reading blockers of %s: %w", sourceID, err)
	}
	return blockers, nil
}

// graphHeldBlockedBy returns blockedBy, the source bead's blockers, as edges
// a graph workflow created in deps.GraphStore can hold. Where that store is
// the work store the blockers are wired as read; where it is relocated, each
// must be held by the graph store (graphv2.GraphHeldBlockers), or the launch
// fails with a *graphv2.CrossStoreBlockersError before anything is created.
func graphHeldBlockedBy(deps SlingDeps, sourceID string, blockedBy []string) ([]string, error) {
	if len(blockedBy) == 0 || beads.SameStore(deps.GraphStore, deps.Store) {
		return blockedBy, nil
	}
	return graphv2.GraphHeldBlockers(deps.GraphStore, sourceID, blockedBy)
}

// GraphWorkflowBlockers returns the open blockers of sourceID a graph
// workflow attached to it waits for; it only reads. It applies the same
// split-city holdability check as a launch (graphHeldBlockedBy), so a
// blocker a relocated graph store cannot hold fails with a
// *graphv2.CrossStoreBlockersError.
func GraphWorkflowBlockers(deps SlingDeps, sourceID string) ([]string, error) {
	blockers, err := sourceBlockedBy(deps, sourceID)
	if err != nil {
		return nil, err
	}
	return graphHeldBlockedBy(deps, sourceID, blockers)
}

// reportWaitingOn makes launch report waitingOn, the source bead's open
// blockers the workflow was created waiting for (molecule.Result.BlockedBy),
// as SlingResult.WaitingOn once it starts.
func reportWaitingOn(launch pendingSourceWorkflowLaunch, waitingOn []string) pendingSourceWorkflowLaunch {
	finalize := launch.finalize
	launch.finalize = func() (SlingResult, error) {
		result, err := finalize()
		result.WaitingOn = waitingOn
		return result, err
	}
	return launch
}
