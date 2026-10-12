package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// CloseAll over issueops.BatchApplier.
//
// The per-bead route this replaces spends THREE round trips per bead and both
// its arms cost the same: a status read, then either the metadata patch and the
// close or Close's own read and the close. Measured against a real bd serve
// that is 24 requests for 8 beads (ga-6gv08's wire-cost gate), paid serially,
// and the session wait store closes a whole batch inside one reconcile — so the
// count is the workflow's batch size, not a constant. At 20ms that is 60ms of
// pure link per bead.
//
// A close batch is exactly what issueops.BatchApplier is for: one request whose
// items apply inside one transaction, all or nothing. The metadata stamp and
// the close ride it as an update item followed by a close item, the shape
// runTxAsBatch already records for the reconciler's own close sites.
//
// # What the per-bead reads were buying, and where each answer comes from now
//
// THE CLOSE REASON. gc stamps close_reason into metadata and the close carries
// it, so the reason was read back off the row — the read this route cannot
// make, because the batch is composed before it is dialed. It does not need to:
// the reason a CloseAll has is the one it is STAMPING, and it holds it in hand.
// A CloseAll with no metadata carries no reason and closes without one, which
// is what BdStore.CloseAll — the same interface method on the other production
// store — has always done on its batch path. gc reads a bead's close reason
// from its METADATA (nativeCloseReasonFromIssue), never from the close column,
// and the column's other consumer is a server-side close validator, which the
// stamped arm satisfies with the shared reason. Store.Close is unchanged and
// still reads: it closes ONE bead, has no metadata to stamp, and its read is
// the only round trip in it.
//
// THE ALREADY-CLOSED SKIP. A close item on a closed row is a no-op — first
// close wins, CloseItem's own contract — and the result reports Changed false,
// so the count of beads this call actually closed is read off the response
// instead of predicted from a pre-read. That is a BETTER answer than the loop's:
// the loop's read and its close are two acts with a gap between them, and a row
// closed inside that gap was counted anyway.
//
// THE NOT-FOUND VERDICT. A close item naming a row that does not exist refuses
// the whole request with issueops.ErrNotFound inside an *issueops.ItemError,
// which is classified back to this package's ErrNotFound below so a caller's
// errors.Is arm holds unchanged.
//
// # The two semantics that MOVE, stated rather than buried
//
// ATOMICITY PER CHUNK. A CloseAll was never atomic — the loop closed a prefix
// and returned the failure — and now each chunk is. Strictly more atomic than
// before, never less, and a caller that could tolerate a prefix can tolerate a
// shorter one.
//
// THE STAMP REACHES ALREADY-CLOSED ROWS. The batch has no status read, so it
// stamps every id the caller named, closed or not. The per-bead route a
// backing that cannot batch still takes (closeAllOneAtATime) stamps an
// already-closed row too, so the same call does not mean two things in this
// store depending on the backing. BdStore.CloseAll does the same (its
// setMetadataBatchAll runs over every id before the batch close); MemStore,
// FileStore and SQLiteStore read each status first and skip a closed row.
// Store.CloseAll leaves that difference open, so a caller that must not touch
// a finished bead's metadata drops its closed ids before calling. Molecule
// cleanup does, and so do the workflow skip and delete paths, which list with
// IncludeClosed and would otherwise rewrite a passed step's gc.outcome and
// close_reason to the skip (cmd/gc closeOpenWorkflowBeads, the API's workflow
// delete handlers). A row that closes between that filter and this write is
// still stamped: the caller named it while it was open, so it is not a row
// whose metadata the caller meant to leave alone.
//
// WHY THE CHUNK IS NOT FURTHER SPLIT TO RECOVER MAIN'S EXACT PREFIX, now that
// this has been asked (ga-opus-g12 review, 2026-10): a bad id anywhere in a
// chunk of up to issueops.MaxApplyBatchItems now costs the whole chunk, where
// main's per-item loop only ever cost the ids from the bad one onward. Closing
// that gap with a per-item retry-on-failure fallback was tried and reverted,
// because the fallback has to read every bead in the chunk to find out where
// the "prefix" ends, and that read is the EXACT cost this file exists to
// remove — TestCloseAllDoesNotFallBackOnceAChunkHasLanded and
// closeBatchSpy (both in this package's tests) already pin ZERO per-bead reads
// on the batch route's error arm, for the same reason CloseAll's own
// batchRouteUnavailable fallback is gated on `closed == 0`: re-walking a
// backing that already committed part of the input is the one thing a caller
// counting on this route's cost cannot have happen silently. A caller that
// needs the lost prefix back today has two ways to get it at the SAME cost
// main paid: a smaller caller-chosen chunk size (trading round trips for
// precision, which is the caller's call to make, not this route's), or a
// retry over just the ids CloseAll did not report as closed. Shrinking the
// built-in chunk size itself remains open if a real workload needs it; no
// caller measured against this one has, so the chunk stays at the cap.

// nativeCloseAllProvenance labels the history entry the batch records, the way
// the per-bead route's writes were labeled by the operations themselves.
const nativeCloseAllProvenance = "gc: close all"

