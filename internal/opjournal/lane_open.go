package opjournal

import (
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// HasOpenLaneOps reports whether conversation holds an open op that a lane
// executes, with one keyed read of at most one row (gc.op.open =
// conversation and gc.op.executor = lane). Like HasSubmittedOps it needs no
// conditional-write capability or namespace, so a caller that usually has
// nothing to do, such as the in-band hook pass on every prompt (DES-O1 §8),
// learns so with one read and opens the lane store only when there is work.
// A failed read is returned, never read as "no ops".
func HasOpenLaneOps(store beads.SessionStore, conversation string) (bool, error) {
	if isNilStore(store.Store) {
		return false, errors.New("probing for open lane ops: store is required")
	}
	if err := validateConversation(conversation); err != nil {
		return false, err
	}
	rows, err := store.List(beads.ListQuery{
		Metadata: map[string]string{keyOpen: conversation, keyExecutor: string(ExecutorLane)},
		Limit:    1,
		TierMode: beads.TierBoth,
	})
	if err != nil {
		return false, fmt.Errorf("probing conversation %s for open lane ops: %w", conversation, err)
	}
	return len(rows) > 0, nil
}
