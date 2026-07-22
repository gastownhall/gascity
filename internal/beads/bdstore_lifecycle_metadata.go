package beads

import "errors"

// WithLifecycleMetadataTransaction serializes a metadata-only callback for one
// bead across BdStore handles and gc processes sharing the same beads scope.
func (s *BdStore) WithLifecycleMetadataTransaction(id string, fn func(LifecycleMetadataTransaction) error) error {
	if s == nil {
		return errors.New("lifecycle metadata transaction: nil BdStore")
	}
	if fn == nil {
		return errors.New("lifecycle metadata transaction: nil callback")
	}
	lease, err := acquireLifecycleMutationLease(s.dir, inheritedLifecycleMutationFromEnv())
	if err != nil {
		return err
	}
	defer lease.Unlock()
	return fn(bdLifecycleMetadataTransaction{
		store:      s,
		id:         id,
		commandEnv: lease.CommandEnv(),
	})
}

type bdLifecycleMetadataTransaction struct {
	store      *BdStore
	id         string
	commandEnv map[string]string
}

func (tx bdLifecycleMetadataTransaction) Get() (Bead, error) {
	return tx.GetByID(tx.id)
}

func (tx bdLifecycleMetadataTransaction) GetByID(id string) (Bead, error) {
	// Classify the transaction's own bead from the canonical cross-tier row (bd
	// query + dependency hydration), not bd show. In the documented bd 1.1
	// duplicate-row state a stale issues row can disagree with the wisp the
	// mutation actually targets, so bd show would misclassify the close
	// before/after status and its dependency edges. Other beads keep the ordinary
	// issues-first Get.
	if id == tx.id {
		return tx.store.readCanonicalTransitionSnapshot(id)
	}
	return tx.store.Get(id)
}

func (tx bdLifecycleMetadataTransaction) List(query ListQuery) ([]Bead, error) {
	query.Live = true
	query.TierMode = TierBoth
	return tx.store.List(query)
}

func (tx bdLifecycleMetadataTransaction) SetMetadata(key, value string) error {
	return tx.store.setMetadataWithEnv(tx.id, key, value, tx.commandEnv)
}

func (tx bdLifecycleMetadataTransaction) SetMetadataBatch(values map[string]string) error {
	return tx.store.setMetadataBatchWithEnv(tx.id, values, tx.commandEnv)
}

func (tx bdLifecycleMetadataTransaction) CloseWithReasonWithoutObserver(reason string) (LifecycleCloseResult, error) {
	return closeLifecycleMetadataTransaction(tx, func() (CloseObserverDelivery, error) {
		return nil, tx.store.closeWithoutScopeLock(tx.id, reason, tx.commandEnv)
	})
}

var (
	_ LifecycleMetadataTransactionStore = (*BdStore)(nil)
	_ LifecycleCloseTransaction         = bdLifecycleMetadataTransaction{}
	_ LifecycleReadTransaction          = bdLifecycleMetadataTransaction{}
)
