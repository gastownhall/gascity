package beads

import (
	"fmt"
	"time"
)

var (
	_ ConditionalWriter                = (*MemStore)(nil)
	_ conditionalWritesModeCarrier     = (*MemStore)(nil)
	_ conditionalWriteCapabilityProber = (*MemStore)(nil)

	// FileStore inherits the stamp and the prober through its embedded
	// *MemStore: DisableConditionalWrites is ONE field stored on the embedded
	// MemStore (FileStore's CAS shadows read the same storage through
	// promotion), so a promoted prober answers identically and FileStore
	// needs no shadow of its own.
	_ conditionalWritesModeCarrier     = (*FileStore)(nil)
	_ conditionalWriteCapabilityProber = (*FileStore)(nil)
)

// probeConditionalWriteCapability reports the instance toggle: a MemStore is
// natively capable unless DisableConditionalWrites is set (the deterministic
// auto-degrade / require-fail-closed matrix cell, §7.3).
func (m *MemStore) probeConditionalWriteCapability() (bool, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return false, "conditional writes disabled on this store instance"
	}
	return true, ""
}

// UpdateIfMatch applies opts only when the bead's current revision equals
// expectedRevision, otherwise it returns *PreconditionFailedError. When the
// instance has DisableConditionalWrites set it returns ErrConditionalWriteUnsupported.
func (m *MemStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	if isEmptyUpdateOpts(opts) {
		return fmt.Errorf("conditional update %s: %w", id, ErrEmptyConditionalUpdate)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(id)
	if i < 0 {
		return fmt.Errorf("updating bead %q: %w", id, ErrNotFound)
	}
	if m.beads[i].Revision != expectedRevision {
		return &PreconditionFailedError{ID: id, Expected: expectedRevision, Current: m.beads[i].Revision}
	}
	m.applyUpdateLocked(i, opts)
	return nil
}

// CloseIfMatch closes the bead only when its current revision equals
// expectedRevision. Closing an already-closed bead is a no-op (matching Close).
func (m *MemStore) CloseIfMatch(id string, expectedRevision int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(id)
	if i < 0 {
		return fmt.Errorf("closing bead %q: %w", id, ErrNotFound)
	}
	if m.beads[i].Revision != expectedRevision {
		return &PreconditionFailedError{ID: id, Expected: expectedRevision, Current: m.beads[i].Revision}
	}
	if m.beads[i].Status == "closed" {
		return nil
	}
	m.beads[i].Status = "closed"
	m.beads[i].UpdatedAt = time.Now()
	m.beads[i].Revision++
	return nil
}

// DeleteIfMatch removes the bead only when its current revision equals
// expectedRevision.
func (m *MemStore) DeleteIfMatch(id string, expectedRevision int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(id)
	if i < 0 {
		return fmt.Errorf("deleting bead %q: %w", id, ErrNotFound)
	}
	if m.beads[i].Revision != expectedRevision {
		return &PreconditionFailedError{ID: id, Expected: expectedRevision, Current: m.beads[i].Revision}
	}
	m.beads = append(m.beads[:i], m.beads[i+1:]...)
	return nil
}

// CompareAndSetMetadataKey atomically sets metadata[key] = next when the current
// value equals expected. expected == "" matches an absent or empty-valued key.
// Reading a key from a nil metadata map yields "", so the absent case falls out
// naturally. Returns (true, nil) on swap, (false, nil) on a genuine mismatch.
func (m *MemStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.DisableConditionalWrites {
		return false, ErrConditionalWriteUnsupported
	}
	i := m.indexOfLocked(id)
	if i < 0 {
		return false, fmt.Errorf("compare-and-set metadata on %q: %w", id, ErrNotFound)
	}
	if m.beads[i].Metadata[key] != expected {
		return false, nil
	}
	if m.beads[i].Metadata == nil {
		m.beads[i].Metadata = make(StringMap)
	}
	m.beads[i].Metadata[key] = next
	m.beads[i].UpdatedAt = time.Now()
	m.beads[i].Revision++
	return true, nil
}
