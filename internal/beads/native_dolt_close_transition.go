package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"maps"

	beadslib "github.com/steveyegge/beads"
)

type nativeCloseIdentity struct {
	ID              string
	Status          string
	CloseReason     string
	ClosedBySession string
}

type nativeCloseIdentityReadFunc func(context.Context, string) (nativeCloseIdentity, error)

// CloseWithReasonIfOpen closes a live bead through one upstream transaction.
// The transaction's before and after snapshots make ownership exact even when
// a raw bd writer races Gas City with the same close reason.
func (s *NativeDoltStore) CloseWithReasonIfOpen(id, reason string) (CloseTransition, error) {
	unlockScope, err := lockCloseTransitionScope(s.closeTransitionScopeKey())
	if err != nil {
		return CloseTransition{}, fmt.Errorf("locking native close transition for bead %q: %w", id, err)
	}
	defer unlockScope()
	return s.closeWithReasonIfOpenWithoutScopeLock(id, reason)
}

// closeWithReasonIfOpenWithoutScopeLock performs the exact native transition
// while its caller holds the lifecycle scope. A clean transaction commit owns
// an open-to-closed transition. Any transaction error makes ownership
// ambiguous, so a post-read may report durable state but never Transitioned.
func (s *NativeDoltStore) closeWithReasonIfOpenWithoutScopeLock(id, reason string) (CloseTransition, error) {
	storage, release, err := s.acquireStorage()
	if err != nil {
		return CloseTransition{}, err
	}
	defer release()
	ctx, cancel := nativeDoltOperationContext(context.TODO())
	defer cancel()

	var transition CloseTransition
	runErr := storage.RunInTransaction(ctx, fmt.Sprintf("gc: close bead %s with transition", id), func(tx beadslib.Transaction) error {
		before, err := nativeUpdateSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		transition.Before = before
		if before.Status == "closed" {
			transition.After = before
			return nil
		}

		reason = closeReasonForTransition(before, reason)
		// Preserve NativeDoltStore.Close's durable attribution semantics. The
		// upstream native API has no Gas City session argument, so ordinary closes
		// historically store the empty session rather than an ownership nonce.
		if err := tx.CloseIssue(ctx, id, reason, s.actor, ""); err != nil {
			return nativeStoreError(id, err)
		}
		after, err := nativeUpdateSnapshot(ctx, tx, id)
		if err != nil {
			return err
		}
		if after.Status != "closed" {
			return fmt.Errorf("closing bead %q with reason: transaction status is %q after close", id, after.Status)
		}
		transition.After = after
		transition.Transitioned = true
		return nil
	})
	if runErr == nil {
		return transition, nil
	}

	// A failed commit acknowledgement cannot prove which writer owns the close.
	// Discard the in-transaction After and replace it only with a fresh,
	// authoritative post-read from the storage handle.
	transition.After = Bead{}
	transition.Transitioned = false
	wrappedRunErr := nativeStoreError(id, runErr)
	snapshotReader, ok := storage.(nativeUpdateSnapshotReader)
	if !ok {
		verifyErr := fmt.Errorf("verifying native close transition for bead %q: %w", id, ErrCloseTransitionUnsupported)
		return transition, errors.Join(wrappedRunErr, verifyErr)
	}
	after, verifyErr := nativeUpdateSnapshot(ctx, snapshotReader, id)
	if verifyErr != nil {
		return transition, errors.Join(wrappedRunErr, verifyErr)
	}
	transition.After = after
	return transition, wrappedRunErr
}

func (s *NativeDoltStore) closeTransitionScopeKey() string {
	if s == nil {
		return "native-dolt:nil"
	}
	if s.scopeRoot != "" {
		return s.scopeRoot
	}
	return fmt.Sprintf("native-dolt:%p", s)
}

func nativeCloseIdentityReaderForScope(scopeRoot string, env map[string]string) nativeCloseIdentityReadFunc {
	env = maps.Clone(env)
	return func(ctx context.Context, id string) (nativeCloseIdentity, error) {
		reader := NewBdStore(scopeRoot, ExecCommandRunnerWithEnvContext(ctx, env),
			WithBdStoreCommandEnvRunner(ExecCommandEnvRunnerWithEnvContext(ctx, env)))
		identity, err := reader.readCloseIdentity(id)
		if err != nil {
			return nativeCloseIdentity{}, err
		}
		return nativeCloseIdentity(identity), nil
	}
}

func queryNativeCloseIdentity(ctx context.Context, db *sql.DB, id string) (nativeCloseIdentity, error) {
	const query = `
		SELECT id, status, close_reason, closed_by_session FROM wisps WHERE id = ?
		UNION ALL
		SELECT id, status, close_reason, closed_by_session FROM issues
		WHERE id = ? AND NOT EXISTS (SELECT 1 FROM wisps WHERE id = ?)
		LIMIT 1`
	var identity nativeCloseIdentity
	var reason, session sql.NullString
	err := db.QueryRowContext(ctx, query, id, id, id).Scan(&identity.ID, &identity.Status, &reason, &session)
	if errors.Is(err, sql.ErrNoRows) {
		return nativeCloseIdentity{}, fmt.Errorf("reading native close identity for bead %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return nativeCloseIdentity{}, fmt.Errorf("reading native close identity for bead %q: %w", id, err)
	}
	identity.CloseReason = reason.String
	identity.ClosedBySession = session.String
	return identity, nil
}
