package beads

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"

	beadslib "github.com/steveyegge/beads"
)

const nativeConditionalCloseCoordinatePrefix = "gc:native-conditional-close:"

// UpdateIfMatch applies opts only while the bead's opaque RowVersion equals
// expectedRevision. Scalar and metadata-only updates use beads' checked
// primitive. Parent and label changes share one native transaction with the
// row-version check, so the owning row and every related-table mutation publish
// together.
func (s *NativeDoltStore) UpdateIfMatch(id string, expectedRevision int64, opts UpdateOpts) error {
	if isEmptyUpdateOpts(opts) {
		return fmt.Errorf("conditional update %q: %w", id, ErrEmptyConditionalUpdate)
	}
	if err := nativeConditionalUsableRevision(id, expectedRevision); err != nil {
		return err
	}
	if nativeConditionalUpdateNeedsTransaction(opts) {
		return s.updateIfMatchInTransaction(id, expectedRevision, opts)
	}

	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	// Metadata is a merge on Gas City's Store surface. nativeUpdates reads the
	// current JSON to construct the replacement, while ExpectedVersion performs
	// the decisive comparison inside UpdateIssueChecked's write transaction.
	updates, err := s.nativeUpdates(ctx, storage, id, opts)
	if err != nil {
		return err
	}
	expected := expectedRevision
	err = retryOnNativeDoltSerializationConflict(func() error {
		return storage.UpdateIssueChecked(ctx, id, updates, s.actor, beadslib.UpdateIssueOptions{
			ExpectedVersion: &expected,
		})
	})
	if errors.Is(err, beadslib.ErrVersionMismatch) {
		return nativeConditionalVersionMismatch(ctx, storage, id, expectedRevision, err)
	}
	return nativeStoreError(id, err)
}

func nativeConditionalUpdateNeedsTransaction(opts UpdateOpts) bool {
	return opts.ParentID != nil || len(opts.Labels) > 0 || len(opts.RemoveLabels) > 0
}

// nativeDoltTransactionIssueToucher is the minimal beads transaction
// prerequisite for folding related-table mutations into Gas City's aggregate
// revision fence. Keep the assertion structural so this branch remains
// fail-closed, rather than silently reusing a stale token, if it is compiled
// against a beads revision that predates TouchIssue.
type nativeDoltTransactionIssueToucher interface {
	TouchIssue(context.Context, string, string) error
}

// nativeDoltTransactionCheckedCloser preserves the same blocker/open-child
// policy as Storage.CloseIssueChecked while composing metadata and close in one
// transaction. Plain Transaction.CloseIssue intentionally lacks those guards.
type nativeDoltTransactionCheckedCloser interface {
	CloseIssueChecked(context.Context, string, string, beadslib.CloseIssueOptions) (beadslib.CloseIssueResult, error)
}

func nativeDoltTouchIssueInTx(ctx context.Context, tx beadslib.Transaction, id, actor string) error {
	toucher, ok := any(tx).(nativeDoltTransactionIssueToucher)
	if !ok {
		return fmt.Errorf("native beads transaction %T lacks TouchIssue required for related-field revision fencing", tx)
	}
	return nativeStoreError(id, toucher.TouchIssue(ctx, id, actor))
}

func nativeDoltCloseIssueCheckedInTx(
	ctx context.Context,
	tx beadslib.Transaction,
	id, actor string,
	opts beadslib.CloseIssueOptions,
) (beadslib.CloseIssueResult, error) {
	closer, ok := any(tx).(nativeDoltTransactionCheckedCloser)
	if !ok {
		return beadslib.CloseIssueResult{}, fmt.Errorf("native beads transaction %T lacks CloseIssueChecked required for atomic close policy", tx)
	}
	result, err := closer.CloseIssueChecked(ctx, id, actor, opts)
	return result, nativeStoreError(id, err)
}

func (s *NativeDoltStore) updateIfMatchInTransaction(id string, expectedRevision int64, opts UpdateOpts) error {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	err = storage.RunInTransaction(ctx, fmt.Sprintf("gc: conditional update bead %s", id), func(tx beadslib.Transaction) error {
		issue, present, err := nativeConditionalIssueInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("conditional update %q: %w", id, ErrNotFound)
		}
		if issue.RowVersion != expectedRevision {
			return &PreconditionFailedError{
				ID:       id,
				Expected: expectedRevision,
				Current:  issue.RowVersion,
				Raw:      "native row-version mismatch",
			}
		}
		return s.applyUpdateInTx(ctx, tx, id, opts, nativeUpdatePreserveClosePolicy)
	})
	// beads af459+ invokes the callback at most once. In particular, an error
	// wrapping ErrCommitIndeterminate is returned unchanged: neither a post-read
	// nor a second callback can safely decide whether the composite published.
	return nativeStoreError(id, err)
}

