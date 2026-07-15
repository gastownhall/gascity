package beads

import (
	"errors"
	"fmt"
)

// BatchDeleter is an optional Store capability: remove many beads by id in one
// batched operation, orphaning any external dependents rather than recursively
// deleting them. The backend's ON DELETE CASCADE drops each listed bead's own
// dependency, label, and event rows, but beads that merely depend on a deleted
// bead are preserved (their dangling edge into the deleted bead is cleaned up).
// A Store that implements it lets callers (notably the wisp GC) collapse an
// O(subprocess-per-edge) teardown of a molecule closure into a single batched
// delete on the sqlite/Dolt graph store. Callers that hold a plain Store fall
// back to per-bead deletion when the concrete store does not implement this
// interface.
//
// This is deliberately NOT dependent-recursive deletion: the wisp GC collects
// only an ownership closure and must not reach live work outside it, so the
// batch delete removes exactly the given ids — the semantics of
// `bd delete <ids...> --force`, not `bd delete --cascade`.
type BatchDeleter interface {
	// DeleteBatch removes exactly the given ids as a batch, orphaning external
	// dependents. Implementations tolerate ids that are already gone
	// (idempotent) and may chunk internally to respect backend limits. When an
	// implementation commits some ids before a later failure, it reports the
	// committed ids via a *BatchDeleteError so a caching layer can reconcile the
	// partial success instead of leaving deleted beads present-but-stale.
	DeleteBatch(ids []string) error
}

// ErrBatchDeleteUnsupported signals that a Store wrapper implements DeleteBatch
// only to forward the capability, but its backing store does not implement
// BatchDeleter. A wrapper embeds the plain Store interface, which does not
// promote optional capabilities, so callers that reach DeleteBatch through such
// a wrapper treat this sentinel as the cue to fall back to per-bead deletion —
// exactly as they would if the wrapper did not advertise BatchDeleter at all.
var ErrBatchDeleteUnsupported = errors.New("batch delete unsupported by backing store")

// BatchDeleteError reports a batch delete that aborted after durably removing
// some ids. Committed holds the ids the backing store removed before Err (for a
// chunk-committing backend, every id in the fully-applied earlier chunks), so a
// caching layer can tombstone exactly those and leave the rest resident. It is
// empty when the batch failed before committing anything.
type BatchDeleteError struct {
	Committed []string
	Err       error
}

// Error renders the underlying failure and, when a partial commit occurred, how
// many ids were already removed.
func (e *BatchDeleteError) Error() string {
	if e == nil || e.Err == nil {
		return "batch delete failed"
	}
	if len(e.Committed) == 0 {
		return e.Err.Error()
	}
	return fmt.Sprintf("%v (%d id(s) already committed)", e.Err, len(e.Committed))
}

// Unwrap exposes the underlying failure for errors.Is/errors.As.
func (e *BatchDeleteError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
