package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	beadslib "github.com/steveyegge/beads"
)

var (
	_ ConditionalWriter                = (*NativeDoltStore)(nil)
	_ AtomicConditionalCloser          = (*NativeDoltStore)(nil)
	_ MetadataCASWriter                = (*NativeDoltStore)(nil)
	_ AtomicCommentImporter            = (*NativeDoltStore)(nil)
	_ conditionalWriteCapabilityProber = (*NativeDoltStore)(nil)
)

// UpdateWithCommentsIfMatch imports external comments and their durable
// idempotency IDs together with any row-backed update inside one upstream Dolt
// transaction. It deliberately has no fallback to storage-level comment
// writes: a backend whose Transaction cannot import/read comments fails and
// rolls the transaction back.
func (s *NativeDoltStore) UpdateWithCommentsIfMatch(
	id string,
	expectedRevision int64,
	opts UpdateOpts,
	comments []ImportedComment,
) error {
	if err := validateImportedComments(comments); err != nil {
		return fmt.Errorf("atomic comment import %s: %w", id, err)
	}
	if _, collision := opts.Metadata[ImportedCommentIDsMetadataKey]; collision {
		return fmt.Errorf("atomic comment import %s: metadata %q is managed by comments", id, ImportedCommentIDsMetadataKey)
	}
	if !isEmptyUpdateOpts(opts) {
		if err := validateConditionalUpdateOpts(opts); err != nil {
			return fmt.Errorf("atomic comment import %s: %w", id, err)
		}
	}

	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	commitMsg := fmt.Sprintf("gc: import %d comments on bead %s at revision %d", len(comments), id, expectedRevision)
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

			metadata, err := metadataMapFromNative(issue.Metadata)
			if err != nil {
				return fmt.Errorf("parsing metadata for bead %q: %w", id, err)
			}
			existing, err := importedCommentIDSet(metadata[ImportedCommentIDsMetadataKey])
			if err != nil {
				return fmt.Errorf("parsing comment receipt for bead %q: %w", id, err)
			}
			missing := make([]ImportedComment, 0, len(comments))
			for _, comment := range comments {
				if _, ok := existing[comment.ExternalID]; ok {
					continue
				}
				existing[comment.ExternalID] = struct{}{}
				missing = append(missing, comment)
			}

			ids := make([]string, 0, len(existing))
			for externalID := range existing {
				ids = append(ids, externalID)
			}
			sort.Strings(ids)
			rawIDs, err := json.Marshal(ids)
			if err != nil {
				return fmt.Errorf("marshaling imported comment ids: %w", err)
			}
			transactionOpts := opts
			transactionOpts.Metadata = cloneStringMap(opts.Metadata)
			transactionOpts.Metadata[ImportedCommentIDsMetadataKey] = string(rawIDs)

			updates, err := s.nativeUpdatesPreservingRawMetadata(ctx, tx, id, issue.Metadata, transactionOpts)
			if err != nil {
				return err
			}
			if len(updates) > 0 {
				if err := tx.UpdateIssue(ctx, id, updates, s.actor); err != nil {
					return nativeStoreError(id, err)
				}
			}

			imported := make(map[string]ImportedComment, len(missing))
			for _, comment := range missing {
				created, err := tx.ImportIssueComment(
					ctx, id, comment.Author, comment.Text, comment.CreatedAt.UTC(),
				)
				if err != nil {
					return fmt.Errorf("importing comment %q on bead %q: %w", comment.ExternalID, id, err)
				}
				if created == nil || created.ID == "" {
					return fmt.Errorf("importing comment %q on bead %q: transaction returned no durable comment identity", comment.ExternalID, id)
				}
				imported[created.ID] = comment
			}
			if len(imported) > 0 {
				readback, err := tx.GetIssueComments(ctx, id)
				if err != nil {
					return fmt.Errorf("reading imported comments on bead %q in transaction: %w", id, err)
				}
				for _, got := range readback {
					want, ok := imported[got.ID]
					if !ok {
						continue
					}
					if got.IssueID != id || got.Author != want.Author || got.Text != want.Text || !got.CreatedAt.Equal(want.CreatedAt.UTC()) {
						return fmt.Errorf("imported comment %q on bead %q failed in-transaction readback", want.ExternalID, id)
					}
					delete(imported, got.ID)
				}
				if len(imported) != 0 {
					return fmt.Errorf("imported comments on bead %q are absent from in-transaction readback", id)
				}
			}
			return nil
		})
	})
	if err != nil {
		return nativeStoreError(id, err)
	}
	return nil
}

func (s *NativeDoltStore) nativeUpdatesPreservingRawMetadata(
	ctx context.Context,
	storage nativeIssueGetter,
	id string,
	rawMetadata json.RawMessage,
	opts UpdateOpts,
) (map[string]interface{}, error) {
	withoutMetadata := opts
	withoutMetadata.Metadata = nil
	updates, err := s.nativeUpdates(ctx, storage, id, withoutMetadata)
	if err != nil {
		return nil, err
	}
	if len(opts.Metadata) == 0 {
		return updates, nil
	}
	rawValues, err := metadataRawValuesFromNative(rawMetadata)
	if err != nil {
		return nil, fmt.Errorf("parsing raw metadata for bead %q: %w", id, err)
	}
	if rawValues == nil {
		rawValues = make(map[string]json.RawMessage, len(opts.Metadata))
	}
	for key, value := range opts.Metadata {
		rawValue, err := json.Marshal(value)
		if err != nil {
			return nil, fmt.Errorf("marshaling metadata value %q: %w", key, err)
		}
		rawValues[key] = rawValue
	}
	rawBytes, err := json.Marshal(rawValues)
	if err != nil {
		return nil, fmt.Errorf("marshaling metadata: %w", err)
	}
	updates["metadata"] = json.RawMessage(rawBytes)
	return updates, nil
}

func cloneStringMap(source map[string]string) map[string]string {
	clone := make(map[string]string, len(source)+1)
	for key, value := range source {
		clone[key] = value
	}
	return clone
}

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
