package runproj

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

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
// folds to a terminal status, a step still presenting as non-terminal is either
// about to be closed by dispatch or lost its own close event (a worker-side close
// that never emitted, or a reaper that deleted the step bead before it did), so
// the projection presents it as finished rather than eternally "Running". The
// clamp is presentation-only — the folded bead data is never mutated, so a
// late-arriving close event re-derives the real status on the next build.
//
// The clamped status is the one dispatch durably writes when it closes such a
// step, so a step never reads one terminal status and then another. A root
// settled by the workflow finalizer (pass or fail) leaves its open members to
// the finalizer's sweep, which closes each gc.outcome=skipped whether or not it
// started (dispatch.processWorkflowFinalize); a root closed skipped
// (sourceworkflow.CloseWorkflowSubtree) closes its members skipped too. A run
// canceled via POST /runs/{id}/cancel closes its open members
// gc.outcome=canceled, started or not, and a finalizer settling a canceled
// workflow sweeps them canceled.
//
// The exception is a member that executes after the root settles by contract:
// the workflow finalizer closes the root before it closes itself, and the
// teardown tail runs because the root settled. An open one under a terminal
// root has not lost its close; it has not closed yet, and it will close with
// its own outcome. Clamping it would present a step that is about to run (or
// is finishing) as skipped or canceled, and the next read would contradict
// that with its real outcome, so it keeps its own status.
type terminalRootClamp struct {
	active bool
	// target is the status a non-terminal step collapses to: "canceled" under a
	// canceled root, "skipped" under every other terminal root. Only meaningful
	// when active.
	target string
	// postTerminal holds the ids of the run members that execute after the root
	// settles (see postTerminalMemberIDs); the clamp leaves them unchanged.
	postTerminal map[string]bool
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
	// A canceled root's unfinished steps were (or will be) closed canceled by the
	// run cancel; every other terminal root's were (or will be) swept skipped.
	target := "skipped"
	if rootStatus == "canceled" {
		target = "canceled"
	}
	return terminalRootClamp{active: true, target: target}
}

// RunStepClamp clamps a run's step statuses against the run's canonical
// lifecycle state, keeping the terminal-root clamp as the one terminality source
// shared by the typed run API and the dashboard projection. A terminal run
// (completed/failed/canceled/skipped) collapses each non-terminal step exactly
// as the detail DAG does, except a step that executes after the root settles; a
// non-terminal run (pending/active/waiting/canceling) leaves every status
// unchanged.
type RunStepClamp struct {
	clamp terminalRootClamp
}

// NewRunStepClamp builds the clamp for a run in state run whose members are
// members (the run's beads; the root may be among them).
func NewRunStepClamp(run CanonicalRunStatus, members []beads.Bead) RunStepClamp {
	clamp := clampForRootStatus(string(run))
	if clamp.active {
		clamp.postTerminal = postTerminalMemberIDs(members,
			func(b beads.Bead) string { return b.ID },
			func(b beads.Bead) map[string]string { return b.Metadata })
	}
	return RunStepClamp{clamp: clamp}
}

// Apply clamps the status of the step with id stepID. status is a
// RunStepStatus-vocabulary value (pending/active/blocked/completed/failed/
// skipped/canceled).
func (c RunStepClamp) Apply(stepID, status string) string {
	return c.clamp.applyTo(stepID, status)
}

// postTerminalMemberIDs returns the ids of the run members that execute after
// the run's root settles by contract, so a terminal root says nothing about
// them: the workflow finalizer (gc.kind=workflow-finalize), which closes the
// root before itself, and the teardown tail — a teardown-role member and every
// member generated for a teardown step (same gc.step_id). It mirrors the
// members the finalizer leaves open when it sweeps the rest skipped
// (dispatch.processWorkflowFinalize, molecule.TeardownTailExclusionFromMembers).
func postTerminalMemberIDs[T any](members []T, id func(T) string, metadata func(T) map[string]string) map[string]bool {
	teardownSteps := make(map[string]bool)
	for _, member := range members {
		md := metadata(member)
		if md[beadmeta.ScopeRoleMetadataKey] != beadmeta.ScopeRoleTeardown {
			continue
		}
		if step := strings.TrimSpace(md[beadmeta.StepIDMetadataKey]); step != "" {
			teardownSteps[step] = true
		}
	}
	ids := make(map[string]bool)
	for _, member := range members {
		md := metadata(member)
		if md[beadmeta.KindMetadataKey] == beadmeta.KindWorkflowFinalize ||
			md[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleTeardown ||
			teardownSteps[strings.TrimSpace(md[beadmeta.StepIDMetadataKey])] {
			ids[id(member)] = true
		}
	}
	return ids
}

// withPostTerminal returns the clamp with the members of a run that execute
// after its root settles exempted. An inactive clamp skips the scan.
func (c terminalRootClamp) withPostTerminal(members []runSnapshotBead) terminalRootClamp {
	if !c.active {
		return c
	}
	c.postTerminal = postTerminalMemberIDs(members,
		func(b runSnapshotBead) string { return b.id },
		func(b runSnapshotBead) map[string]string { return b.metadata })
	return c
}

// applyTo clamps the status of the member with id beadID: unchanged for a
// member that executes after the root settles, apply otherwise.
func (c terminalRootClamp) applyTo(beadID, status string) string {
	if c.postTerminal[beadID] {
		return status
	}
	return c.apply(status)
}

// apply clamps one step presentation status under a terminal root. Already-terminal
// statuses are returned unchanged — they are real recorded outcomes, including the
// gc.outcome-derived failed/skipped/canceled a closed step carries. Every
// non-terminal step, started or not, becomes the root's terminal target. An
// inactive clamp returns the status unchanged.
func (c terminalRootClamp) apply(status string) string {
	if !c.active || isTerminalRunNodeStatus(status) {
		return status
	}
	return c.target
}
