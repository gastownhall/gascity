package beads

import (
	"context"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

// NativeDoltStore offers the narrow metadata value-CAS and deliberately NOT
// the full ConditionalWriter. The revision-CAS trio needs a backend fence
// token that beads v1.1.0 cannot supply, and declaring the interface to get
// this one method would make ResolveConditionalWriter resolve under require
// mode — converting a loud typed refusal into a silent wrong-fenced write.
// internal/beads/metadata_cas.go carries the full reasoning.
var _ MetadataCASWriter = (*NativeDoltStore)(nil)

// CompareAndSetMetadataKey atomically sets metadata[key] = next when the key's
// current value equals expected.
//
// expected == "" matches a key that is ABSENT or present with the empty value:
// parsing an absent key out of the stored metadata map yields "", so the two
// states are indistinguishable here exactly as they are to callers (release
// paths write "" to clear). Returns (true, nil) on swap, (false, nil) on a
// genuine value mismatch — a lost race is NOT an error — and (false, err) for
// a missing bead, a malformed metadata blob, or a transport failure.
//
// Atomicity is the read-check-write inside one native Dolt transaction, the
// same shape ReleaseIfCurrent uses for its assignee guard. The whole
// read-compare-write runs inside the callback, so the compare and the write
// commit together or not at all: the upstream storage layer exposes no
// conditional-UPDATE ... WHERE primitive and no raw-SQL escape hatch, making
// the transaction the only composition point available.
//
// Sibling keys are preserved: the metadata column is a single blob, so the
// write re-serializes the map read inside this transaction rather than
// patching one field.
func (s *NativeDoltStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return false, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	swapped := false
	commitMsg := fmt.Sprintf("gc: compare-and-set metadata %s on bead %s", key, id)
	err = storage.RunInTransaction(ctx, commitMsg, func(tx beadslib.Transaction) error {
		issue, err := tx.GetIssue(ctx, id)
		if err != nil {
			return nativeStoreError(id, err)
		}
		if issue == nil {
			return fmt.Errorf("compare-and-set metadata on %q: %w", id, ErrNotFound)
		}
		metadata, err := metadataMapFromNative(issue.Metadata)
		if err != nil {
			return fmt.Errorf("parsing metadata for bead %q: %w", id, err)
		}
		if metadata[key] != expected {
			// A genuine lost race. Returning nil commits an empty transaction
			// and leaves swapped false, which the caller reads as (false, nil).
			return nil
		}
		if metadata == nil {
			metadata = make(map[string]string, 1)
		}
		metadata[key] = next
		raw, err := metadataRawFromMap(metadata)
		if err != nil {
			return err
		}
		if err := tx.UpdateIssue(ctx, id, map[string]interface{}{"metadata": raw}, s.actor); err != nil {
			return nativeStoreError(id, err)
		}
		swapped = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return swapped, nil
}
