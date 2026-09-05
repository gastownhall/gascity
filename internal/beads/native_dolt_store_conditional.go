package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	beadslib "github.com/steveyegge/beads"
)

var (
	_ ConditionalWriter                = (*NativeDoltStore)(nil)
	_ AtomicConditionalCloser          = (*NativeDoltStore)(nil)
	_ MetadataCASWriter                = (*NativeDoltStore)(nil)
	_ conditionalWriteCapabilityProber = (*NativeDoltStore)(nil)
)

// CloseWithMetadataIfMatch merges metadata and closes id inside one native
// transaction, but only while the exact opaque row version still matches.
// It returns the final in-transaction row only after the transaction commits.
func (s *NativeDoltStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (Bead, error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return Bead{}, err
	}
	defer release()

	var closed Bead
	err = retryOnNativeDoltSerializationConflict(func() error {
		closed = Bead{}
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		return storage.RunInTransaction(ctx, fmt.Sprintf("gc: fenced metadata close bead %s", id), func(tx beadslib.Transaction) error {
			issue, err := tx.GetIssue(ctx, id)
			if err != nil {
				return nativeStoreError(id, err)
			}
			if issue == nil {
				return fmt.Errorf("bead %q: %w", id, ErrNotFound)
			}
			if issue.RowVersion != expectedRevision {
				return &PreconditionFailedError{
					ID:       id,
					Expected: expectedRevision,
					Current:  issue.RowVersion,
					Raw:      "native row-version mismatch",
				}
			}
			merged, err := metadataMapFromNative(issue.Metadata)
			if err != nil {
				return fmt.Errorf("parsing metadata for bead %q: %w", id, err)
			}
			if merged == nil {
				merged = make(map[string]string, len(metadata))
			}
			for key, value := range metadata {
				merged[key] = value
			}
			raw, err := metadataRawFromMap(merged)
			if err != nil {
				return err
			}
			if err := tx.UpdateIssue(ctx, id, map[string]interface{}{"metadata": raw}, s.actor); err != nil {
				return nativeStoreError(id, err)
			}
			issueWithMergedMetadata := *issue
			issueWithMergedMetadata.Metadata = raw
			if err := tx.CloseIssue(ctx, id, nativeCloseReasonFromIssue(&issueWithMergedMetadata), s.actor, ""); err != nil {
				return nativeStoreError(id, err)
			}
			finalIssue, err := tx.GetIssue(ctx, id)
			if err != nil {
				return nativeStoreError(id, err)
			}
			if finalIssue == nil {
				return fmt.Errorf("bead %q: %w", id, ErrNotFound)
			}
			if finalIssue.Status != beadslib.StatusClosed {
				return fmt.Errorf("closing bead %q atomically: transaction returned status %q", id, finalIssue.Status)
			}
			closed, err = beadFromNativeIssue(finalIssue)
			return err
		})
	})
	if err != nil {
		return Bead{}, nativeStoreError(id, err)
	}
	return closed, nil
}

func (s *NativeDoltStore) probeConditionalWriteCapability() (bool, string) {
	_, release, err := s.acquireStorage()
	if err != nil {
		return false, err.Error()
	}
	defer release()
	return true, "native beads backend exposes row-version checked writes and transactions"
}

// UpdateIfMatch applies row-backed opts only while id still has
// expectedRevision.
func (s *NativeDoltStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	if err := validateConditionalUpdateOpts(opts); err != nil {
		return fmt.Errorf("conditional update %s: %w", id, err)
	}
	storage, release, err := s.acquireStorage()
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
	// Retry a transient native-Dolt serialization conflict rather than letting
	// it escape raw: embedded-Dolt has no internal withRetryTx, and the
	// nudge-queue CAS loop only re-drives PreconditionFailedError, so an
	// un-retried conflict would hard-fail to an API 500. The fence is
	// unaffected — ExpectedVersion is re-checked every attempt and a genuine
	// mismatch returns ErrVersionMismatch, which is not a serialization
	// conflict, so precondition failures still propagate immediately (never
	// retried). Mirrors DeleteIfMatch/CloseWithMetadataIfMatch.
	err = retryOnNativeDoltSerializationConflict(func() error {
		return storage.UpdateIssueChecked(ctx, id, updates, s.actor, beadslib.UpdateIssueOptions{
			ExpectedVersion: &expectedRevision,
		})
	})
	return s.conditionalWriteError(ctx, storage, id, expectedRevision, err)
}

// CloseIfMatch closes id only while it still has expectedRevision.
func (s *NativeDoltStore) CloseIfMatch(id string, expectedRevision int64) error {
	storage, release, err := s.acquireStorage()
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
	// See UpdateIfMatch: wrap only the checked write so a transient
	// serialization conflict is retried while a version mismatch still
	// short-circuits through conditionalWriteError. The pre-read close reason is
	// a deterministic function of the issue and stays valid across attempts.
	err = retryOnNativeDoltSerializationConflict(func() error {
		_, closeErr := storage.CloseIssueChecked(ctx, id, s.actor, beadslib.CloseIssueOptions{
			Reason:          nativeCloseReasonFromIssue(current),
			ExpectedVersion: &expectedRevision,
		})
		return closeErr
	})
	return s.conditionalWriteError(ctx, storage, id, expectedRevision, err)
}

// DeleteIfMatch deletes id only while it still has expectedRevision.
func (s *NativeDoltStore) DeleteIfMatch(id string, expectedRevision int64) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	commitMsg := fmt.Sprintf("gc: delete bead %s at revision %d", id, expectedRevision)
	err = retryOnNativeDoltSerializationConflict(func() error {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		return storage.RunInTransaction(ctx, commitMsg, func(tx beadslib.Transaction) error {
			issue, err := tx.GetIssue(ctx, id)
			if err != nil {
				return nativeStoreError(id, err)
			}
			if issue == nil {
				return fmt.Errorf("bead %q: %w", id, ErrNotFound)
			}
			if issue.RowVersion != expectedRevision {
				return &PreconditionFailedError{
					ID:       id,
					Expected: expectedRevision,
					Current:  issue.RowVersion,
					Raw:      "native row-version mismatch",
				}
			}
			if err := tx.DeleteIssue(ctx, id); err != nil {
				return nativeStoreError(id, err)
			}
			return nil
		})
	})
	if err != nil {
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