// CloseIfMatch retains beads' checked-close primitive, including its live
// blocker policy. A per-call coordinate in closed_by_session permits the one
// safe ambiguous-ack recovery: an exact authoritative coordinate proves this
// call's close reached the durable owning tier.
func (s *NativeDoltStore) CloseIfMatch(id string, expectedRevision int64) error {
	if err := nativeConditionalUsableRevision(id, expectedRevision); err != nil {
		return err
	}
	coordinate, err := newNativeConditionalCloseCoordinate()
	if err != nil {
		return fmt.Errorf("conditional close %q: generating operation coordinate: %w", id, err)
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
		return fmt.Errorf("conditional close %q: %w", id, ErrNotFound)
	}
	expected := expectedRevision
	err = retryOnNativeDoltSerializationConflict(func() error {
		_, closeErr := storage.CloseIssueChecked(ctx, id, s.actor, beadslib.CloseIssueOptions{
			Reason:          nativeCloseReasonFromIssue(current),
			Session:         coordinate,
			ExpectedVersion: &expected,
		})
		return closeErr
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, beadslib.ErrCommitIndeterminate) &&
		nativeConditionalCloseCoordinateMatchesAuthoritative(ctx, storage, id, coordinate) {
		return nil
	}
	if errors.Is(err, beadslib.ErrVersionMismatch) {
		return nativeConditionalVersionMismatch(ctx, storage, id, expectedRevision, err)
	}
	return nativeStoreError(id, err)
}

func nativeConditionalUsableRevision(id string, revision int64) error {
	if revision != 0 {
		return nil
	}
	return &PreconditionFailedError{
		ID:       id,
		Expected: revision,
		Raw:      "zero is not a usable conditional-write revision token",
	}
}

func newNativeConditionalCloseCoordinate() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return nativeConditionalCloseCoordinatePrefix + hex.EncodeToString(raw[:]), nil
}

// nativeConditionalCloseCoordinateReader is a narrow test/backend seam for an
// exact authoritative closed_by_session read. Production Dolt handles use the
// raw-DB path below; fakes that do not deliberately model the durable column
// fail closed.
type nativeConditionalCloseCoordinateReader interface {
	readNativeConditionalCloseCoordinate(context.Context, string) (string, error)
}

// nativeConditionalRawDBAccessor mirrors upstream storage.RawDBAccessor
// structurally. The root beads package does not re-export that internal type.
type nativeConditionalRawDBAccessor interface {
	DB() *sql.DB
	UnderlyingDB() *sql.DB
}

func nativeConditionalCloseCoordinateMatchesAuthoritative(ctx context.Context, storage beadslib.Storage, id, want string) bool {
	issue, err := storage.GetIssue(ctx, id)
	if err != nil || issue == nil || issue.Status != beadslib.StatusClosed {
		return false
	}
	coordinate, err := readNativeConditionalCloseCoordinate(ctx, storage, id)
	return err == nil && coordinate == want
}

func readNativeConditionalCloseCoordinate(ctx context.Context, storage beadslib.Storage, id string) (string, error) {
	if reader, ok := storage.(nativeConditionalCloseCoordinateReader); ok {
		return reader.readNativeConditionalCloseCoordinate(ctx, id)
	}
	accessor, ok := nativeConditionalRawDBAccessorFor(storage)
	if !ok {
		return "", fmt.Errorf("native storage %T does not expose raw closed_by_session reads", storage)
	}
	db := accessor.DB()
	if db == nil {
		db = accessor.UnderlyingDB()
	}
	if db == nil {
		return "", errors.New("native storage returned a nil raw database")
	}

	// A permanent issue is authoritative only at Dolt HEAD: an ordinary table
	// read can see a regular SQL commit whose subsequent DOLT_COMMIT failed.
	// Wisps are Dolt-ignored, so their ordinary committed row is their durable
	// owning tier. Query both because explicit-ID wisps need not contain
	// "-wisp-"; duplicate ownership is corruption, not proof.
	type owner struct {
		present    bool
		coordinate sql.NullString
	}
	readOwner := func(query string) (owner, error) {
		var coordinate sql.NullString
		err := db.QueryRowContext(ctx, query, id).Scan(&coordinate)
		switch {
		case err == nil:
			return owner{present: true, coordinate: coordinate}, nil
		case errors.Is(err, sql.ErrNoRows):
			return owner{}, nil
		default:
			return owner{}, err
		}
	}
	permanent, err := readOwner("SELECT closed_by_session FROM issues AS OF 'HEAD' WHERE id = ?")
	if err != nil {
		return "", fmt.Errorf("reading native issue close coordinate for %q: %w", id, err)
	}
	wisp, err := readOwner("SELECT closed_by_session FROM wisps WHERE id = ?")
	if err != nil {
		return "", fmt.Errorf("reading native wisp close coordinate for %q: %w", id, err)
	}
	if permanent.present == wisp.present {
		count := 0
		if permanent.present {
			count = 2
		}
		return "", fmt.Errorf("native close coordinate owner count for %q = %d, want exactly 1", id, count)
	}
	if permanent.present {
		return permanent.coordinate.String, nil
	}
	return wisp.coordinate.String, nil
}

