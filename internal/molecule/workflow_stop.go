package molecule

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// maxWorkflowStopHops bounds the ancestor walk in StoppedWorkflowAncestor. Each
// hop climbs one workflow level (step -> root -> drain control -> parent root),
// and real drain nesting is a handful of levels deep, so a chain this long is a
// corrupt or cyclic link rather than a workflow.
const maxWorkflowStopHops = 16

// WorkflowStop names the workflow root that makes a bead's work invalid: the
// nearest root on its ancestor chain that has settled or been canceled. The
// zero value means the chain is live.
type WorkflowStop struct {
	// RootID is the stopped root.
	RootID string
	// Canceled reports that the root was canceled (gc.outcome=canceled or the
	// gc.cancel_requested intent marker) rather than settled some other way.
	Canceled bool
}

// Stopped reports whether a stopped root was found.
func (s WorkflowStop) Stopped() bool { return s.RootID != "" }

// Outcome is the gc.outcome a bead stranded by this stop closes with: canceled
// under a cancellation, otherwise skipped — the bead never ran to a verdict, so
// it takes the same residue outcome the finalizer's own sweep stamps.
func (s WorkflowStop) Outcome() string {
	if s.Canceled {
		return beadmeta.OutcomeCanceled
	}
	return beadmeta.OutcomeSkipped
}

// WorkflowStopReason reports whether bead's work must not start because a
// workflow it belongs to has already stopped. It is StoppedWorkflowAncestor
// with the teardown tail exempted: teardown runs after its root reaches a
// terminal state by contract, so a settled or canceled root is the condition
// it waits for, not a reason to skip it.
func WorkflowStopReason(store beads.Store, bead beads.Bead) (WorkflowStop, error) {
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

// StoppedWorkflowAncestor walks bead's workflow ancestry and returns the
// nearest root that is closed (any outcome) or carries cancel intent.
//
// A step names its root in gc.root_bead_id. A drain item root is a workflow of
// its own whose steps name IT, not the run that drained into it; its only link
// upward is gc.drain_control_id, the drain control bead, which is a member of
// the parent workflow and so names the parent root in its own gc.root_bead_id.
// The walk follows that chain to the top, so a step under an open item root
// still stops when the run above it was canceled.
//
// A link that does not resolve in store (deleted, or resident in another
// store) ends the walk without a stop, as does a cycle or a chain longer than
// maxWorkflowStopHops: this is a gate on starting work, and an unreadable
// ancestry is not evidence that the workflow stopped. Any other read error is
// returned.
func StoppedWorkflowAncestor(store beads.Store, bead beads.Bead) (WorkflowStop, error) {
	if store == nil {
		return WorkflowStop{}, nil
	}
	current := bead
	visited := map[string]bool{bead.ID: true}
	for hop := 0; hop < maxWorkflowStopHops; hop++ {
		root := current
		rootID := strings.TrimSpace(current.Metadata[beadmeta.RootBeadIDMetadataKey])
		switch {
		case rootID == "" || rootID == current.ID:
			// Only the gated bead itself may be a root here (a drain item root
			// handed out whole); a drain control always names the root it
			// belongs to, so one that does not ends the chain.
			if hop > 0 {
				return WorkflowStop{}, nil
			}
		case visited[rootID]:
			return WorkflowStop{}, nil
		default:
			visited[rootID] = true
			loaded, found, err := getWorkflowLink(store, rootID)
			if err != nil || !found {
				return WorkflowStop{}, err
			}
			root = loaded
			if stop, stopped := workflowRootStop(root); stopped {
				return stop, nil
			}
		}
		controlID := strings.TrimSpace(root.Metadata[beadmeta.DrainControlIDMetadataKey])
		if controlID == "" || visited[controlID] {
			return WorkflowStop{}, nil
		}
		visited[controlID] = true
		control, found, err := getWorkflowLink(store, controlID)
		if err != nil || !found {
			return WorkflowStop{}, err
		}
		current = control
	}
	return WorkflowStop{}, nil
}

// workflowRootStop classifies one root on the chain, with the dispatcher's
// control-gate semantics: cancellation is gc.outcome=canceled or the
// gc.cancel_requested marker run cancel stamps, and any closure is settlement.
func workflowRootStop(root beads.Bead) (WorkflowStop, bool) {
	if strings.TrimSpace(root.Metadata[beadmeta.OutcomeMetadataKey]) == beadmeta.OutcomeCanceled ||
		strings.TrimSpace(root.Metadata[beadmeta.CancelRequestedMetadataKey]) != "" {
		return WorkflowStop{RootID: root.ID, Canceled: true}, true
	}
	if root.Status == "closed" {
		return WorkflowStop{RootID: root.ID}, true
	}
	return WorkflowStop{}, false
}

func getWorkflowLink(store beads.Store, id string) (beads.Bead, bool, error) {
	b, err := store.Get(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, false, nil
		}
		return beads.Bead{}, false, fmt.Errorf("loading workflow ancestor %s: %w", id, err)
	}
	return b, true, nil
}

// IsTeardownTailMember reports whether bead belongs to its workflow's teardown
// tail as TeardownTailExclusion defines it. The cheap arm answers for a bead
// that carries gc.scope_role=teardown itself; an attempt linked to a teardown
// step only by gc.step_id needs the subtree read.
func IsTeardownTailMember(store beads.Store, bead beads.Bead) (bool, error) {
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
