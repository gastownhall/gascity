package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strconv"

	"github.com/gastownhall/gascity/internal/beadmeta"
	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

var (
	_ ConditionalWriter                = (*NativeDoltStore)(nil)
	_ AtomicConditionalCloser          = (*NativeDoltStore)(nil)
	_ MetadataCASWriter                = (*NativeDoltStore)(nil)
	_ conditionalWriteCapabilityProber = (*NativeDoltStore)(nil)
	_ conditionalLabelsGuard           = (*NativeDoltStore)(nil)
)

// CloseWithMetadataIfMatch merges metadata and closes id inside one native
// transaction, but only while the exact opaque row version still matches.
// It returns the final in-transaction row only after the transaction commits.
func (s *NativeDoltStore) CloseWithMetadataIfMatch(id string, expectedRevision int64, metadata map[string]string) (Bead, error) {
	if err := s.readOnlyGuard(); err != nil {
		return Bead{}, err
	}
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

// conditionalLabelsGuarded reports that UpdateIfMatch applies labels in the
// transaction that checks the row version, and moves the version with them
// (updateLabelsIfMatch).
func (s *NativeDoltStore) conditionalLabelsGuarded() bool { return true }

// UpdateIfMatch applies row-backed opts only while id still has
// expectedRevision.
func (s *NativeDoltStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
	if err := validateConditionalUpdateOpts(opts, s.conditionalLabelsGuarded()); err != nil {
		return fmt.Errorf("conditional update %s: %w", id, err)
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	if len(opts.Labels) > 0 || len(opts.RemoveLabels) > 0 {
		return s.updateLabelsIfMatch(storage, id, expectedRevision, opts)
	}
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

// updateLabelsIfMatch is UpdateIfMatch for opts that carry labels: one
// transaction checks the row version, then applies the row fields and the
// label writes. Upstream label writes touch only the label and event tables
// (wisp_labels and wisp_events for a wisp), which leave the row version where
// it was, so the transaction also advances beadmeta.LabelRevisionMetadataKey
// in the row's metadata. That issues-row (or wisps-row) change mints a fresh
// row version, so a CAS read before this write fails afterwards.
//
// The key is bookkeeping, not desired state: a caller comparing metadata for
// convergence must ignore it, and nothing else reads it.
func (s *NativeDoltStore) updateLabelsIfMatch(storage beadslib.Storage, id string, expectedRevision int64, opts UpdateOpts) error {
	return retryOnNativeDoltSerializationConflict(func() error {
		ctx, cancel := nativeDoltOperationContext(context.TODO())
		defer cancel()
		return storage.RunInTransaction(ctx, fmt.Sprintf("gc: fenced label update bead %s", id), func(tx beadslib.Transaction) error {
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
			stamped := opts
			stamped.Metadata = maps.Clone(opts.Metadata)
			if stamped.Metadata == nil {
				stamped.Metadata = make(map[string]string, 1)
			}
			stamped.Metadata[beadmeta.LabelRevisionMetadataKey] = nextConditionalLabelRevision(issue.Metadata)
			return s.applyUpdateInTx(ctx, tx, id, stamped)
		})
	})
}

// nextConditionalLabelRevision returns a beadmeta.LabelRevisionMetadataKey
// value that differs from the one raw holds, so writing it always changes the
// row.
func nextConditionalLabelRevision(raw json.RawMessage) string {
	var fields map[string]json.RawMessage
	var prev string
	if json.Unmarshal(raw, &fields) == nil {
		_ = json.Unmarshal(fields[beadmeta.LabelRevisionMetadataKey], &prev)
	}
	n, _ := strconv.ParseInt(prev, 10, 64)
	return strconv.FormatInt(n+1, 10)
}

// CloseIfMatch closes id only while it still has expectedRevision.
func (s *NativeDoltStore) CloseIfMatch(id string, expectedRevision int64) error {
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
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
	if err := s.readOnlyGuard(); err != nil {
		return err
	}
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
// ATOMICITY IS THE ROLE'S. issueops.MetadataCAS reads the key, compares it and
// re-serializes the whole metadata object inside ONE transaction the substrate
// opens, which is the property this method used to compose by hand out of
// RunInTransaction. Composing it by hand is no longer possible everywhere:
// RunInTransaction is off the v0 served surface, so the hand-built version was
// a hard failure on any store reached over the wire. The role is a required
// member of the Storage contract, so every backend answers it.
//
// THE STRING FRONT DOOR AND THE ROLE DISAGREE ABOUT THREE THINGS, and each
// disagreement is normalized here rather than pushed onto callers:
//
//   - ABSENT vs PRESENT-EMPTY. The role tells them apart (a nil Expected means
//     absent; an Expected of `""` means present holding the empty string); this
//     front door does not, and its conformance contract says an empty
//     expectation claims EITHER. So an empty expectation is TWO ARMS: absent
//     first, then present-empty, and the second is dialed only when the first
//     refusal reports Current as exactly the empty string. Any other Current is
//     a live value and a genuine lost race, which must not be retried against —
//     a single-arm request would claim only one of the two states, and which
//     one it missed would depend on whether the row predates the
//     clear-as-deletion below.
//   - CLEARING. A next of "" is spelled as the role's DELETION (a nil Value)
//     rather than as a stored empty string. The two are observationally
//     identical through this front door — TestMetadataEmptyStringClearContract
//     pins the observable clear semantics and explicitly does not assert
//     physical deletion — and deletion is the spelling that leaves the NEXT
//     acquire winning on its first arm instead of putting every workspace
//     permanently on the fallback.
//   - NON-STRING VALUES. The role compares JSON values; this front door names
//     a stored value by the text the read projection rendered for it, and a
//     stored null, number or object renders as its JSON re-encoding, which
//     the string encoding of expected never equals. So a refusal over such a
//     value is retried once with the stored bytes when, and only when, its
//     rendering equals expected.
func (s *NativeDoltStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if err := s.readOnlyGuard(); err != nil {
		return false, err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return false, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	cas, err := storage.MetadataCAS()
	if err != nil {
		return false, nativeStoreError(id, err)
	}
	value, err := nativeMetadataCASValue(next)
	if err != nil {
		return false, fmt.Errorf("compare-and-set metadata on %q: %w", id, err)
	}
	request := issueops.CompareAndSetKeyRequest{
		Actor:   s.actor,
		IssueID: id,
		Key:     key,
		Value:   value,
	}

	if expected != "" {
		stringExpected, err := nativeMetadataCASValue(expected)
		if err != nil {
			return false, fmt.Errorf("compare-and-set metadata on %q: %w", id, err)
		}
		request.Expected = stringExpected
		result, err := cas.CompareAndSetKey(ctx, request)
		if err != nil {
			return false, nativeStoreError(id, err)
		}
		if result.Swapped {
			return true, nil
		}
		// Arm two, dialed only when the refusal's Current is a non-string JSON
		// value that the read projection renders as expected -- a stored value
		// this store's writers never produce (they only ever write strings) but
		// that a caller still names by the text metadataMapFromNative handed it
		// (a stored null reads as "null", a stored 1.50 as "1.5"). Retrying with
		// the stored bytes verbatim is the one extra call that lets the
		// comparison reach them. Anything else -- a live different value, or a
		// JSON string (which arm one already compared) -- is a genuine mismatch,
		// and retrying against it would turn a lost race into a steal.
		raw, ok, err := nativeMetadataCASNonStringTextMatch(result.Current, expected)
		if err != nil {
			return false, fmt.Errorf("compare-and-set metadata on %q: %w", id, err)
		}
		if !ok {
			return false, nil
		}
		request.Expected = raw
		result, err = cas.CompareAndSetKey(ctx, request)
		if err != nil {
			return false, nativeStoreError(id, err)
		}
		return result.Swapped, nil
	}

	// Arm one: the key is absent.
	request.Expected = nil
	result, err := cas.CompareAndSetKey(ctx, request)
	if err != nil {
		return false, nativeStoreError(id, err)
	}
	if result.Swapped {
		return true, nil
	}
	// Arm two, and ONLY when the refusal named the empty string. A refusal
	// carrying any other value is a live holder, and retrying against it would
	// turn a lost race into a steal.
	heldEmpty, err := nativeMetadataCASIsEmptyString(result.Current)
	if err != nil {
		return false, fmt.Errorf("compare-and-set metadata on %q: %w", id, err)
	}
	if !heldEmpty {
		return false, nil
	}
	empty := json.RawMessage(`""`)
	request.Expected = &empty
	result, err = cas.CompareAndSetKey(ctx, request)
	if err != nil {
		return false, nativeStoreError(id, err)
	}
	return result.Swapped, nil
}

// nativeMetadataCASValue encodes one string-shaped metadata value for the role.
// The empty string is the CLEAR, and the role spells a clear as a nil value.
func nativeMetadataCASValue(value string) (*json.RawMessage, error) {
	if value == "" {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("encoding metadata value: %w", err)
	}
	raw := json.RawMessage(encoded)
	return &raw, nil
}

// nativeMetadataCASNonStringTextMatch reports whether current is a stored raw
// JSON value that is NOT itself a JSON string but that the read projection
// renders as expected, and if so returns the stored bytes to retry with --
// production carries beads with a non-string metadata value written by some
// other path (an integer gc.control_epoch, among others), and this is the one
// comparison that still reaches them without a pre-read: the role's own refusal
// already carries the stored value in Current, so there is nothing to fetch
// separately.
//
// The comparison is against metadataValueText's rendering, not Current's own
// text, because that rendering is what a read-then-CAS caller was handed: the
// role reports Current canonically (literal numbers, sorted keys), the read
// re-encodes a decoded value, and the two part on a stored 1.50 or a null. The
// base store compared the rendering too.
//
// A JSON string is excluded outright: arm one's plain string encoding already
// compared it, and matching it here would risk a false positive against a
// string that happens to equal its own quoted form.
func nativeMetadataCASNonStringTextMatch(current *json.RawMessage, expected string) (*json.RawMessage, bool, error) {
	if current == nil {
		return nil, false, nil
	}
	var decoded interface{}
	if err := json.Unmarshal(*current, &decoded); err != nil {
		return nil, false, fmt.Errorf("decoding the stored value the refusal reported: %w", err)
	}
	if _, isString := decoded.(string); isString {
		return nil, false, nil
	}
	text, err := metadataValueText(decoded)
	if err != nil {
		return nil, false, fmt.Errorf("rendering the stored value the refusal reported: %w", err)
	}
	if text != expected {
		return nil, false, nil
	}
	raw := append(json.RawMessage(nil), *current...)
	return &raw, true, nil
}

// nativeMetadataCASIsEmptyString reports the one Current value that makes the
// present-empty arm worth dialing. A nil Current is an absent key the first arm
// already lost to (a concurrent writer took it between the two), and anything
// else is a live value -- a stored null included, which reads as "null" and
// which a plain string decode would otherwise accept as "".
func nativeMetadataCASIsEmptyString(current *json.RawMessage) (bool, error) {
	if current == nil {
		return false, nil
	}
	var decoded interface{}
	if err := json.Unmarshal(*current, &decoded); err != nil {
		return false, fmt.Errorf("decoding the stored value the refusal reported: %w", err)
	}
	s, isString := decoded.(string)
	return isString && s == "", nil
}
