package beads

import (
	"fmt"
	"math"
	"strings"
)

// The file store takes rows with their ids kept, as the reverse migration
// writes them into a file-backed work or rig store (S5 §3.2). A file store
// fences no id namespace, so the capability exempts nothing: it is the
// verbatim create a copy needs, which Create is not (Create mints a fresh
// status and creation time).
var (
	_ ForeignIDCreator      = (*FileStore)(nil)
	_ ForeignIDBatchCreator = (*FileStore)(nil)
)

// CreateWithForeignID creates b under its own id, keeping its status, its
// timestamps and every field, and flushes it to disk. It satisfies
// ForeignIDCreator.
func (fs *FileStore) CreateWithForeignID(b Bead) (Bead, error) {
	if err := fs.CreateWithForeignIDBatch([]Bead{b}); err != nil {
		return Bead{}, err
	}
	return fs.Get(b.ID)
}

// CreateWithForeignIDBatch creates rows under their own ids, all or nothing:
// one reload, one check that every id is new, one flush. It satisfies
// ForeignIDBatchCreator.
func (fs *FileStore) CreateWithForeignIDBatch(rows []Bead) error {
	if len(rows) == 0 {
		return nil
	}
	fs.fmu.Lock()
	defer fs.fmu.Unlock()
	if err := fs.locker.Lock(); err != nil {
		return err
	}
	defer fs.locker.Unlock() //nolint:errcheck // best-effort unlock
	if err := fs.reloadFromDisk(); err != nil {
		return err
	}
	snap := fs.snapshotLocked()
	fs.mu.Lock()
	err := fs.insertVerbatimLocked(rows)
	fs.mu.Unlock()
	if err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// insertVerbatimLocked stores rows under their ids, keeping status, creation
// and update times (a zero creation time is stamped now) and every field;
// the edges are the caller's to add. A pinned "<prefix>-<n>" consumes its
// suffix, as Create does. Every id is checked before any row is stored, so a
// refusal stores nothing. The caller holds m.mu.
func (m *MemStore) insertVerbatimLocked(rows []Bead) error {
	seen := make(map[string]bool, len(rows))
	for _, b := range rows {
		id := strings.TrimSpace(b.ID)
		if id == "" {
			return fmt.Errorf("creating bead with foreign id: empty id")
		}
		if seen[id] || m.beadExistsLocked(id) {
			return fmt.Errorf("creating bead %q: duplicate id", id)
		}
		seen[id] = true
	}
	prefix := m.IDPrefix
	if prefix == "" {
		prefix = "gc"
	}
	for _, b := range rows {
		if n, ok := parseSQLiteAutoIDSuffix(prefix, b.ID); ok && n > 0 && n <= math.MaxInt && n > int64(m.seq) {
			m.seq = int(n)
		}
		if b.Status == "" {
			b.Status = "open"
		}
		if b.Type == "" {
			b.Type = "task"
		}
		if b.CreatedAt.IsZero() {
			b.CreatedAt = m.nextCreatedAtLocked()
		} else if b.CreatedAt.After(m.lastCreatedAt) {
			m.lastCreatedAt = b.CreatedAt
		}
		if b.UpdatedAt.IsZero() {
			b.UpdatedAt = b.CreatedAt
		}
		b.Dependencies, b.Needs, b.IsBlocked = nil, nil, nil
		b.Revision = 1
		b.ClaimFence = 0
		m.beads = append(m.beads, cloneBead(b))
	}
	return nil
}
