package molecule

import (
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/workflowstop"
)

// The workflow-stop walk lives in internal/workflowstop (moved verbatim from
// this file): molecule imports the formula layer, and the readers that judge
// the same stop semantics — readywork, input admission — must not. The
// exported names stay, as an alias and one-line delegations, so every caller
// keeps compiling against molecule.

// WorkflowStop names the workflow root that makes a bead's work invalid: the
// nearest root on its ancestor chain that has settled or been canceled. The
// zero value means the chain is live.
type WorkflowStop = workflowstop.WorkflowStop

// StoppedWorkflowAncestor walks bead's workflow ancestry and returns the
// nearest root that is closed (any outcome) or carries cancel intent; see
// workflowstop.StoppedWorkflowAncestor.
func StoppedWorkflowAncestor(store beads.MoleculeTarget, bead beads.Bead) (WorkflowStop, error) {
	if store.Absent() {
		return WorkflowStop{}, nil
	}
	return workflowstop.StoppedWorkflowAncestor(store.Get, bead)
}

// WorkflowStopReason reports whether bead's work must not start because a
// workflow it belongs to has already stopped. It is StoppedWorkflowAncestor
// with the teardown tail exempted: teardown runs after its root reaches a
// terminal state by contract, so a settled or canceled root is the condition
// it waits for, not a reason to skip it.
func WorkflowStopReason(store beads.MoleculeTarget, bead beads.Bead) (WorkflowStop, error) {
	stop, err := StoppedWorkflowAncestor(store, bead)
	if err != nil || !stop.Stopped() {
		return WorkflowStop{}, err
	}
	teardown, err := IsTeardownTailMember(store, bead)
	if err != nil {
		return WorkflowStop{}, err
	}
	if teardown {
		return WorkflowStop{}, nil
	}
	return stop, nil
}

// IsTeardownTailMember reports whether bead belongs to its workflow's teardown
// tail as TeardownTailExclusion defines it. The cheap arm answers for a bead
// that carries gc.scope_role=teardown itself; an attempt linked to a teardown
// step only by gc.step_id needs the subtree read.
func IsTeardownTailMember(store beads.MoleculeTarget, bead beads.Bead) (bool, error) {
	if bead.Metadata[beadmeta.ScopeRoleMetadataKey] == beadmeta.ScopeRoleTeardown {
		return true, nil
	}
	rootID := strings.TrimSpace(bead.Metadata[beadmeta.RootBeadIDMetadataKey])
	if rootID == "" || strings.TrimSpace(bead.Metadata[beadmeta.StepIDMetadataKey]) == "" {
		return false, nil
	}
	exclude, err := TeardownTailExclusion(store, rootID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return exclude(bead), nil
}
