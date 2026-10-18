package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// ctxWaitStore is a MemStore that records every store call CancelWaits can
// make, so a test can assert how many calls a context let through and what
// each CloseAll carried. afterCloseAll runs once the real CloseAll returned,
// with the 1-based ordinal of the call, which is how a test cancels a context
// "inside" the Nth CloseAll: the call in flight finishes, later ones must not
// be issued.
type ctxWaitStore struct {
	*beads.MemStore
	calls         int
	closeAllIDs   [][]string
	closeAllMeta  []map[string]string
	afterCloseAll func(ordinal int)
}

func (s *ctxWaitStore) Get(id string) (beads.Bead, error) {
	s.calls++
	return s.MemStore.Get(id)
}

func (s *ctxWaitStore) Update(id string, opts beads.UpdateOpts) error {
	s.calls++
	return s.MemStore.Update(id, opts)
}

func (s *ctxWaitStore) Close(id string) error {
	s.calls++
	return s.MemStore.Close(id)
}

func (s *ctxWaitStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.calls++
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *ctxWaitStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.calls++
	return s.MemStore.List(query)
}

func (s *ctxWaitStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	s.calls++
	s.closeAllIDs = append(s.closeAllIDs, append([]string(nil), ids...))
	s.closeAllMeta = append(s.closeAllMeta, metadata)
	n, err := s.MemStore.CloseAll(ids, metadata)
	if s.afterCloseAll != nil {
		s.afterCloseAll(len(s.closeAllIDs))
	}
	return n, err
}

// ctxWaitSessionID is the session whose waits every test in this file seeds and cancels.
const ctxWaitSessionID = "gc-session"

// seedCtxWaits creates pending open waits and terminal-state open waits for
// ctxWaitSessionID, each with a distinct nudge ID, and returns the store.
func seedCtxWaits(t *testing.T, pending, terminal int) *ctxWaitStore {
	t.Helper()
	store := &ctxWaitStore{MemStore: beads.NewMemStore()}
	create := func(i int, state string) {
		t.Helper()
		if _, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%s-%d", state, i),
			Type:   WaitBeadType,
			Labels: []string{WaitBeadLabel, "session:" + ctxWaitSessionID},
			Metadata: map[string]string{
				"session_id": ctxWaitSessionID,
				"state":      state,
				"nudge_id":   fmt.Sprintf("nudge-%s-%d", state, i),
			},
		}); err != nil {
			t.Fatalf("create %s wait %d: %v", state, i, err)
		}
	}
	for i := 0; i < pending; i++ {
		create(i, waitStatePending)
	}
	for i := 0; i < terminal; i++ {
		create(i, waitStateCanceled)
	}
	return store
}

