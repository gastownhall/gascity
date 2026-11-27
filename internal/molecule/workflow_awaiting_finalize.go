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
// beadmeta.WorkflowTopologyKinds has closed and no member keeps the work alive
// (WorkflowHasLiveWork). A root with no members, or with no closed worker
// member, is not finished: a root-only or fresh workflow still carries its own
// work.
//
// The hook's crash-recovery gate answers the same question in shell
// (config's gc_workflow_work_done); the two must move together.
func WorkflowAwaitingFinalize(root beads.Bead, members []beads.Bead) bool {
	if strings.TrimSpace(root.Metadata[beadmeta.KindMetadataKey]) != beadmeta.KindWorkflow || beadClosed(root) {
		return false
	}
	return workflowWorkRan(root, members) && !WorkflowHasLiveWork(root, members)
}

// WorkflowHasLiveWork reports whether members hold a member of root that keeps
// its work alive: one that is not closed and whose kind is outside
// beadmeta.AwaitingFinalizeInertKinds. It is the liveness half of
// WorkflowAwaitingFinalize, and it needs only the members that are not closed,
// so a caller can settle a live root before it reads any closed member. The
// root's own row and rows of other roots are ignored.
func WorkflowHasLiveWork(root beads.Bead, members []beads.Bead) bool {
	for _, m := range members {
		if isWorkflowMember(root, m) && !beadClosed(m) &&
			!slices.Contains(beadmeta.AwaitingFinalizeInertKinds, memberKind(m)) {
			return true
		}
	}
	return false
}

// workflowWorkRan reports whether some member of root outside
// beadmeta.WorkflowTopologyKinds has closed: the evidence that a worker step
// ran.
func workflowWorkRan(root beads.Bead, members []beads.Bead) bool {
	for _, m := range members {
		if isWorkflowMember(root, m) && beadClosed(m) &&
			!slices.Contains(beadmeta.WorkflowTopologyKinds, memberKind(m)) {
			return true
		}
	}
	return false
}

// isWorkflowMember reports whether m is a member of root other than the root
// itself.
func isWorkflowMember(root, m beads.Bead) bool {
	return m.ID != root.ID && strings.TrimSpace(m.Metadata[beadmeta.RootBeadIDMetadataKey]) == root.ID
}

func memberKind(m beads.Bead) string {
	return strings.TrimSpace(m.Metadata[beadmeta.KindMetadataKey])
}

func beadClosed(b beads.Bead) bool {
	return strings.EqualFold(strings.TrimSpace(b.Status), "closed")
}
