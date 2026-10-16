package beads

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

var (
	_ GuardedBatcher               = (*NativeDoltStore)(nil)
	_ GuardedBatcherHandleProvider = (*NativeDoltStore)(nil)
)

// GuardedBatcherHandle reports the store as a guarded batcher only when a call
// could apply a batch: a read-only handle (WithProxiedReadOnly, or one latched
// read-only) and a backend whose storage refuses the batch-applier role answer
// false, so a caller that requires atomic batches refuses at startup rather
// than at the first write. Any other failure to reach the storage is left to
// the call to report.
func (s *NativeDoltStore) GuardedBatcherHandle() (GuardedBatcher, bool) {
	if s.readOnlyGuard() != nil {
		return nil, false
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return s, true
	}
	defer release()
	if _, err := storage.BatchApplier(); err != nil {
		var unsupported *beadslib.ErrUnsupported
		if errors.As(err, &unsupported) {
			return nil, false
		}
	}
	return s, true
}

// nativeGuardedBatchProvenance labels a batch's history entry when the caller
// gives no reason.
const nativeGuardedBatchProvenance = "gc: guarded batch"

// ApplyGuarded applies b as ONE issueops.BatchApplier request, which the
// backing applies in one transaction it opens itself, all or nothing, on every
// route: embedded, server and served HTTP. The items map one to one:
//
//   - a create is a CreateItem whose issue carries the pinned id, which the
//     role treats as create-only (ErrAlreadyExists refuses the request);
//   - an update is an UpdateItem whose ExpectedVersion is the item's guard. A
//     status write waives the close policy and an assignee write waives the
//     transfer guard, as UpdateIfMatch does, so a close is an update to
//     status closed.
//
// The revisions are the items' ItemResult.RowVersion. Upstream label writes
// leave the row version where it was, so an update carrying labels also
// advances beadmeta.LabelRevisionMetadataKey, as UpdateIfMatch does, and its
// revision moves with its labels.
//
// Like Store.Tx, it acquires the storage once and replays the whole request
// only on a serialization conflict, which commits nothing. A connection
// failure is returned rather than replayed across a reconnect: the request may
// have committed, and a replay would refuse its own pinned creates.
func (s *NativeDoltStore) ApplyGuarded(ctx context.Context, b GuardedBatch) (GuardedResult, error) {
	if err := s.readOnlyGuard(); err != nil {
		return GuardedResult{}, err
	}
	if err := validateGuardedBatch(b); err != nil {
		return GuardedResult{}, err
	}
	if err := checkGuardedCreateNamespaces("native dolt guarded batch", b, s.reservedPrefixes); err != nil {
		return GuardedResult{}, err
	}
	for _, it := range b.Items {
		if it.Create != nil {
			noteSessionCreate(*it.Create)
		} else {
			noteSessionKeysByID(s.Get, it.Update.ID, it.Update.Opts.Metadata)
		}
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return GuardedResult{}, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(ctx)
	defer cancel()

	applier, err := storage.BatchApplier()
	if err != nil {
		var unsupported *beadslib.ErrUnsupported
		if errors.As(err, &unsupported) {
			return GuardedResult{}, fmt.Errorf("native Dolt store: %w: %w", ErrGuardedBatchUnsupported, err)
		}
		return GuardedResult{}, nativeStoreError("", err)
	}
	items, err := s.nativeGuardedItems(ctx, storage, b)
	if err != nil {
		return GuardedResult{}, err
	}
	provenance := strings.TrimSpace(b.Reason)
	if provenance == "" {
		provenance = nativeGuardedBatchProvenance
	}
	var result issueops.ApplyBatchResult
	err = retryOnNativeDoltSerializationConflict(func() error {
		var err error
		result, err = applier.ApplyBatch(ctx, issueops.ApplyBatchRequest{
			Actor:         s.actor,
			Items:         items,
			Provenance:    provenance,
			ForceIDPrefix: true,
		})
		return err
	})
	if err != nil {
		return GuardedResult{}, s.nativeGuardedBatchError(ctx, storage, b, err)
	}
	revisions, err := nativeGuardedRevisions(b, result)
	if err != nil {
		return GuardedResult{}, err
	}
	return GuardedResult{Revisions: revisions}, nil
}

// nativeGuardedRevisions maps the applied batch's item results onto its rows.
// The role reports one result per item, in request order, naming the row the
// item acted on. A result set that does not line up with the request is an
// error rather than trusted, since a revision attributed to the wrong row
// would let the caller's next guard pass against a row it never read. The
// batch has applied by then, so the error is not one of the typed refusals:
// it is the kind the GuardedBatcher contract says to re-read after.
func nativeGuardedRevisions(b GuardedBatch, result issueops.ApplyBatchResult) (map[string]int64, error) {
	if len(result.Items) != len(b.Items) {
		return nil, fmt.Errorf("native Dolt store: guarded batch applied but the backend reported %d item results for %d items", len(result.Items), len(b.Items))
	}
	revisions := make(map[string]int64, len(b.Items))
	for i, it := range b.Items {
		if got := result.Items[i].IssueID; got != it.id() {
			return nil, fmt.Errorf("native Dolt store: guarded batch applied but the backend reported item %d as %q, not %q", i, got, it.id())
		}
		revisions[it.id()] = result.Items[i].RowVersion
	}
	return revisions, nil
}

// nativeGuardedItems translates the batch into apply items. Only an update
// that carries labels reads first, for the label revision it advances; a row
// that read finds missing refuses the batch before anything is dialed.
func (s *NativeDoltStore) nativeGuardedItems(ctx context.Context, storage beadslib.Storage, b GuardedBatch) ([]issueops.ApplyItem, error) {
	items := make([]issueops.ApplyItem, 0, len(b.Items))
	for idx, it := range b.Items {
		if it.Create != nil {
			create := *it.Create
			guardedCreateCloseReason(&create)
			issue, err := nativeIssueFromBead(create)
			if err != nil {
				return nil, fmt.Errorf("guarded batch item %d (%s): %w", idx, it.Create.ID, err)
			}
			issue.RowVersion = 0
			items = append(items, issueops.ApplyItem{
				Kind:   issueops.ItemCreate,
				Create: &issueops.CreateItem{Issue: issue},
			})
			continue
		}
		u := it.Update
		opts := u.Opts
		if len(opts.Labels) > 0 || len(opts.RemoveLabels) > 0 {
			stamped, err := s.stampGuardedLabelRevision(ctx, storage, idx, u.ID, opts)
			if err != nil {
				return nil, err
			}
			opts = stamped
		}
		patch, err := nativeIssuePatchFromUpdateOpts(opts)
		if err != nil {
			return nil, fmt.Errorf("guarded batch item %d (%s): %w", idx, u.ID, err)
		}
		items = append(items, issueops.ApplyItem{
			Kind: issueops.ItemUpdate,
			Update: &issueops.UpdateItem{
				Target:                issueops.Ref{ID: u.ID},
				Patch:                 patch,
				ExpectedVersion:       u.ExpectedRevision,
				ForceClosePolicy:      opts.Status != nil,
				ForceAssigneeTransfer: opts.Assignee != nil,
			},
		})
	}
	return items, nil
}

// stampGuardedLabelRevision returns opts plus the next
// beadmeta.LabelRevisionMetadataKey value for row id.
func (s *NativeDoltStore) stampGuardedLabelRevision(ctx context.Context, storage beadslib.Storage, idx int, id string, opts UpdateOpts) (UpdateOpts, error) {
	reader, err := storage.IssueReader()
	if err != nil {
		return UpdateOpts{}, nativeStoreError(id, err)
	}
	current, err := reader.Get(ctx, issueops.GetRequest{ID: id})
	if err != nil {
		if errors.Is(nativeReadNotFound(id, err), ErrNotFound) {
			return UpdateOpts{}, guardedNotFoundError(idx, id, err)
		}
		return UpdateOpts{}, nativeStoreError(id, err)
	}
	if current == nil {
		return UpdateOpts{}, guardedNotFoundError(idx, id, nil)
	}
	opts.Metadata = maps.Clone(opts.Metadata)
	if opts.Metadata == nil {
		opts.Metadata = make(map[string]string, 1)
	}
	opts.Metadata[beadmeta.LabelRevisionMetadataKey] = nextConditionalLabelRevision(current.Metadata)
	return opts, nil
}

// nativeGuardedBatchError maps the role's refusal onto the typed batch errors.
// The role names the refusing item with *issueops.ItemError; its index is the
// batch's own, since the items map one to one.
func (s *NativeDoltStore) nativeGuardedBatchError(ctx context.Context, storage beadslib.Storage, b GuardedBatch, err error) error {
	var itemErr *issueops.ItemError
	if !errors.As(err, &itemErr) || itemErr.Index < 0 || itemErr.Index >= len(b.Items) {
		return nativeStoreError("", err)
	}
	idx, it := itemErr.Index, b.Items[itemErr.Index]
	id := it.id()
	switch {
	case errors.Is(itemErr.Err, issueops.ErrAlreadyExists):
		return guardedExistsError(idx, id, itemErr.Err)
	case errors.Is(itemErr.Err, issueops.ErrVersionMismatch):
		expected := int64(0)
		if want := it.expectedRevision(); want != nil {
			expected = *want
		}
		current := int64(0)
		if reader, readerErr := storage.IssueReader(); readerErr == nil {
			if issue, readErr := reader.Get(ctx, issueops.GetRequest{ID: id}); readErr == nil && issue != nil {
				current = issue.RowVersion
			}
		}
		return guardedPreconditionError(idx, id, expected, current, itemErr.Err.Error())
	case errors.Is(itemErr.Err, issueops.ErrNotFound), nativeUpstreamNotFound(itemErr.Err):
		return guardedNotFoundError(idx, id, itemErr.Err)
	default:
		return nativeStoreError(id, err)
	}
}
