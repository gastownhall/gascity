package beads

import (
	"context"
	"errors"
)

var _ GuardedBatcherHandleProvider = (*CachingStore)(nil)

// GuardedBatcherHandle exposes guarded batches through the cache only when its
// backing can apply them. It asks the backing itself, not the backing's
// conditional-writes resolve target: see GuardedBatcherFor. The handle is the
// cache's own forwarder, so a batch applied through it keeps the cache current
// and notifies each changed row once.
//
// Each notification types its row as a Tx's do, from the row read back rather
// than the item that wrote it: a row the batch left closed is bead.closed
// unless its close was already announced, and any other changed row is
// bead.updated. A created row is therefore never announced as bead.created, and
// one created closed is announced only as bead.closed. A consumer that must
// tell creates apart reads them from the batch it applied.
func (c *CachingStore) GuardedBatcherHandle() (GuardedBatcher, bool) {
	backing, ok := GuardedBatcherFor(c.backing)
	if !ok {
		return nil, false
	}
	return cachingGuardedBatcher{cache: c, backing: backing}, true
}

// cachingGuardedBatcher forwards a batch to the backing batcher, then refreshes
// every row the batch touched exactly as Tx does (refreshTxTouchedBeads): each
// row is re-read from the backing and installed verbatim, fenced against any
// newer local write, and announced once, typed as GuardedBatcherHandle says.
type cachingGuardedBatcher struct {
	cache   *CachingStore
	backing GuardedBatcher
}

func (g cachingGuardedBatcher) ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	startSeq := g.cache.currentMutationSeq()
	res, err := g.backing.ApplyGuarded(ctx, b)
	if err != nil {
		g.forgetAfterFailure(b, err)
		return GuardedResult{}, err
	}
	ids, closed := guardedBatchTouched(b)
	g.cache.refreshTxTouchedBeads(ids, closed, startSeq)
	return res, nil
}

// forgetAfterFailure keeps a refused or failed batch from leaving a stale row
// behind. An item refusal names the one row the backing disagreed about: a
// missed guard evicts it, so the caller's re-read reaches the backing. A
// refusal raised before any write (validation, no capability) touched
// nothing. Any other failure may or may not have committed, so every row the
// batch named is marked dirty.
func (g cachingGuardedBatcher) forgetAfterFailure(b GuardedBatch, err error) {
	var itemErr *BatchItemError
	switch {
	case errors.As(err, &itemErr):
		g.cache.applyConditionalWriteFailure(itemErr.ID, itemErr.Err)
	case errors.Is(err, ErrInvalidGuardedBatch), errors.Is(err, ErrGuardedBatchUnsupported):
	default:
		ids, _ := guardedBatchTouched(b)
		for _, id := range ids {
			g.cache.applyConditionalWriteFailure(id, err)
		}
	}
}
