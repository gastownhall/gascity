package molecule

import (
	"errors"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/workflowstop"
)

// WorkflowStop names the workflow root that makes a bead's work invalid
// (workflowstop.Stop).
type WorkflowStop = workflowstop.Stop

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

// StoppedWorkflowAncestor returns the nearest root on bead's workflow ancestry
// that is closed or carries cancel intent (workflowstop.StoppedWorkflowAncestor).
// An absent store holds no workflow, so nothing has stopped.
func StoppedWorkflowAncestor(store beads.MoleculeTarget, bead beads.Bead) (WorkflowStop, error) {
	if store.Absent() {
		return WorkflowStop{}, nil
	}
	return workflowstop.StoppedWorkflowAncestor(store, bead)
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
