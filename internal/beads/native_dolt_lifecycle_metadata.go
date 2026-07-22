package beads

import (
	"context"
	"errors"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

// WithLifecycleMetadataTransaction serializes one bead's lifecycle metadata
// callback across every NativeDoltStore handle opened for the same scope. Each
// mutation remains its own native transaction so an earlier successful write
// stays durable when a later operation or the callback returns an error.
func (s *NativeDoltStore) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	if s == nil {
		return errors.New("lifecycle metadata transaction: nil NativeDoltStore")
	}
	if fn == nil {
		return errors.New("lifecycle metadata transaction: nil callback")
	}
	unlock, err := lockLifecycleMetadata(s.closeTransitionScopeKey(), id)
	if err != nil {
		return err
	}
	defer unlock()
	return fn(nativeDoltLifecycleMetadataTransaction{store: s, id: id})
}

type nativeDoltLifecycleMetadataTransaction struct {
	store *NativeDoltStore
	id    string
}

func (tx nativeDoltLifecycleMetadataTransaction) Get() (Bead, error) {
	return tx.GetByID(tx.id)
}

func (tx nativeDoltLifecycleMetadataTransaction) GetByID(id string) (Bead, error) {
	storage, release, err := tx.store.acquireStorage()
	if err != nil {
		return Bead{}, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	var out Bead
	err = storage.RunInTransaction(ctx, "", func(nativeTx beadslib.Transaction) error {
		out, err = nativeUpdateSnapshot(ctx, nativeTx, id)
		return err
	})
	if err != nil {
		return Bead{}, nativeStoreError(id, err)
	}
	return out, nil
}

func (tx nativeDoltLifecycleMetadataTransaction) List(query ListQuery) ([]Bead, error) {
	query.Live = true
	query.TierMode = TierBoth
	return tx.store.List(query)
}

func (tx nativeDoltLifecycleMetadataTransaction) SetMetadata(key, value string) error {
	return tx.SetMetadataBatch(map[string]string{key: value})
}

func (tx nativeDoltLifecycleMetadataTransaction) SetMetadataBatch(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	storage, release, err := tx.store.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	err = storage.RunInTransaction(ctx, fmt.Sprintf("gc: update lifecycle metadata for bead %s", tx.id), func(nativeTx beadslib.Transaction) error {
		return tx.store.applySetMetadataBatchInTx(ctx, nativeTx, tx.id, values)
	})
	return nativeStoreError(tx.id, err)
}

func (tx nativeDoltLifecycleMetadataTransaction) CloseWithReasonWithoutObserver(reason string) (LifecycleCloseResult, error) {
	transition, err := tx.store.closeWithReasonIfOpenWithoutScopeLock(tx.id, reason)
	return LifecycleCloseResult{
		Before:         transition.Before,
		After:          transition.After,
		Transitioned:   transition.Transitioned,
		CloseSucceeded: err == nil && transition.Transitioned,
	}, err
}

var (
	_ LifecycleMetadataTransactionStore = (*NativeDoltStore)(nil)
	_ LifecycleCloseTransaction         = nativeDoltLifecycleMetadataTransaction{}
	_ LifecycleReadTransaction          = nativeDoltLifecycleMetadataTransaction{}
)
