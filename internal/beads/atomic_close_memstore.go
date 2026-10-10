// ga-vnycm2.37 measurement probe line 0 (reverted next)
// ga-vnycm2.37 measurement probe line 1 (reverted next)
// ga-vnycm2.37 measurement probe line 2 (reverted next)
// ga-vnycm2.37 measurement probe line 3 (reverted next)
// ga-vnycm2.37 measurement probe line 4 (reverted next)
// ga-vnycm2.37 measurement probe line 5 (reverted next)
// ga-vnycm2.37 measurement probe line 6 (reverted next)
// ga-vnycm2.37 measurement probe line 7 (reverted next)
// ga-vnycm2.37 measurement probe line 8 (reverted next)
// ga-vnycm2.37 measurement probe line 9 (reverted next)
// ga-vnycm2.37 measurement probe line 10 (reverted next)
// ga-vnycm2.37 measurement probe line 11 (reverted next)
// ga-vnycm2.37 measurement probe line 12 (reverted next)
// ga-vnycm2.37 measurement probe line 13 (reverted next)
// ga-vnycm2.37 measurement probe line 14 (reverted next)
// ga-vnycm2.37 measurement probe line 15 (reverted next)
// ga-vnycm2.37 measurement probe line 16 (reverted next)
// ga-vnycm2.37 measurement probe line 17 (reverted next)
// ga-vnycm2.37 measurement probe line 18 (reverted next)
// ga-vnycm2.37 measurement probe line 19 (reverted next)
// ga-vnycm2.37 measurement probe line 20 (reverted next)
// ga-vnycm2.37 measurement probe line 21 (reverted next)
// ga-vnycm2.37 measurement probe line 22 (reverted next)

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
