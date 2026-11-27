package molecule

import (
	"slices"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// WorkflowAwaitingFinalize reports whether root is an open workflow root
// whose worker steps have run and which now waits only on its finalize
// control. members is the root's member read (rows carrying
// gc.root_bead_id=root.ID, closed rows included); the root's own row and rows
// of other roots are ignored.
//
// The verdict is true when at least one member outside
// beadmeta.WorkflowTopologyKinds has closed and every member that is still
// open has a kind in beadmeta.AwaitingFinalizeInertKinds. A root with no
// members, or with no closed worker member, is not finished: a root-only or
// fresh workflow still carries its own work.
func WorkflowAwaitingFinalize(root beads.Bead, members []beads.Bead) bool {
	if strings.TrimSpace(root.Metadata[beadmeta.KindMetadataKey]) != beadmeta.KindWorkflow || beadClosed(root) {
		return false
	}
	workClosed := false
	for _, m := range members {
		if m.ID == root.ID || strings.TrimSpace(m.Metadata[beadmeta.RootBeadIDMetadataKey]) != root.ID {
			continue
		}
		kind := strings.TrimSpace(m.Metadata[beadmeta.KindMetadataKey])
		if beadClosed(m) {
			if !slices.Contains(beadmeta.WorkflowTopologyKinds, kind) {
				workClosed = true
			}
			continue
		}
		if !slices.Contains(beadmeta.AwaitingFinalizeInertKinds, kind) {
			return false
		}
	}
	return workClosed
}

func beadClosed(b beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(b.Status), "closed")
}
