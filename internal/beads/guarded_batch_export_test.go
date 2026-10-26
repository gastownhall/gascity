package beads

import (
	"context"
	"errors"
	"fmt"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// NewNativeDoltStoreForGuardedBatchConformance returns a NativeDoltStore over
// an in-memory storage whose batch applier keeps the issueops.BatchApplier
// contract a guarded batch depends on, for beadstest.RunGuardedBatchConformance.
func NewNativeDoltStoreForGuardedBatchConformance() Store {
	return newNativeDoltStoreForTest(newNativeDoltGuardedBatchStorage(nativeDoltMemIssuePrefixForTest))
}

// NewProxiedStoreForGuardedBatchConformance is NewProxiedStoreForConformance
// over the same contract-keeping storage: a read-only latched native read leaf
// and a native write leaf standing in for the bd one.
func NewProxiedStoreForGuardedBatchConformance(mintPrefix string) Store {
	storage := newNativeDoltGuardedBatchStorage(mintPrefix)
	native := newNativeDoltStoreForTest(storage, WithProxiedReadOnly())
	native.idPrefix = normalizeIDPrefix(mintPrefix)
	write := newNativeDoltStoreForTest(storage)
	write.idPrefix = normalizeIDPrefix(mintPrefix)
	store, err := NewProxiedStore(native, write, PinForTest("/scope", "/scope/.beads/dolt", "beads"))
	if err != nil {
		panic("beads: building the guarded-batch proxied store: " + err.Error())
	}
	return store
}

// nativeDoltGuardedBatchStorage is the in-memory storage with a batch applier
// that keeps the parts of the issueops.BatchApplier contract the shared
// nativeDoltMemStorage applier deliberately leaves out:
//
//   - ALL OR NOTHING: a refused request restores the rows it found;
//   - an explicit create id that names a row is ErrAlreadyExists;
//   - a refusal is an *issueops.ItemError naming the item, wrapping the
//     verb's sentinel (ErrAlreadyExists, ErrVersionMismatch, ErrNotFound);
//   - each ItemResult carries the row's version after the item;
//   - a create keeps the status it names, so a row can be written closed
//     (the shared storage's create always opens the row), and stores the
//     issue's CloseReason as sent, as the real role's close_reason column
//     does.
//
// Requests are serialized, as the shared applier's are, which models the
// backing's transaction isolation.
type nativeDoltGuardedBatchStorage struct {
	*nativeDoltMemStorage
}

func newNativeDoltGuardedBatchStorage(prefix string) *nativeDoltGuardedBatchStorage {
	return &nativeDoltGuardedBatchStorage{nativeDoltMemStorage: &nativeDoltMemStorage{
		store:       &MemStore{storageOnly: true, IDPrefix: prefix, HonorExplicitIDs: true},
		issuePrefix: prefix,
	}}
}

// BatchApplier returns the contract-keeping applier.
func (s *nativeDoltGuardedBatchStorage) BatchApplier() (issueops.BatchApplier, error) {
	return nativeDoltGuardedBatchApplier{storage: s.nativeDoltMemStorage}, nil
}

type nativeDoltGuardedBatchApplier struct{ storage *nativeDoltMemStorage }

func (a nativeDoltGuardedBatchApplier) ApplyBatch(ctx context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	if len(req.Items) == 0 || len(req.Items) > issueops.MaxApplyBatchItems {
		return issueops.ApplyBatchResult{}, fmt.Errorf("%w: %d items", issueops.ErrValidation, len(req.Items))
	}
	a.storage.batchMu.Lock()
	defer a.storage.batchMu.Unlock()
	a.storage.store.mu.Lock()
	seq, rows, deps := a.storage.store.snapshot()
	a.storage.store.mu.Unlock()
	result, err := a.apply(ctx, req)
	if err != nil {
		a.storage.store.restoreFrom(seq, rows, deps)
		return issueops.ApplyBatchResult{}, err
	}
	return result, nil
}

func (a nativeDoltGuardedBatchApplier) apply(ctx context.Context, req issueops.ApplyBatchRequest) (issueops.ApplyBatchResult, error) {
	lifecycle, err := a.storage.IssueLifecycle()
	if err != nil {
		return issueops.ApplyBatchResult{}, err
	}
	result := issueops.ApplyBatchResult{Keys: map[string]string{}, Items: make([]issueops.ItemResult, 0, len(req.Items))}
	for i, item := range req.Items {
		switch item.Kind {
		case issueops.ItemCreate:
			id := item.Create.Issue.ID
			if _, err := a.storage.store.Get(id); err == nil {
				return issueops.ApplyBatchResult{}, &issueops.ItemError{Index: i, Kind: item.Kind, Err: issueops.ErrAlreadyExists}
			}
			created, err := lifecycle.Create(ctx, issueops.CreateRequest{Actor: req.Actor, Issue: item.Create.Issue, ForceIDPrefix: req.ForceIDPrefix})
			if err != nil {
				return issueops.ApplyBatchResult{}, &issueops.ItemError{Index: i, Kind: item.Kind, Err: err}
			}
			a.storage.store.mu.Lock()
			if idx := a.storage.store.indexOfLocked(id); idx >= 0 {
				if item.Create.Issue.Status == beadslib.StatusClosed {
					a.storage.store.beads[idx].Status = "closed"
				}
				a.storage.store.beads[idx].CloseReason = item.Create.Issue.CloseReason
			}
			a.storage.store.mu.Unlock()
			result.Items = append(result.Items, issueops.ItemResult{
				Kind: item.Kind, IssueID: created.Issue.ID, Changed: true, RowVersion: created.Issue.RowVersion,
			})
		case issueops.ItemUpdate:
			id := item.Update.Target.ID
			updated, err := lifecycle.Update(ctx, issueops.UpdateRequest{
				Actor:                 req.Actor,
				IssueID:               id,
				Patch:                 item.Update.Patch,
				ExpectedVersion:       item.Update.ExpectedVersion,
				ForceClosePolicy:      item.Update.ForceClosePolicy,
				ForceAssigneeTransfer: item.Update.ForceAssigneeTransfer,
			})
			if errors.Is(err, ErrNotFound) || (err != nil && nativeUpstreamNotFound(err)) {
				err = fmt.Errorf("%w: %w", issueops.ErrNotFound, err)
			}
			if err != nil {
				return issueops.ApplyBatchResult{}, &issueops.ItemError{Index: i, Kind: item.Kind, IssueID: id, Err: err}
			}
			result.Items = append(result.Items, issueops.ItemResult{
				Kind: item.Kind, IssueID: id, Changed: updated.Changed, RowVersion: updated.Issue.RowVersion,
			})
		default:
			return issueops.ApplyBatchResult{}, fmt.Errorf("%w: guarded-batch double applies no %q items", issueops.ErrValidation, item.Kind)
		}
	}
	return result, nil
}
