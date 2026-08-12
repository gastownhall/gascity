package beads

import "fmt"

// CreateWithStorage creates a bead in an explicitly selected physical storage
// tier, satisfying [StorageCreateStore]. It is the single-create analog of
// ApplyGraphPlanWithStorage: it maps the StorageClass onto the bead's tier fields
// (which upsertBeadTx persists as the `tier` column / JSON) and delegates to
// Create. StorageDefault leaves the bead's tier fields untouched.
func (s *SQLiteStore) CreateWithStorage(b Bead, storage StorageClass) (Bead, error) {
	switch storage {
	case StorageEphemeral:
		b.Ephemeral, b.NoHistory = true, false
	case StorageNoHistory:
		b.Ephemeral, b.NoHistory = false, true
	case StorageHistory:
		b.Ephemeral, b.NoHistory = false, false
	case StorageDefault:
		// leave the bead's existing tier fields as-is
	default:
		return Bead{}, fmt.Errorf("sqlite create-with-storage: unknown storage class %q", storage)
	}
	return s.Create(b)
}

var _ StorageCreateStore = (*SQLiteStore)(nil)
