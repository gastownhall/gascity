// Package workflowstop answers whether a bead's workflow has stopped: the
// nearest root on its workflow ancestry that has settled or been canceled.
//
// It reads only beads and their workflow metadata, so every layer that gates
// new execution on a stopped workflow (the molecule stop gate, native input
// admission) shares one walk without depending on the formula layer.
package workflowstop

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// maxHops bounds the ancestor walk in StoppedWorkflowAncestor. Each hop climbs
// one workflow level (step -> root -> drain control -> parent root), and real
// drain nesting is a handful of levels deep, so a chain this long is a corrupt
// or cyclic link rather than a workflow.
const maxHops = 16

// Stop names the workflow root that makes a bead's work invalid: the nearest
// root on its ancestor chain that has settled or been canceled. The zero value
// means the chain is live.
type Stop struct {
	// RootID is the stopped root.
	RootID string
	// Canceled reports that the root was canceled (gc.outcome=canceled or the
	// gc.cancel_requested intent marker) rather than settled some other way.
	Canceled bool
}

// Stopped reports whether a stopped root was found.
func (s Stop) Stopped() bool { return s.RootID != "" }

// Outcome is the gc.outcome a bead stranded by this stop closes with: canceled
// under a cancellation, otherwise skipped — the bead never ran to a verdict, so
// it takes the same residue outcome the finalizer's own sweep stamps.
func (s Stop) Outcome() string {
	if s.Canceled {
		return beadmeta.OutcomeCanceled
	}
	return beadmeta.OutcomeSkipped
}

// Getter reads one bead by id from the store that holds the workflow, failing
// with beads.ErrNotFound for an id it does not hold.
type Getter interface {
	Get(id string) (beads.Bead, error)
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
// maxHops: this is a gate on starting work, and an unreadable ancestry is not
// evidence that the workflow stopped. Any other read error is returned.
func StoppedWorkflowAncestor(store Getter, bead beads.Bead) (Stop, error) {
	current := bead
	visited := map[string]bool{bead.ID: true}
	for hop := 0; hop < maxHops; hop++ {
		root := current
		rootID := strings.TrimSpace(current.Metadata[beadmeta.RootBeadIDMetadataKey])
		switch {
		case rootID == "" || rootID == current.ID:
			// Only the gated bead itself may be a root here (a drain item root
			// handed out whole); a drain control always names the root it
			// belongs to, so one that does not ends the chain.
			if hop > 0 {
				return Stop{}, nil
			}
		case visited[rootID]:
			return Stop{}, nil
		default:
			visited[rootID] = true
			loaded, found, err := getLink(store, rootID)
			if err != nil || !found {
				return Stop{}, err
			}
			root = loaded
			if stop, stopped := rootStop(root); stopped {
				return stop, nil
			}
		}
		controlID := strings.TrimSpace(root.Metadata[beadmeta.DrainControlIDMetadataKey])
		if controlID == "" || visited[controlID] {
			return Stop{}, nil
		}
		visited[controlID] = true
		control, found, err := getLink(store, controlID)
		if err != nil || !found {
			return Stop{}, err
		}
		current = control
	}
	return Stop{}, nil
}

// rootStop classifies one root on the chain, with the dispatcher's
// control-gate semantics: cancellation is gc.outcome=canceled or the
// gc.cancel_requested marker run cancel stamps, and any closure is settlement.
func rootStop(root beads.Bead) (Stop, bool) {
	if strings.TrimSpace(root.Metadata[beadmeta.OutcomeMetadataKey]) == beadmeta.OutcomeCanceled ||
		strings.TrimSpace(root.Metadata[beadmeta.CancelRequestedMetadataKey]) != "" {
		return Stop{RootID: root.ID, Canceled: true}, true
	}
	if root.Status == "closed" {
		return Stop{RootID: root.ID}, true
	}
	return Stop{}, false
}

func getLink(store Getter, id string) (beads.Bead, bool, error) {
	b, err := store.Get(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, false, nil
		}
		return beads.Bead{}, false, fmt.Errorf("loading workflow ancestor %s: %w", id, err)
	}
	return b, true, nil
}
