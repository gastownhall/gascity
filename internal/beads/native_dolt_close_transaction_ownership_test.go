package beads

import (
	"context"
	"errors"
	"sync"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

type nativeRawWinnerBeforeTransactionStorage struct {
	*nativeCloseTransitionStorage

	mu           sync.Mutex
	targetID     string
	reason       string
	winnerOnce   sync.Once
	transactions int
}

func (s *nativeRawWinnerBeforeTransactionStorage) installRawWinner(ctx context.Context, id string) error {
	var winnerErr error
	s.winnerOnce.Do(func() {
		winnerErr = s.nativeCloseTransitionStorage.CloseIssue(
			ctx,
			id,
			s.reason,
			"raw-bd-writer",
			"raw-bd-session",
		)
	})
	return winnerErr
}

func (s *nativeRawWinnerBeforeTransactionStorage) RunInTransaction(
	ctx context.Context,
	commitMsg string,
	fn func(beadslib.Transaction) error,
) error {
	s.mu.Lock()
	s.transactions++
	targetID := s.targetID
	s.mu.Unlock()
	if err := s.installRawWinner(ctx, targetID); err != nil {
		return err
	}
	return s.nativeCloseTransitionStorage.RunInTransaction(ctx, commitMsg, fn)
}

func (s *nativeRawWinnerBeforeTransactionStorage) CloseIssue(
	ctx context.Context,
	id, _ string,
	_, _ string,
) error {
	return s.installRawWinner(ctx, id)
}

func TestNativeDoltCloseTransitionDoesNotClaimSameReasonRawWinner(t *testing.T) {
	const reason = "shared close reason"
	storage := &nativeRawWinnerBeforeTransactionStorage{
		nativeCloseTransitionStorage: newNativeCloseTransitionStorage(),
		reason:                       reason,
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "same-reason raw winner"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	storage.targetID = created.ID

	transition, err := store.CloseWithReasonIfOpen(created.ID, reason)
	if err != nil {
		t.Fatalf("CloseWithReasonIfOpen: %v", err)
	}
	if transition.Transitioned {
		t.Fatal("Transitioned = true, want false for a same-reason raw winner")
	}
	if !transition.AuthoritativeClosed(created.ID) {
		t.Fatalf("After = %#v, want authoritative raw-winner close", transition.After)
	}
	if got := transition.After.Metadata["close_reason"]; got != reason {
		t.Fatalf("After close_reason = %q, want %q", got, reason)
	}
	storage.mu.Lock()
	transactions := storage.transactions
	storage.mu.Unlock()
	if transactions != 1 {
		t.Fatalf("native close transactions = %d, want 1", transactions)
	}
}

func TestNativeDoltLifecycleCloseDoesNotClaimSameReasonRawWinner(t *testing.T) {
	const reason = "shared lifecycle close reason"
	storage := &nativeRawWinnerBeforeTransactionStorage{
		nativeCloseTransitionStorage: newNativeCloseTransitionStorage(),
		reason:                       reason,
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "same-reason raw lifecycle winner"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	storage.targetID = created.ID

	var result LifecycleCloseResult
	err = store.WithLifecycleMetadataTransaction(created.ID, func(tx LifecycleMetadataTransaction) error {
		var closeErr error
		result, closeErr = CloseWithinLifecycleMetadataTransaction(tx, reason)
		return closeErr
	})
	if err != nil {
		t.Fatalf("WithLifecycleMetadataTransaction: %v", err)
	}
	if result.Transitioned || result.CloseSucceeded {
		t.Fatalf("close result = %#v, want no ownership or close acknowledgement for raw winner", result)
	}
	if !result.AuthoritativeClosed(created.ID) {
		t.Fatalf("After = %#v, want authoritative raw-winner close", result.After)
	}
}

func TestNativeDoltCloseAllTransitionDoesNotClaimSameReasonRawWinner(t *testing.T) {
	const reason = "shared batch close reason"
	storage := &nativeRawWinnerBeforeTransactionStorage{
		nativeCloseTransitionStorage: newNativeCloseTransitionStorage(),
		reason:                       reason,
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "same-reason raw batch winner"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	storage.targetID = created.ID

	result, err := store.CloseAllWithTransitions(
		[]string{created.ID},
		map[string]string{"close_reason": reason},
	)
	if err != nil {
		t.Fatalf("CloseAllWithTransitions: %v", err)
	}
	if result.Count != 0 {
		t.Fatalf("close count = %d, want 0 for raw winner", result.Count)
	}
	transition := result.Transitions[created.ID]
	if transition.Transitioned {
		t.Fatal("Transitioned = true, want false for a same-reason raw batch winner")
	}
	if !transition.AuthoritativeClosed(created.ID) {
		t.Fatalf("After = %#v, want authoritative raw-winner close", transition.After)
	}
}

type nativeAmbiguousCloseCommitStorage struct {
	*nativeCloseTransitionStorage
	commitErr error
}

func (s *nativeAmbiguousCloseCommitStorage) RunInTransaction(
	ctx context.Context,
	commitMsg string,
	fn func(beadslib.Transaction) error,
) error {
	if err := s.nativeCloseTransitionStorage.RunInTransaction(ctx, commitMsg, fn); err != nil {
		return err
	}
	return s.commitErr
}

func TestNativeDoltCloseTransitionAmbiguousCommitReturnsAfterWithoutOwnership(t *testing.T) {
	commitErr := errors.New("commit acknowledgement lost")
	storage := &nativeAmbiguousCloseCommitStorage{
		nativeCloseTransitionStorage: newNativeCloseTransitionStorage(),
		commitErr:                    commitErr,
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "ambiguous native close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	transition, err := store.CloseWithReasonIfOpen(created.ID, "ambiguous close")
	if !errors.Is(err, commitErr) {
		t.Fatalf("CloseWithReasonIfOpen error = %v, want %v", err, commitErr)
	}
	if transition.Transitioned {
		t.Fatal("Transitioned = true after ambiguous commit acknowledgement")
	}
	if !transition.AuthoritativeClosed(created.ID) {
		t.Fatalf("After = %#v, want authoritative durable close", transition.After)
	}
	if got := transition.After.Metadata["close_reason"]; got != "ambiguous close" {
		t.Fatalf("After close_reason = %q, want ambiguous close", got)
	}
}

func TestNativeDoltLifecycleCloseAmbiguousCommitReturnsAfterWithoutOwnership(t *testing.T) {
	commitErr := errors.New("lifecycle commit acknowledgement lost")
	storage := &nativeAmbiguousCloseCommitStorage{
		nativeCloseTransitionStorage: newNativeCloseTransitionStorage(),
		commitErr:                    commitErr,
	}
	store := newNativeDoltStoreForTest(storage)
	created, err := store.Create(Bead{Title: "ambiguous native lifecycle close"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var result LifecycleCloseResult
	err = store.WithLifecycleMetadataTransaction(created.ID, func(tx LifecycleMetadataTransaction) error {
		var closeErr error
		result, closeErr = CloseWithinLifecycleMetadataTransaction(tx, "ambiguous lifecycle close")
		return closeErr
	})
	if !errors.Is(err, commitErr) {
		t.Fatalf("WithLifecycleMetadataTransaction error = %v, want %v", err, commitErr)
	}
	if result.Transitioned || result.CloseSucceeded {
		t.Fatalf("close result = %#v, want durable state without ownership or acknowledgement", result)
	}
	if !result.AuthoritativeClosed(created.ID) {
		t.Fatalf("After = %#v, want authoritative durable close", result.After)
	}
	if got := result.After.Metadata["close_reason"]; got != "ambiguous lifecycle close" {
		t.Fatalf("After close_reason = %q, want ambiguous lifecycle close", got)
	}
}
