package beads

import (
	"errors"
	"fmt"
	"time"
)

func (m *MemStore) withLifecycleMetadataTransaction(
	id string,
	fn func(LifecycleMetadataTransaction) error,
) error {
	if m == nil {
		return errors.New("lifecycle metadata transaction: nil MemStore")
	}
	unlockScope, err := lockLifecycleMetadata(memLifecycleMutationScope(m), id)
	if err != nil {
		return err
	}
	defer unlockScope()
	m.mu.Lock()
	defer m.mu.Unlock()
	return fn(memLifecycleMetadataTransaction{store: m, id: id})
}

type memLifecycleMetadataTransaction struct {
	store *MemStore
	id    string
}

func (tx memLifecycleMetadataTransaction) Get() (Bead, error) {
	return tx.GetByID(tx.id)
}

func (tx memLifecycleMetadataTransaction) GetByID(id string) (Bead, error) {
	b, err := tx.store.getLocked(id)
	if err != nil {
		return Bead{}, err
	}
	return tx.store.snapshotBeadWithDepsLocked(b), nil
}

func (tx memLifecycleMetadataTransaction) List(query ListQuery) ([]Bead, error) {
	return tx.store.listLocked(query)
}

func (tx memLifecycleMetadataTransaction) SetMetadata(key, value string) error {
	return tx.setMetadataBatch(map[string]string{key: value})
}

func (tx memLifecycleMetadataTransaction) SetMetadataBatch(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	return tx.setMetadataBatch(values)
}

func (tx memLifecycleMetadataTransaction) setMetadataBatch(values map[string]string) error {
	i := tx.store.indexOfLocked(tx.id)
	if i < 0 {
		return fmt.Errorf("setting lifecycle metadata on %q: %w", tx.id, ErrNotFound)
	}
	if tx.store.beads[i].Metadata == nil {
		tx.store.beads[i].Metadata = make(map[string]string, len(values))
	}
	for key, value := range values {
		tx.store.beads[i].Metadata[key] = value
	}
	tx.store.beads[i].UpdatedAt = time.Now()
	tx.store.beads[i].Revision++
	return nil
}

func (tx memLifecycleMetadataTransaction) CloseWithReasonWithoutObserver(reason string) (LifecycleCloseResult, error) {
	i := tx.store.indexOfLocked(tx.id)
	if i < 0 {
		return LifecycleCloseResult{}, fmt.Errorf("closing bead %q with reason: %w", tx.id, ErrNotFound)
	}
	before := tx.store.snapshotBeadWithDepsLocked(tx.store.beads[i])
	result := LifecycleCloseResult{Before: before}
	if before.Status == "closed" {
		result.After = before
		return result, nil
	}

	reason = closeReasonForTransition(before, reason)
	if reason != "" {
		if tx.store.beads[i].Metadata == nil {
			tx.store.beads[i].Metadata = make(map[string]string)
		}
		tx.store.beads[i].Metadata["close_reason"] = reason
	}
	tx.store.beads[i].Status = "closed"
	tx.store.beads[i].UpdatedAt = time.Now().UTC()
	tx.store.beads[i].Revision++
	result.After = tx.store.snapshotBeadWithDepsLocked(tx.store.beads[i])
	result.Transitioned = true
	result.CloseSucceeded = true
	return result, nil
}

func (fs *FileStore) withLifecycleMetadataTransaction(
	id string,
	fn func(LifecycleMetadataTransaction) error,
) error {
	if fs == nil {
		return errors.New("lifecycle metadata transaction: nil FileStore")
	}
	unlockScope, err := lockProcessLifecycleMetadata(fs.path, id)
	if err != nil {
		return err
	}
	defer unlockScope()
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // mutation result takes precedence
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	return fn(fileLifecycleMetadataTransaction{store: fs, id: id})
}

type fileLifecycleMetadataTransaction struct {
	store *FileStore
	id    string
}

func (tx fileLifecycleMetadataTransaction) Get() (Bead, error) {
	return tx.GetByID(tx.id)
}

func (tx fileLifecycleMetadataTransaction) GetByID(id string) (Bead, error) {
	return tx.store.MemStore.Get(id)
}

func (tx fileLifecycleMetadataTransaction) List(query ListQuery) ([]Bead, error) {
	return tx.store.MemStore.List(query)
}

func (tx fileLifecycleMetadataTransaction) SetMetadata(key, value string) error {
	return tx.mutateAndSave(func() error {
		return tx.store.MemStore.SetMetadata(tx.id, key, value)
	})
}

func (tx fileLifecycleMetadataTransaction) SetMetadataBatch(values map[string]string) error {
	if len(values) == 0 {
		return nil
	}
	return tx.mutateAndSave(func() error {
		return tx.store.MemStore.SetMetadataBatch(tx.id, values)
	})
}

func (tx fileLifecycleMetadataTransaction) mutateAndSave(mutate func() error) error {
	snapshot := tx.store.snapshotLocked()
	if err := mutate(); err != nil {
		return err
	}
	if err := tx.store.save(); err != nil {
		tx.store.restoreFrom(snapshot.seq, snapshot.beads, snapshot.deps)
		return err
	}
	return nil
}

func (tx fileLifecycleMetadataTransaction) CloseWithReasonWithoutObserver(reason string) (LifecycleCloseResult, error) {
	snapshot := tx.store.snapshotLocked()
	transition, err := tx.store.MemStore.CloseWithReasonIfOpen(tx.id, reason)
	if err != nil {
		return LifecycleCloseResult{}, err
	}
	result := LifecycleCloseResult{
		Before:         transition.Before,
		After:          transition.After,
		Transitioned:   transition.Transitioned,
		CloseSucceeded: true,
	}
	if !transition.Transitioned {
		return result, nil
	}
	if err := tx.store.save(); err != nil {
		tx.store.restoreFrom(snapshot.seq, snapshot.beads, snapshot.deps)
		return LifecycleCloseResult{Before: transition.Before}, err
	}
	return result, nil
}

var (
	_ LifecycleCloseTransaction = memLifecycleMetadataTransaction{}
	_ LifecycleCloseTransaction = fileLifecycleMetadataTransaction{}
	_ LifecycleReadTransaction  = memLifecycleMetadataTransaction{}
	_ LifecycleReadTransaction  = fileLifecycleMetadataTransaction{}
)