// countOpenWaits returns how many of ctxWaitSessionID's wait beads are still open.
func countOpenWaits(t *testing.T, store beads.Store) int {
	t.Helper()
	waits, err := store.List(beads.ListQuery{Label: "session:" + ctxWaitSessionID, IncludeClosed: true})
	if err != nil {
		t.Fatalf("list waits: %v", err)
	}
	open := 0
	for _, w := range waits {
		if IsWaitBead(w) && w.Status != "closed" {
			open++
		}
	}
	return open
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestCancelWaitsBackgroundCtxKeepsOneCloseAllPerIDClass pins that a context
// that can never be canceled leaves CancelWaits' call shape as it was: one
// CloseAll for the cancellable ids and one for the terminal ids per lookup
// batch, however many waits the batch holds.
func TestCancelWaitsBackgroundCtxKeepsOneCloseAllPerIDClass(t *testing.T) {
	const sessionID = ctxWaitSessionID
	pending, terminal := 2*waitCancelChunk+1, 3
	store := seedCtxWaits(t, pending, terminal)

	nudgeIDs, capped, err := waitStoreOver(store).CancelWaits(context.Background(), sessionID, time.Now().UTC())
	if err != nil {
		t.Fatalf("CancelWaits: %v", err)
	}
	if capped {
		t.Fatal("capped = true, want false")
	}
	if len(nudgeIDs) != pending+terminal {
		t.Fatalf("nudge ID count = %d, want %d", len(nudgeIDs), pending+terminal)
	}
	if got := len(store.closeAllIDs); got != 2 {
		t.Fatalf("CloseAll calls = %d (%v), want 2: one per id class", got, store.closeAllIDs)
	}
	if got := len(store.closeAllIDs[0]); got != pending {
		t.Fatalf("first CloseAll carried %d ids, want all %d cancellable waits", got, pending)
	}
	if store.closeAllMeta[0]["state"] != waitStateCanceled {
		t.Fatalf("first CloseAll metadata = %v, want state=%s", store.closeAllMeta[0], waitStateCanceled)
	}
	if got := len(store.closeAllIDs[1]); got != terminal {
		t.Fatalf("second CloseAll carried %d ids, want all %d terminal waits", got, terminal)
	}
	if got := countOpenWaits(t, store); got != 0 {
		t.Fatalf("%d waits still open, want 0", got)
	}
}

// TestCancelWaitsCancellableCtxChunksCloseAll pins that a context that can be
// canceled, but never is, reaches the same end state with the same nudge IDs
// and cap flag as the Background path, in CloseAll calls of at most
// waitCancelChunk ids.
func TestCancelWaitsCancellableCtxChunksCloseAll(t *testing.T) {
	const sessionID = ctxWaitSessionID
	pending, terminal := 2*waitCancelChunk+1, 3

	base := seedCtxWaits(t, pending, terminal)
	wantIDs, wantCapped, err := waitStoreOver(base).CancelWaits(context.Background(), sessionID, time.Now().UTC())
	if err != nil {
		t.Fatalf("Background CancelWaits: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := seedCtxWaits(t, pending, terminal)
	gotIDs, gotCapped, err := waitStoreOver(store).CancelWaits(ctx, sessionID, time.Now().UTC())
	if err != nil {
		t.Fatalf("cancellable CancelWaits: %v", err)
	}
	if gotCapped != wantCapped {
		t.Fatalf("capped = %v, want %v", gotCapped, wantCapped)
	}
	if !equalStrings(sortedCopy(gotIDs), sortedCopy(wantIDs)) {
		t.Fatalf("nudge IDs = %v, want %v", sortedCopy(gotIDs), sortedCopy(wantIDs))
	}
	if len(store.closeAllIDs) <= 1 {
		t.Fatalf("CloseAll calls = %d, want more than one chunked call", len(store.closeAllIDs))
	}
	closed := 0
	for i, ids := range store.closeAllIDs {
		if len(ids) > waitCancelChunk {
			t.Fatalf("CloseAll #%d carried %d ids, want at most %d", i+1, len(ids), waitCancelChunk)
		}
		closed += len(ids)
	}
	if closed != pending+terminal {
		t.Fatalf("CloseAll calls closed %d ids in total, want %d", closed, pending+terminal)
	}
	if got := countOpenWaits(t, store); got != 0 {
		t.Fatalf("%d waits still open, want 0", got)
	}
}

// TestCancelWaitsPreCancelledCtxMakesNoStoreCalls pins that a context that is
// already done lets no store call through, closes nothing and reports the
// context's error.
func TestCancelWaitsPreCancelledCtxMakesNoStoreCalls(t *testing.T) {
	const sessionID = ctxWaitSessionID
	store := seedCtxWaits(t, 5, 2)
	callsAfterSeed := store.calls

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := waitStoreOver(store).CancelWaits(ctx, sessionID, time.Now().UTC())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is context.Canceled", err)
	}
	if got := store.calls - callsAfterSeed; got != 0 {
		t.Fatalf("store calls with a pre-canceled ctx = %d, want 0", got)
	}
	if got := countOpenWaits(t, store.MemStore); got != 7 {
		t.Fatalf("open waits = %d, want 7 untouched", got)
	}
}

// TestCancelWaitsCancelInsideCloseAllLeavesLaterChunksOpen pins the cut-off
// contract: a context ended inside the Nth CloseAll lets that call finish,
// issues no later chunk, wraps the context error, and still returns the nudge
// ID of every wait it had collected, including those in chunks it did not close.
func TestCancelWaitsCancelInsideCloseAllLeavesLaterChunksOpen(t *testing.T) {
	const sessionID = ctxWaitSessionID
	pending := 2*waitCancelChunk + 1
	store := seedCtxWaits(t, pending, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.afterCloseAll = func(ordinal int) {
		if ordinal == 2 {
			cancel()
		}
	}

	nudgeIDs, _, err := waitStoreOver(store).CancelWaits(ctx, sessionID, time.Now().UTC())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want errors.Is context.Canceled", err)
	}
	if got := len(store.closeAllIDs); got != 2 {
		t.Fatalf("CloseAll calls = %d, want 2: none after the canceling call", got)
	}
	if got := countOpenWaits(t, store); got != pending-2*waitCancelChunk {
		t.Fatalf("open waits = %d, want %d left in the unclosed chunk", got, pending-2*waitCancelChunk)
	}
	if len(nudgeIDs) != pending {
		t.Fatalf("returned %d nudge IDs, want all %d including the unclosed chunk's", len(nudgeIDs), pending)
	}
}