// batchRouteUnavailable reports a refusal that means the BACKING cannot apply a
// batch, as opposed to one that means this batch failed.
//
// It is the whole of the route decision, so what it misses is a CloseAll that
// hard-fails where the per-bead loop would have worked. The one shape that
// answers true is the portable *beadslib.ErrUnsupported: every backend this
// package can build against on gascity main answers that way, so there is no
// embedder-registered second shape to classify here. (A served backend that
// answers in its own wire-specific refusal shape instead is a capability a
// private distribution would have to teach this function about from outside
// the package; none does on this tree.)
func batchRouteUnavailable(err error) bool {
	if err == nil {
		return false
	}
	var unsupported *beadslib.ErrUnsupported
	return errors.As(err, &unsupported)
}

// closeAllAsBatch closes ids in as few requests as the applier's item cap
// allows, and reports how many of them this call actually closed.
func (s *NativeDoltStore) closeAllAsBatch(ids []string, metadata map[string]string) (int, error) {
	patch, err := nativeIssuePatchFromUpdateOpts(UpdateOpts{Metadata: metadata})
	if err != nil {
		return 0, err
	}
	stamps := !nativeIssuePatchIsEmpty(patch)

	storage, release, err := s.acquireStorage()
	if err != nil {
		return 0, err
	}
	defer release()
	applier, err := storage.BatchApplier()
	if err != nil {
		return 0, err
	}

	// The reason the stamp is about to write. Trimmed the way every other close
	// site trims it, so a whitespace-only value is the absent one.
	reason := strings.TrimSpace(metadata["close_reason"])
	closed := 0
	for _, chunk := range nativeCloseAllChunks(ids, stamps) {
		items := make([]issueops.ApplyItem, 0, len(chunk)*nativeCloseAllItemsPerBead(stamps))
		for _, id := range chunk {
			if stamps {
				items = append(items, issueops.ApplyItem{
					Kind:   issueops.ItemUpdate,
					Update: &issueops.UpdateItem{Target: issueops.Ref{ID: id}, Patch: patch},
				})
			}
			items = append(items, issueops.ApplyItem{
				Kind: issueops.ItemClose,
				Close: &issueops.CloseItem{
					Target: issueops.Ref{ID: id},
					Reason: reason,
					// Force mirrors both routes it replaces: the storage-layer
					// close applies no close policy, and a molecule root
					// routinely closes over open children.
					Force: true,
				},
			})
		}
		result, err := s.applyCloseChunk(applier, items)
		if err != nil {
			return closed, nativeCloseAllError(err)
		}
		for _, item := range result.Items {
			if item.Kind == issueops.ItemClose && item.Changed {
				closed++
			}
		}
	}
	return closed, nil
}

// applyCloseChunk dials one chunk under its OWN operation budget, retrying the
// whole chunk when it loses a serialization race.
//
// The budget is per chunk because a chunk is one request and one transaction —
// the unit the per-operation timeout was written for. One deadline spanning
// every chunk would leave each further chunk less of it, so a large CloseAll
// would fail on its size rather than on any request being slow.
//
// The chunk is also the retry unit, for the same reason it is the budget unit:
// a conflicted transaction commits nothing, so the stamp and the close it pairs
// replay together. Retrying a leg of it would be the incoherent choice — that
// asymmetry is what the per-bead route this replaced had to guard against.
func (s *NativeDoltStore) applyCloseChunk(applier issueops.BatchApplier, items []issueops.ApplyItem) (issueops.ApplyBatchResult, error) {
	var result issueops.ApplyBatchResult
	err := retryOnNativeDoltSerializationConflict(func() error {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		var attemptErr error
		result, attemptErr = applier.ApplyBatch(ctx, issueops.ApplyBatchRequest{
			Actor:      s.actor,
			Items:      items,
			Provenance: nativeCloseAllProvenance,
		})
		return attemptErr
	})
	return result, err
}

// nativeCloseAllItemsPerBead is how many apply items one bead costs: the close,
// plus the metadata stamp when there is one.
func nativeCloseAllItemsPerBead(stamps bool) int {
	if stamps {
		return 2
	}
	return 1
}

// nativeCloseAllChunks splits ids so no request exceeds the role's item cap.
//
// It chunks at the CAP rather than at some smaller round number, because the
// cap is what the role and the wire both publish and a chunk is the atomic
// unit: a smaller one would buy nothing and hand back less atomicity than the
// backend offers. A bead's items are never split across two requests, so the
// stamp and the close it feeds always land together.
func nativeCloseAllChunks(ids []string, stamps bool) [][]string {
	perChunk := issueops.MaxApplyBatchItems / nativeCloseAllItemsPerBead(stamps)
	chunks := make([][]string, 0, (len(ids)+perChunk-1)/perChunk)
	for start := 0; start < len(ids); start += perChunk {
		end := start + perChunk
		if end > len(ids) {
			end = len(ids)
		}
		chunks = append(chunks, ids[start:end])
	}
	return chunks
}

// nativeCloseAllError names the bead a refused batch refused on.
//
// The applier reports a missing row as issueops.ErrNotFound wrapped in an
// *issueops.ItemError that carries the id, which is more than the per-bead
// route's own read could say. Restating it as this package's ErrNotFound is
// what keeps a caller's errors.Is arm holding across the route change.
func nativeCloseAllError(err error) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, issueops.ErrNotFound) {
		return nativeStoreError("", err)
	}
	var item *issueops.ItemError
	if errors.As(err, &item) && item.IssueID != "" {
		return fmt.Errorf("bead %q: %w: %w", item.IssueID, ErrNotFound, err)
	}
	return fmt.Errorf("%w: %w", ErrNotFound, err)
}
