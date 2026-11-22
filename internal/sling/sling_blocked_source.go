package sling

import (
	"fmt"

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