// nativeConditionalRawDBAccessorFor peels upstream storage decorators until it
// reaches the concrete raw-DB provider. beads' public root aliases Storage but
// does not export the DoltStorage return type of decorators' Unwrap method, so
// an ordinary Go interface cannot name that method here.
func nativeConditionalRawDBAccessorFor(storage beadslib.Storage) (nativeConditionalRawDBAccessor, bool) {
	current := any(storage)
	for range 16 {
		if accessor, ok := current.(nativeConditionalRawDBAccessor); ok {
			return accessor, true
		}
		value := reflect.ValueOf(current)
		if !value.IsValid() || value.Kind() == reflect.Pointer && value.IsNil() {
			return nil, false
		}
		unwrap := value.MethodByName("Unwrap")
		if !unwrap.IsValid() || unwrap.Type().NumIn() != 0 || unwrap.Type().NumOut() != 1 {
			return nil, false
		}
		out := unwrap.Call(nil)[0]
		if !out.IsValid() || out.Kind() == reflect.Interface && out.IsNil() || out.Kind() == reflect.Pointer && out.IsNil() {
			return nil, false
		}
		current = out.Interface()
	}
	return nil, false
}

// DeleteIfMatch deletes a bead only while its opaque RowVersion matches. The
// read, fence, and delete share one at-most-once native transaction. An
// indeterminate commit is deliberately surfaced: absence cannot prove which
// actor deleted which same-ID incarnation.
func (s *NativeDoltStore) DeleteIfMatch(id string, expectedRevision int64) error {
	if err := nativeConditionalUsableRevision(id, expectedRevision); err != nil {
		return err
	}
	storage, release, err := s.acquireStorage()
	if err != nil {
		return err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	err = storage.RunInTransaction(ctx, fmt.Sprintf("gc: conditional delete bead %s", id), func(tx beadslib.Transaction) error {
		issue, present, err := nativeConditionalIssueInTx(ctx, tx, id)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("conditional delete %q: %w", id, ErrNotFound)
		}
		if issue.RowVersion != expectedRevision {
			return &PreconditionFailedError{
				ID:       id,
				Expected: expectedRevision,
				Current:  issue.RowVersion,
				Raw:      "native row-version mismatch",
			}
		}
		return nativeStoreError(id, tx.DeleteIssue(ctx, id))
	})
	if err != nil {
		return nativeStoreError(id, err)
	}

	// localStrings is keyed only by ID and carries no issue-incarnation token.
	// Erasing it after the transaction returns could delete sidecar data written
	// for a concurrently recreated bead, so conditional delete leaves it intact.
	return nil
}

func nativeConditionalIssueInTx(ctx context.Context, tx beadslib.Transaction, id string) (*beadslib.Issue, bool, error) {
	issue, err := tx.GetIssue(ctx, id)
	if err != nil {
		if nativeConditionalMissing(err) {
			return nil, false, nil
		}
		return nil, false, nativeStoreError(id, err)
	}
	return issue, issue != nil, nil
}

func nativeConditionalMissing(err error) bool {
	return err != nil && (errors.Is(err, ErrNotFound) || errors.Is(err, beadslib.ErrNotFound) || nativeUpstreamNotFound(err))
}

func nativeConditionalVersionMismatch(ctx context.Context, storage beadslib.Storage, id string, expected int64, mismatch error) error {
	pfe := &PreconditionFailedError{ID: id, Expected: expected}
	if mismatch != nil {
		pfe.Raw = mismatch.Error()
	}
	current, err := storage.GetIssue(ctx, id)
	if err == nil && current != nil {
		pfe.Current = current.RowVersion
	} else if err != nil {
		if pfe.Raw != "" {
			pfe.Raw += "; "
		}
		pfe.Raw += "current revision reread failed: " + err.Error()
	}
	return pfe
}
