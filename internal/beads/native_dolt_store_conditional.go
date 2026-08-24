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
// transaction, but only while the exact opaque row version still matches. The
// beads af459+ transaction contract invokes the callback at most once.
func (s *NativeDoltStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (Bead, error) {
	if err := nativeConditionalUsableRevision(id, expectedRevision); err != nil {
		return Bead{}, err
	}
	coordinate, err := newNativeConditionalCloseCoordinate()
	if err != nil {
		return Bead{}, fmt.Errorf("atomic conditional close %q: generating operation coordinate: %w", id, err)
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return Bead{}, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	var closed Bead
	err = storage.RunInTransaction(ctx, fmt.Sprintf("gc: fenced metadata close bead %s", id), func(tx beadslib.Transaction) error {
		issue, present, err := nativeConditionalIssueInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("atomic conditional close %q: %w", id, ErrNotFound)
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
		if _, err := nativeDoltCloseIssueCheckedInTx(ctx, tx, id, s.actor, beadslib.CloseIssueOptions{
			Reason:  nativeCloseReasonFromIssue(&issueWithMergedMetadata),
			Session: coordinate,
		}); err != nil {
			return err
		}
		finalIssue, err := tx.GetIssue(ctx, id)
		if err != nil {
			return nativeStoreError(id, err)
		}
		if finalIssue == nil {
			return fmt.Errorf("atomic conditional close %q: %w", id, ErrNotFound)
		}
		if finalIssue.Status != beadslib.StatusClosed {
			return fmt.Errorf("closing bead %q atomically: transaction returned status %q", id, finalIssue.Status)
		}
		closed, err = beadFromNativeIssue(finalIssue)
		return err
	})
	if err == nil {
		return closed, nil
	}
	// The coordinate is checked only for the exported indeterminate sentinel,
	// and only against the authoritative owning tier. Any mismatch/absence keeps
	// the original ambiguity; a semantic closed post-image is not authorship.
	if errors.Is(err, beadslib.ErrCommitIndeterminate) && closed.ID != "" &&
		nativeConditionalCloseCoordinateMatchesAuthoritative(ctx, storage, id, coordinate) {
		return closed, nil
	}
	return Bead{}, nativeStoreError(id, err)
}

func (s *NativeDoltStore) probeConditionalWriteCapability() (bool, string) {
	_, release, err := s.acquireStorage()
	if err != nil {
		return false, err.Error()
	}
	defer release()
	return true, "native beads backend exposes row-version checked writes and at-most-once transactions"
}

// CompareAndSetMetadataKey atomically sets metadata[key] = next when the key's
// current value equals expected.
//
// expected == "" matches a key that is ABSENT or present with the empty value:
// parsing an absent key out of the stored metadata map yields "", so the two
// states are indistinguishable here exactly as they are to callers (release
// paths write "" to clear). Returns (true, nil) on swap, (false, nil) on a
// genuine value mismatch, and (false, err) for everything else.
//
// The compare and raw sibling-preserving replacement run in one at-most-once
// transaction. ErrCommitIndeterminate is propagated as (false, err); a post-read
// match cannot establish whether this caller authored the value.
func (s *NativeDoltStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return false, err
	}
	defer release()

	swapped := false
	err = retryOnNativeDoltSerializationConflict(func() error {
		// A decoded 1213/1205/1105 rollback permits a fresh value-CAS. Reset
		// the attempt result before re-reading so a first rolled-back write can
		// never credit a later value mismatch as this caller's success.
		swapped = false
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		return storage.RunInTransaction(ctx, fmt.Sprintf("gc: compare-and-set metadata %s on bead %s", key, id), func(tx beadslib.Transaction) error {
			issue, present, err := nativeConditionalIssueInTx(ctx, tx, id)
			if err != nil {
				return err
			}
			if !present {
				return fmt.Errorf("compare-and-set metadata on %q: %w", id, ErrNotFound)
			}
			metadata, err := metadataMapFromNative(issue.Metadata)
			if err != nil {
				return fmt.Errorf("parsing metadata for bead %q: %w", id, err)
			}
			if metadata[key] != expected {
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
			if err := tx.UpdateIssue(ctx, id, map[string]interface{}{"metadata": json.RawMessage(rawBytes)}, s.actor); err != nil {
				return nativeStoreError(id, err)
			}
			swapped = true
			return nil
		})
	})
	if err != nil {
		return false, nativeStoreError(id, err)
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
