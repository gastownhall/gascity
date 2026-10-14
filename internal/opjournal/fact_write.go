package opjournal

import (
	"errors"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// RecordFact writes f create-if-absent at its pinned fact id. A new row
// returns created=true. A fact is immutable and keyed by its occurrence alone,
// so when the occurrence is already recorded — found by the read that comes
// first, or by the read after a refused create — RecordFact returns
// created=false and leaves the stored row as it is, whatever body f carries.
// A stored row that names another occurrence under the id is ErrCorruptOp. A
// refused fact writes nothing. A store that creates every row open has the
// fact's bead closed right after the create, and a fact row found open is
// closed then too. When that close fails after a create, RecordFact returns
// created=true with the error: the fact is recorded, and a later RecordFact
// of the occurrence closes the row.
//
// The read comes first so a retry never reaches Create: a store that
// overwrites a duplicate pinned id instead of refusing it cannot rewrite a
// recorded fact. Concurrent writers of one occurrence still resolve cleanly
// on every store that refuses duplicates.
func (s *Store) RecordFact(f Fact) (created bool, err error) {
	b, err := EncodeFact(s.prefix, f)
	if err != nil {
		return false, err
	}
	f.ID = b.ID
	existing, getErr := s.store.Get(b.ID)
	switch {
	case getErr == nil:
		return false, s.matchRecordedFact(f, existing)
	case !errors.Is(getErr, beads.ErrNotFound):
		return false, fmt.Errorf("reading fact %s before creating it: %w", b.ID, getErr)
	}
	stored, createErr := s.store.Create(b)
	if createErr == nil {
		if stored.ID != b.ID {
			strayErr := s.writer.DeleteIfMatch(stored.ID, stored.Revision)
			return false, errors.Join(
				fmt.Errorf("%w: wanted fact %s, store minted %s", ErrPinnedIDNotHonored, b.ID, stored.ID), strayErr)
		}
		if stored.Status != beadStatusClosed {
			// Some stores create every row open; a fact is closed on create.
			return true, s.closeRow("fact", b.ID)
		}
		return true, nil
	}
	existing, getErr = s.store.Get(b.ID)
	switch {
	case getErr == nil:
		return false, s.matchRecordedFact(f, existing)
	case errors.Is(getErr, beads.ErrNotFound):
		if strings.Contains(createErr.Error(), "invalid issue type") {
			return false, errors.Join(ErrBeadTypeUnregistered, createErr)
		}
		return false, fmt.Errorf("creating fact %s: %w", b.ID, createErr)
	default:
		return false, errors.Join(fmt.Errorf("creating fact %s: %w", b.ID, createErr),
			fmt.Errorf("reading fact %s after a failed create: %w", b.ID, getErr))
	}
}

// matchRecordedFact checks that row b, stored under want's id, records the
// same occurrence, and closes it if a writer that lost its race left it open.
func (s *Store) matchRecordedFact(want Fact, b beads.Bead) error {
	got, err := DecodeFact(b)
	if err != nil {
		return err
	}
	if got.Kind != want.Kind || got.Conversation != want.Conversation ||
		got.Generation != want.Generation || got.occurrence() != want.occurrence() {
		return corruptf(want.ID, "stored fact records another occurrence (hash collision)")
	}
	if b.Status != beadStatusClosed {
		return s.closeRow("fact", b.ID)
	}
	return nil
}
