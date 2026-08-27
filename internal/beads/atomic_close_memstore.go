package beads

import (
	"fmt"
	"time"
)

// atomicCloseMemStore is an opt-in test store for terminal paths that require
// an atomic metadata-and-close transition. Plain MemStore intentionally does
// not expose this narrower capability.
type atomicCloseMemStore struct {
	*MemStore
}

var _ AtomicConditionalCloser = (*atomicCloseMemStore)(nil)

// NewAtomicCloseMemStore returns an in-memory Store with atomic terminal close
// support for synthetic workloads and deterministic tests.
func NewAtomicCloseMemStore() Store {
	return &atomicCloseMemStore{MemStore: NewMemStore()}
}

// CloseWithMetadataIfMatch merges metadata and closes one open bead while its
// revision remains expectedRevision. Both changes share one MemStore lock and
// one fresh revision.
func (s *atomicCloseMemStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (Bead, error) {
	if s == nil || s.MemStore == nil {
		return Bead{}, fmt.Errorf("atomic closing bead %q: store is nil", id)
	}
	return s.closeWithMetadataIfMatch(id, expectedRevision, metadata)
}

// closeWithMetadataIfMatch is the shared atomic terminal-close body: the
// metadata merge and the status flip happen under one MemStore lock and mint
// one revision, so nothing can observe or write between them.
//
// It stays unexported deliberately. TestAtomicCloseMemStoreIsOptIn pins that a
// plain MemStore does NOT advertise AtomicConditionalCloser, and that
// non-capability is what keeps session.Store.Close's non-atomic arm
// exercisable. The two callers are the opt-in atomicCloseMemStore and
// FileStore, which needs the fused mutation to complete before its single
// flush.
func (m *MemStore) closeWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (Bead, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return Bead{}, ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(id)
	if i < 0 {
		return Bead{}, fmt.Errorf("atomic closing bead %q: %w", id, ErrNotFound)
	}
	if m.beads[i].Revision != expectedRevision {
		return Bead{}, &PreconditionFailedError{ID: id, Expected: expectedRevision, Current: m.beads[i].Revision}
	}
	if m.beads[i].Metadata == nil {
		m.beads[i].Metadata = make(StringMap, len(metadata))
	}
	for key, value := range metadata {
		m.beads[i].Metadata[key] = value
	}
	m.beads[i].Status = "closed"
	m.beads[i].UpdatedAt = time.Now()
	m.beads[i].Revision++
	return cloneBead(m.beads[i]), nil
}
