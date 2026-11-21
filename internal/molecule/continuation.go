package molecule

import (
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// ReserveContinuation assigns an observed open, unowned continuation member
// without changing its status. The revision fence preserves the observed root,
// group and owner; a concurrent change returns false without a write. Stores
// without conditional writes fail closed.
func ReserveContinuation(store beads.MoleculeTarget, observed beads.Bead, assignee string) (bool, error) {
	if observed.Status != "open" || strings.TrimSpace(observed.Assignee) != "" ||
		strings.TrimSpace(observed.Metadata[beadmeta.RootBeadIDMetadataKey]) == "" ||
		strings.TrimSpace(observed.Metadata[beadmeta.ContinuationGroupMetadataKey]) == "" ||
		beadmeta.HasDispatchHold(observed.Labels) || beadmeta.IsControlKind(observed.Metadata[beadmeta.KindMetadataKey]) {
		return false, nil
	}
	writer, ok := beads.As[beads.ConditionalWriter](store)
	if !ok {
		return false, fmt.Errorf("reserving continuation %s: %w", observed.ID, beads.ErrConditionalWriteUnsupported)
	}
	if strings.TrimSpace(assignee) == "" {
		return false, fmt.Errorf("reserving continuation %s: assignee is required", observed.ID)
	}
	if err := writer.UpdateIfMatch(observed.ID, observed.Revision, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		if beads.IsPreconditionFailed(err) {
			return false, nil
		}
		return false, fmt.Errorf("reserving continuation %s: %w", observed.ID, err)
	}
	return true, nil
}
