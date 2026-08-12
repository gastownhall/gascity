package runproj

// terminalRunNodeStatusSet is the membership form of the terminalRunNodeStatuses
// taxonomy (detail.go). It is derived from that slice — the one
// TestRunNodeStatusTaxonomyIsExhaustive guards — so a status added to the
// taxonomy is reflected here without a second edit, keeping the clamp in lockstep
// with the single terminality source.
var terminalRunNodeStatusSet = func() map[string]bool {
	set := make(map[string]bool, len(terminalRunNodeStatuses))
	for _, status := range terminalRunNodeStatuses {
		set[status] = true
	}
	return set
}()

// isTerminalRunNodeStatus reports whether status is a terminal run-node status,
// per the terminalRunNodeStatuses taxonomy.
func isTerminalRunNodeStatus(status string) bool {
	return terminalRunNodeStatusSet[status]
}

// terminalRootClamp captures whether a run's terminal root forces a presentation
// clamp on its steps. A terminal root cannot have running steps: once the root
// folds to a terminal status, a step still presenting as non-terminal lost its
// own close event (a worker-side close that never emitted, or a reaper that
// deleted the step bead before it did), so the projection presents it as finished
// rather than eternally "Running". The clamp is presentation-only — the folded
// bead data is never mutated, so a late-arriving close event re-derives the real
// status on the next build.
type terminalRootClamp struct {
	active bool
	// nonPendingTarget is the status a non-terminal, non-pending step collapses to:
	// "completed" under a successful root, "canceled" under a failed/canceled/
	// skipped root. Only meaningful when active.
	nonPendingTarget string
}

// rootClampFor derives the clamp implied by a run's root bead. A nil root (root
// absent from the fold) or a non-terminal root yields an inactive clamp, so the
// projection is unchanged for every non-terminal or root-less run.
func rootClampFor(root *runSnapshotBead) terminalRootClamp {
	if root == nil {
		return terminalRootClamp{}
	}
	return clampForRootStatus(presentationStatus(*root))
}

// clampForRootStatus derives the clamp for an already-resolved root presentation
// status. A non-terminal root yields an inactive clamp.
func clampForRootStatus(rootStatus string) terminalRootClamp {
	if !isTerminalRunNodeStatus(rootStatus) {
		return terminalRootClamp{}
	}
	// A successful root (completed/done) collapses its unfinished steps to
	// "completed"; every other terminal root (failed/canceled/skipped) collapses
	// them to "canceled". presentationStatus never yields "done", but classifying
	// it here keeps the mapping total over the terminal taxonomy.
	target := "canceled"
	if rootStatus == "completed" || rootStatus == "done" {
		target = "completed"
	}
	return terminalRootClamp{active: true, nonPendingTarget: target}
}

// ClampStepStatusForRun clamps a single step's presentation status against its
// run's canonical lifecycle state, keeping the terminal-root clamp as the one
// terminality source shared by the typed run API and the dashboard projection. A
// terminal run (completed/failed/canceled/skipped) collapses each non-terminal
// step exactly as the detail DAG does; a non-terminal run (pending/active/
// waiting/canceling) yields an inactive clamp and returns the step status
// unchanged. step is a RunStepStatus-vocabulary value (pending/active/blocked/
// completed/failed/skipped/canceled).
func ClampStepStatusForRun(run CanonicalRunStatus, step string) string {
	return clampForRootStatus(string(run)).apply(step)
}

// apply clamps one step presentation status under a terminal root. Already-terminal
// statuses are returned unchanged — they are real recorded outcomes, including the
// gc.outcome-derived failed/skipped/canceled a closed step carries. A non-terminal
// pending step (never started) becomes "skipped"; every other non-terminal step
// (active/running/ready/blocked) becomes the root's terminal target. An inactive
// clamp returns the status unchanged.
func (c terminalRootClamp) apply(status string) string {
	if !c.active || isTerminalRunNodeStatus(status) {
		return status
	}
	if status == "pending" {
		return "skipped"
	}
	return c.nonPendingTarget
}
