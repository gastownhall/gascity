package beads

import "fmt"

// UpdatedRow is the post-write state of the fields a caller reads back after
// an update to confirm it took: the bead's status and its full metadata.
type UpdatedRow struct {
	Status   string
	Metadata map[string]string
}

// UpdateReporter is the optional capability of a store whose update reports
// the row it wrote in the same round trip, so a caller that must confirm a
// close (or read the resulting metadata) does not pay a second read.
//
// A store that cannot parse its own write output answers with a read of the
// row, so the report is always the post-write state, never a guess.
//
// Wrappers that override Update must not let an embedded UpdateReporting
// bypass the override; they either forward through their own Update or
// implement UpdateReporting themselves.
type UpdateReporter interface {
	UpdateReporting(id string, opts UpdateOpts) (UpdatedRow, error)
}

// UpdateAndReadBack applies opts to id and returns the row as it stands
// afterwards: in one round trip through UpdateReporting when store offers it,
// otherwise as Update followed by Get.
func UpdateAndReadBack(store Store, id string, opts UpdateOpts) (UpdatedRow, error) {
	if reporter, ok := store.(UpdateReporter); ok {
		return reporter.UpdateReporting(id, opts)
	}
	if err := store.Update(id, opts); err != nil {
		return UpdatedRow{}, err
	}
	bead, err := store.Get(id)
	if err != nil {
		return UpdatedRow{}, fmt.Errorf("reading back %s after update: %w", id, err)
	}
	return UpdatedRow{Status: bead.Status, Metadata: bead.Metadata}, nil
}

var _ UpdateReporter = (*BdStore)(nil)

// UpdateReporting runs the same `bd update --json` Update runs and reads the
// written row from the JSON bd prints for it, saving the `bd show` a caller
// would otherwise issue to confirm the write. Output that does not carry the
// row (an older bd, or a shape change) falls back to Get.
func (s *BdStore) UpdateReporting(id string, opts UpdateOpts) (UpdatedRow, error) {
	args := bdUpdateArgs(id, opts)
	if len(args) == 3 {
		// No fields to update: Update is a no-op, so report the row as it is.
		return s.readBackUpdatedRow(id)
	}
	out, err := s.runBDTransientWriteOutput(args...)
	if err != nil {
		if isBdNotFound(err) {
			return UpdatedRow{}, fmt.Errorf("updating bead %q: %w", id, ErrNotFound)
		}
		return UpdatedRow{}, fmt.Errorf("updating bead %q: %w", id, err)
	}
	issues, parseErr := parseIssuesTolerant(extractJSON(out))
	if parseErr == nil {
		for _, issue := range issues {
			if issue.ID != id || issue.Status == "" {
				continue
			}
			written := issue.toBead()
			return UpdatedRow{Status: written.Status, Metadata: written.Metadata}, nil
		}
	}
	return s.readBackUpdatedRow(id)
}

func (s *BdStore) readBackUpdatedRow(id string) (UpdatedRow, error) {
	bead, err := s.Get(id)
	if err != nil {
		return UpdatedRow{}, fmt.Errorf("reading back %s after update: %w", id, err)
	}
	return UpdatedRow{Status: bead.Status, Metadata: bead.Metadata}, nil
}
