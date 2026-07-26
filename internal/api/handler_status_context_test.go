package api

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// deadlineRecordingListStore is a beads.Store + beads.ContextLister fake
// that records whether each ListContext call carried a deadline. It also
// implements Counter, returning ErrCountUnsupported, to force the status
// work-count path down to its List/ListContext fallback.
type deadlineRecordingListStore struct {
	beads.Store
	listContextCalls int
	sawDeadline      bool
	listCalls        int // plain (non-ctx) List calls; must stay 0 once ListContext is preferred
}

func (s *deadlineRecordingListStore) Count(context.Context, beads.ListQuery, ...string) (int, error) {
	return 0, beads.ErrCountUnsupported
}

func (s *deadlineRecordingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.Store.List(query)
}

func (s *deadlineRecordingListStore) ListContext(ctx context.Context, query beads.ListQuery) ([]beads.Bead, error) {
	s.listContextCalls++
	if _, ok := ctx.Deadline(); ok {
		s.sawDeadline = true
	}
	return s.Store.List(query)
}

// TestHandleStatusWorkCountsPreferListContextOverGoroutineList proves the
// status work-count fallback (Counter unsupported) prefers ListContext, with
// a real, request-scoped deadline bound to it — not the abandon-goroutine
// plain List path — when the store implements beads.ContextLister.
func TestHandleStatusWorkCountsPreferListContextOverGoroutineList(t *testing.T) {
	state := newFakeState(t)
	store := &deadlineRecordingListStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store

	getStatus(t, state)

	if store.listContextCalls == 0 {
		t.Fatal("ListContext was never called; want the ContextLister path preferred over plain List")
	}
	if !store.sawDeadline {
		t.Fatal("ListContext ctx had no deadline; want it bound to statusStoreReadTimeout")
	}
	if store.listCalls != 0 {
		t.Fatalf("plain List called %d times, want 0 (ListContext should be used exclusively)", store.listCalls)
	}
}

// ctxIgnoringBlockingListStore claims beads.ContextLister but blocks and
// ignores its context, modeling the two shapes that satisfy the interface yet
// cannot honor a deadline: a nil-runnerContext BdStore (ListContext falls back
// to plain, non-cancellable List) and DoltliteReadStore.ListContext (a
// synchronous native SQLite scan). block is never closed by the store, so the
// only way statusListStoreWithTimeout can return is via its own guard.
type ctxIgnoringBlockingListStore struct {
	beads.Store
	block chan struct{}
}

func (s *ctxIgnoringBlockingListStore) ListContext(_ context.Context, _ beads.ListQuery) ([]beads.Bead, error) {
	<-s.block
	return nil, nil
}

// TestStatusListStoreWithTimeoutBoundsCtxIgnoringLister proves the guard around
// the ContextLister branch bounds the caller even when the implementation
// ignores ctx and blocks — the regression the removed goroutine+select guard
// reintroduced (ga-enpau9 / PR #3918 review, Blocker). The parent context's
// short deadline propagates into statusListStoreWithTimeout's derived timeout,
// so the select fires without waiting on the wedged ListContext.
func TestStatusListStoreWithTimeoutBoundsCtxIgnoringLister(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	store := &ctxIgnoringBlockingListStore{Store: beads.NewMemStore(), block: block}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := statusListStoreWithTimeout(ctx, store, beads.ListQuery{AllowScan: true})
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("statusListStoreWithTimeout returned nil error against a wedged ctx-ignoring ListContext; want a timeout error, never a silent empty result")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("statusListStoreWithTimeout blocked %s against a ctx-ignoring ListContext; want it bounded near the parent deadline", elapsed)
	}
}

// TestStatusSessionSnapshotThreadsDeadlineIntoContextLister proves
// statusSessionSnapshot's internal ctx (bound to statusStoreReadTimeout)
// reaches the city store's ListContext, not context.Background(), so a
// timeout actually cancels the backing query instead of merely abandoning
// the goroutine.
func TestStatusSessionSnapshotThreadsDeadlineIntoContextLister(t *testing.T) {
	state := newFakeState(t)
	store := &deadlineRecordingListStore{Store: beads.NewMemStore()}
	state.cityBeadStore = store

	getStatus(t, state)

	if store.listContextCalls == 0 {
		t.Fatal("ListContext was never called from the session snapshot path; want the ContextLister path preferred")
	}
	if !store.sawDeadline {
		t.Fatal("session snapshot ListContext ctx had no deadline; want it bound to statusStoreReadTimeout")
	}
}
