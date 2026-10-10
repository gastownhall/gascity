package beads

// ga-vnycm2.32 critical-path probe (reverted in the next commit) 0
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 1
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 2
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 3
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 4
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 5
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 6
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 7
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 8
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 9
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 10
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 11
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 12
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 13
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 14
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 15
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 16
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 17
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 18
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 19
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 20
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 21
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 22
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 23
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 24
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 25
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 26
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 27
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 28
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 29
// ga-vnycm2.32 critical-path probe (reverted in the next commit) 30

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
// It stays unexported on purpose. A plain MemStore does NOT advertise
// AtomicConditionalCloser (TestAtomicCloseMemStoreIsOptIn), which is what keeps
// the non-atomic close arm of callers such as session.Store.Close exercisable.
// Its callers are the opt-in atomicCloseMemStore and FileStore, which runs it
// inside its flock / reload / save wrapper.
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
	noteSessionKeys(m.beads[i], metadata)
	if m.beads[i].Metadata == nil {
		m.beads[i].Metadata = make(StringMap, len(metadata))
	}
	for key, value := range metadata {
		m.beads[i].Metadata[key] = value
	}
	wasClosed := m.beads[i].Status == "closed"
	setBeadStatus(&m.beads[i], "closed")
	if !wasClosed {
		recordCloseReason(&m.beads[i])
	}
	m.beads[i].UpdatedAt = time.Now()
	m.beads[i].Revision++
	return cloneBead(m.beads[i]), nil
}
