package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

// nativeGuardedIssueDeleter is the optional upstream capability that completes
// the row-version guarded mutation set. Its presence is also the version-safe
// capability marker: older beads libraries expose neither this method nor the
// complete family-transition fencing required by ConditionalWriter.
type nativeGuardedIssueDeleter interface {
	DeleteIssueChecked(context.Context, string, int64) error
}

var (
	_ ConditionalWriter                = (*NativeDoltStore)(nil)
	_ MetadataCASWriter                = (*NativeDoltStore)(nil)
	_ conditionalWriteCapabilityProber = (*NativeDoltStore)(nil)
)

func (s *NativeDoltStore) probeConditionalWriteCapability() (bool, string) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return false, err.Error()
	}
	defer release()
	if _, ok := storage.(nativeGuardedIssueDeleter); !ok {
		return false, "native beads backend does not expose guarded issue deletion"
	}
	return true, "native beads backend exposes row-version guarded mutations"
}

func (s *NativeDoltStore) guardedStorage() (beadslib.Storage, nativeGuardedIssueDeleter, func(), error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return nil, nil, nil, err
	}
	deleter, ok := storage.(nativeGuardedIssueDeleter)
	if !ok {
		release()
		return nil, nil, nil, ErrConditionalWriteUnsupported
	}
	return storage, deleter, release, nil
}

// UpdateIfMatch applies row-backed opts only while id still has
// expectedRevision.
func (s *NativeDoltStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	if err := validateConditionalUpdateOpts(opts); err != nil {
		return fmt.Errorf("conditional update %s: %w", id, err)
	}
	storage, _, release, err := s.guardedStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	updates, err := s.nativeUpdates(ctx, storage, id, opts)
	if err != nil {
		return err
	}
	err = storage.UpdateIssueChecked(ctx, id, updates, s.actor, beadslib.UpdateIssueOptions{
		ExpectedVersion: &expectedRevision,
	})
	return s.conditionalWriteError(ctx, storage, id, expectedRevision, err)
}

// CloseIfMatch closes id only while it still has expectedRevision.
func (s *NativeDoltStore) CloseIfMatch(id string, expectedRevision int64) error {
	storage, _, release, err := s.guardedStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	current, err := storage.GetIssue(ctx, id)
	if err != nil {
		return nativeStoreError(id, err)
	}
	if current == nil {
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	_, err = storage.CloseIssueChecked(ctx, id, s.actor, beadslib.CloseIssueOptions{
		Reason:          nativeCloseReasonFromIssue(current),
		ExpectedVersion: &expectedRevision,
	})
	return s.conditionalWriteError(ctx, storage, id, expectedRevision, err)
}

// DeleteIfMatch deletes id only while it still has expectedRevision.
func (s *NativeDoltStore) DeleteIfMatch(id string, expectedRevision int64) error {
	storage, deleter, release, err := s.guardedStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	if err := s.conditionalWriteError(ctx, storage, id, expectedRevision,
		deleter.DeleteIssueChecked(ctx, id, expectedRevision)); err != nil {
		return err
	}
	if err := s.localStrings.DeleteBead(id); err != nil {
		return fmt.Errorf("deleting bead %q: cleaning up local strings: %w", id, err)
	}
	return nil
}

func (s *NativeDoltStore) conditionalWriteError(
	ctx context.Context,
	storage beadslib.Storage,
	id string,
	expectedRevision int64,
	err error,
) error {
	if err == nil {
		return nil
	}
	if !errors.Is(err, beadslib.ErrVersionMismatch) {
		return nativeStoreError(id, err)
	}
	current := int64(0)
	if issue, readErr := storage.GetIssue(ctx, id); readErr == nil && issue != nil {
		current = issue.RowVersion
	}
	return &PreconditionFailedError{
		ID:       id,
		Expected: expectedRevision,
		Current:  current,
		Raw:      err.Error(),
	}
}

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
// Sibling keys are preserved with their JSON types: the public Store view is
// map[string]string, but bd metadata may also contain booleans, numbers, null,
// objects, and arrays. The transaction compares through that public string
// view, then replaces only the selected raw JSON member with a JSON string.
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
		// Upstream Dolt storage may retry this entire callback after a
		// retryable commit/connection failure. The result belongs to the
		// current attempt, not any earlier callback invocation: otherwise a
		// first attempt that reached UpdateIssue could leave swapped=true,
		// while a retry observes a competing value and returns a false
		// positive CAS success.
		swapped = false
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
		rawMetadata, err := metadataRawValuesFromNative(issue.Metadata)
		if err != nil {
			return fmt.Errorf("parsing raw metadata for bead %q: %w", id, err)
		}
		if rawMetadata == nil {
			rawMetadata = make(map[string]json.RawMessage, 1)
		}
		nextRaw, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("marshaling metadata value %q: %w", key, err)
		}
		rawMetadata[key] = nextRaw
		rawBytes, err := json.Marshal(rawMetadata)
		if err != nil {
			return fmt.Errorf("marshaling metadata: %w", err)
		}
		raw := json.RawMessage(rawBytes)
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

func metadataRawValuesFromNative(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var values map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("unmarshaling metadata: %w", err)
	}
	return values, nil
}
