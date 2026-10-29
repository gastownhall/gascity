package beads

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
)

// VerbatimImporter is an optional Store capability: insert beads exactly as a
// backup recorded them — id, created_at and updated_at, status, tier, labels
// and metadata — rather than minting fresh ones. It is the restore half of
// `gc storage recover-stranded --purge-type`, which deletes rows from a work
// store after backing them up; putting one back under a new id or a new
// creation time would make it a different bead to every check that keys a
// bead by (id, created_at).
//
// Edges are not imported: a row's inline Dependencies are ignored, and the
// caller restores edges with DepAdd once the rows exist. The caller is also
// responsible for refusing an id the store already holds; an implementation
// may upsert (bd import does).
type VerbatimImporter interface {
	ImportVerbatim(rows []Bead) error
}

// ErrVerbatimImportUnsupported is how a wrapper reports that the store it
// wraps cannot import verbatim.
var ErrVerbatimImportUnsupported = errors.New("verbatim import unsupported by backing store")

// ImportVerbatim appends rows as they are, refusing an id the store holds.
func (m *MemStore) ImportVerbatim(rows []Bead) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, b := range rows {
		if b.ID == "" {
			return fmt.Errorf("importing a bead with no id")
		}
		if m.beadExistsLocked(b.ID) {
			return fmt.Errorf("importing bead %q: duplicate id", b.ID)
		}
	}
	added := make([]Bead, 0, len(rows))
	for _, b := range rows {
		stored := cloneBead(b)
		stored.Dependencies = nil
		stored.Needs = nil
		if stored.Revision == 0 {
			stored.Revision = 1
		}
		m.beads = append(m.beads, stored)
		added = append(added, stored)
	}
	m.seedCreatedAtLocked(added)
	return nil
}

// ImportVerbatim imports rows under the file lock and flushes once.
func (fs *FileStore) ImportVerbatim(rows []Bead) error {
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
	if err := fs.MemStore.ImportVerbatim(rows); err != nil {
		return err
	}
	if err := fs.save(); err != nil {
		fs.restoreFrom(snap.seq, snap.beads, snap.deps)
		return err
	}
	return nil
}

// ImportVerbatim runs `bd import` over the rows, which keeps every timestamp
// the JSONL carries. bd import upserts, so the caller must have refused ids
// the store already holds; a retried import is idempotent.
func (s *BdStore) ImportVerbatim(rows []Bead) error {
	if len(rows) == 0 {
		return nil
	}
	tmp, err := os.CreateTemp("", "gc-import-verbatim-*.jsonl")
	if err != nil {
		return fmt.Errorf("bd import: %w", err)
	}
	name := tmp.Name()
	defer os.Remove(name) //nolint:errcheck // temp file
	encoder := json.NewEncoder(tmp)
	for _, b := range rows {
		row := b
		row.Dependencies = nil
		row.Needs = nil
		// bd keeps the sender in metadata, as Create writes it.
		if row.From != "" && row.Metadata["from"] == "" {
			row.Metadata = maps.Clone(row.Metadata)
			if row.Metadata == nil {
				row.Metadata = map[string]string{}
			}
			row.Metadata["from"] = row.From
		}
		if err := encoder.Encode(row); err != nil {
			_ = tmp.Close()
			return fmt.Errorf("bd import: encoding %s: %w", b.ID, err)
		}
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("bd import: %w", err)
	}
	if err := s.runBDTransientWrite("import", name); err != nil {
		return fmt.Errorf("bd import of %d bead(s): %w", len(rows), err)
	}
	return nil
}

// ImportVerbatim imports on the write leaf.
func (s *ProxiedStore) ImportVerbatim(rows []Bead) error {
	importer, ok := s.writeLeaf().(VerbatimImporter)
	if !ok {
		return fmt.Errorf("proxied store: write leaf %T: %w", s.writeLeaf(), ErrVerbatimImportUnsupported)
	}
	return s.withMutation("import-verbatim", func(Store) error { return importer.ImportVerbatim(rows) })
}
