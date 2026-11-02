package session

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// contextWaitCanceler is the context-first CancelWaits signature. Tests reach
// it through an interface assertion so a store without it fails on an
// assertion rather than on compilation.
type contextWaitCanceler interface {
	CancelWaits(ctx context.Context, sessionID string, now time.Time) ([]string, bool, error)
}

func contextCancelWaits(t *testing.T, s *Store) contextWaitCanceler {
	t.Helper()
	c, ok := any(s).(contextWaitCanceler)
	if !ok {
		t.Fatal("(*Store).CancelWaits does not take a context.Context as its first parameter")
	}
	return c
}

// cancelWaitsCloseCall is one recorded CloseAll call.
type cancelWaitsCloseCall struct {
	ids      []string
	metadata map[string]string
}

// cancelWaitsOpStore records the wait lookups and CloseAll calls CancelWaits
// issues, running optional hooks after each so a test can cancel its context
// at a chosen point.
type cancelWaitsOpStore struct {
	*beads.MemStore
	lists      int
	closes     []cancelWaitsCloseCall
	afterList  func(lists int)
	afterClose func(closes []cancelWaitsCloseCall)
}

func (s *cancelWaitsOpStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	items, err := s.MemStore.List(query)
	s.lists++
	if s.afterList != nil {
		s.afterList(s.lists)
	}
	return items, err
}

func (s *cancelWaitsOpStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	call := cancelWaitsCloseCall{ids: append([]string(nil), ids...)}
	if metadata != nil {
		call.metadata = make(map[string]string, len(metadata))
		for k, v := range metadata {
			call.metadata[k] = v
		}
	}
	n, err := s.MemStore.CloseAll(ids, metadata)
	s.closes = append(s.closes, call)
	if s.afterClose != nil {
		s.afterClose(s.closes)
	}
	return n, err
}

// cancelWaitsTestSession is the session every CancelWaits context test seeds.
const cancelWaitsTestSession = "gc-session"

// seedCancelWaits creates pending waits (each with its own nudge) followed by
// terminal waits for cancelWaitsTestSession and returns the pending and terminal wait IDs.
func seedCancelWaits(t *testing.T, store beads.Store, pending, terminal int) ([]string, []string) {
	t.Helper()
	sessionID := cancelWaitsTestSession
	create := func(i int, state string) string {
		b, err := store.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%d", i),
			Type:   WaitBeadType,
			Labels: []string{WaitBeadLabel, "session:" + sessionID},
			Metadata: map[string]string{
				"session_id": sessionID,
				"state":      state,
				"nudge_id":   fmt.Sprintf("nudge-%d", i),
			},
		})
		if err != nil {
			t.Fatalf("create wait %d: %v", i, err)
		}
		return b.ID
	}
	pendingIDs := make([]string, 0, pending)
	for i := 0; i < pending; i++ {
		pendingIDs = append(pendingIDs, create(i, "pending"))
	}
	terminalIDs := make([]string, 0, terminal)
	for i := 0; i < terminal; i++ {
		terminalIDs = append(terminalIDs, create(pending+i, waitStateExpired))
	}
	return pendingIDs, terminalIDs
}

func closeCallSizes(calls []cancelWaitsCloseCall) []int {
	sizes := make([]int, 0, len(calls))
	for _, call := range calls {
		sizes = append(sizes, len(call.ids))
	}
	return sizes
}

func TestCancelWaitsBatchesClosesUnderCancellableContext(t *testing.T) {
	store := &cancelWaitsOpStore{MemStore: beads.NewMemStore()}
	sessionID := cancelWaitsTestSession
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	pendingIDs, terminalIDs := seedCancelWaits(t, store.MemStore, 25, 12)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nudgeIDs, capped, err := contextCancelWaits(t, waitStoreOver(store)).CancelWaits(ctx, sessionID, now)
	if err != nil {
		t.Fatalf("CancelWaits: %v", err)
	}
	if capped {
		t.Fatal("capped = true, want false")
	}
	if len(nudgeIDs) != 37 {
		t.Fatalf("nudge ID count = %d, want 37", len(nudgeIDs))
	}
	if got, want := closeCallSizes(store.closes), []int{10, 10, 5, 10, 2}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CloseAll batch sizes = %v, want %v", got, want)
	}
	wantCanceled := map[string]string{"state": waitStateCanceled, "canceled_at": now.Format(time.RFC3339)}
	var closedPending, closedTerminal []string
	for i, call := range store.closes {
		if i < 3 {
			if !reflect.DeepEqual(call.metadata, wantCanceled) {
				t.Fatalf("pending batch %d metadata = %v, want %v", i, call.metadata, wantCanceled)
			}
			closedPending = append(closedPending, call.ids...)
			continue
		}
		if call.metadata != nil {
			t.Fatalf("terminal batch %d metadata = %v, want nil", i, call.metadata)
		}
		closedTerminal = append(closedTerminal, call.ids...)
	}
	if !sameIDSet(closedPending, pendingIDs) {
		t.Fatalf("closed pending IDs = %v, want %v", closedPending, pendingIDs)
	}
	if !sameIDSet(closedTerminal, terminalIDs) {
		t.Fatalf("closed terminal IDs = %v, want %v", closedTerminal, terminalIDs)
	}
}

func TestCancelWaitsBackgroundClosesEachClassOnce(t *testing.T) {
	store := &cancelWaitsOpStore{MemStore: beads.NewMemStore()}
	sessionID := cancelWaitsTestSession
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	pendingIDs, terminalIDs := seedCancelWaits(t, store.MemStore, 25, 12)

	if _, _, err := contextCancelWaits(t, waitStoreOver(store)).CancelWaits(context.Background(), sessionID, now); err != nil {
		t.Fatalf("CancelWaits: %v", err)
	}
	if got, want := closeCallSizes(store.closes), []int{25, 12}; !reflect.DeepEqual(got, want) {
		t.Fatalf("CloseAll call sizes = %v, want %v", got, want)
	}
	if !sameIDSet(store.closes[0].ids, pendingIDs) || store.closes[0].metadata["state"] != waitStateCanceled {
		t.Fatalf("first CloseAll = %+v, want every pending wait canceled", store.closes[0])
	}
	if !sameIDSet(store.closes[1].ids, terminalIDs) || store.closes[1].metadata != nil {
		t.Fatalf("second CloseAll = %+v, want every terminal wait with nil metadata", store.closes[1])
	}
}

func sameIDSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[string]int, len(want))
	for _, id := range want {
		seen[id]++
	}
	for _, id := range got {
		if seen[id] == 0 {
			return false
		}
		seen[id]--
	}
	return true
}

// assertWaitCancelContextError checks that err wraps want and names
// cancelWaitsTestSession.
func assertWaitCancelContextError(t *testing.T, err, want error) {
	t.Helper()
	sessionID := cancelWaitsTestSession
	if !errors.Is(err, want) {
		t.Fatalf("CancelWaits error = %v, want it to wrap %v", err, want)
	}
	if !strings.Contains(err.Error(), sessionID) {
		t.Fatalf("CancelWaits error = %q, want it to name session %s", err.Error(), sessionID)
	}
}

func countOpenWaits(t *testing.T, store beads.Store, sessionID string) int {
	t.Helper()
	items, err := store.List(beads.ListQuery{Status: "open", Label: "session:" + sessionID})
	if err != nil {
		t.Fatalf("list open waits: %v", err)
	}
	return len(items)
}

func TestCancelWaitsPreCanceledContextMakesNoStoreCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  func() context.Context
		want error
	}{
		{name: "canceled", want: context.Canceled, ctx: func() context.Context {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx
		}},
		{name: "deadline", want: context.DeadlineExceeded, ctx: func() context.Context {
			ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
			t.Cleanup(cancel)
			return ctx
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &cancelWaitsOpStore{MemStore: beads.NewMemStore()}
			sessionID := cancelWaitsTestSession
			seedCancelWaits(t, store.MemStore, 3, 1)

			nudgeIDs, capped, err := contextCancelWaits(t, waitStoreOver(store)).CancelWaits(tc.ctx(), sessionID, time.Now().UTC())
			assertWaitCancelContextError(t, err, tc.want)
			if store.lists != 0 || len(store.closes) != 0 {
				t.Fatalf("store calls = %d lookups, %d closes, want none", store.lists, len(store.closes))
			}
			if len(nudgeIDs) != 0 || capped {
				t.Fatalf("nudgeIDs/capped = %v/%v, want none/false", nudgeIDs, capped)
			}
		})
	}
}

func TestCancelWaitsStopsAfterLookupWithAllObservedNudges(t *testing.T) {
	store := &cancelWaitsOpStore{MemStore: beads.NewMemStore()}
	sessionID := cancelWaitsTestSession
	seedCancelWaits(t, store.MemStore, 25, 0)
	// Five more waits share the first five nudges; the result deduplicates them.
	for i := 0; i < 5; i++ {
		if _, err := store.Create(beads.Bead{
			Title:    fmt.Sprintf("dup-wait-%d", i),
			Type:     WaitBeadType,
			Labels:   []string{WaitBeadLabel, "session:" + sessionID},
			Metadata: map[string]string{"session_id": sessionID, "state": "pending", "nudge_id": fmt.Sprintf("nudge-%d", i)},
		}); err != nil {
			t.Fatalf("create duplicate wait %d: %v", i, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.afterList = func(int) { cancel() }

	nudgeIDs, _, err := contextCancelWaits(t, waitStoreOver(store)).CancelWaits(ctx, sessionID, time.Now().UTC())
	assertWaitCancelContextError(t, err, context.Canceled)
	if store.lists != 1 || len(store.closes) != 0 {
		t.Fatalf("store calls = %d lookups, %d closes, want 1 lookup and no close", store.lists, len(store.closes))
	}
	if len(nudgeIDs) != 25 {
		t.Fatalf("nudge ID count = %d, want 25 distinct", len(nudgeIDs))
	}
	if got := countOpenWaits(t, store.MemStore, sessionID); got != 30 {
		t.Fatalf("open waits = %d, want 30", got)
	}
}

func TestCancelWaitsStopsAfterCloseBatchWithAllObservedNudges(t *testing.T) {
	store := &cancelWaitsOpStore{MemStore: beads.NewMemStore()}
	sessionID := cancelWaitsTestSession
	seedCancelWaits(t, store.MemStore, 25, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.afterClose = func([]cancelWaitsCloseCall) { cancel() }

	nudgeIDs, capped, err := contextCancelWaits(t, waitStoreOver(store)).CancelWaits(ctx, sessionID, time.Now().UTC())
	assertWaitCancelContextError(t, err, context.Canceled)
	if got := closeCallSizes(store.closes); !reflect.DeepEqual(got, []int{10}) {
		t.Fatalf("CloseAll batch sizes = %v, want [10]", got)
	}
	if len(nudgeIDs) != 25 || capped {
		t.Fatalf("nudge ID count/capped = %d/%v, want 25/false", len(nudgeIDs), capped)
	}
	if got := countOpenWaits(t, store.MemStore, sessionID); got != 15 {
		t.Fatalf("open waits = %d, want 15", got)
	}
}

func TestCancelWaitsStopsBeforeTerminalClass(t *testing.T) {
	store := &cancelWaitsOpStore{MemStore: beads.NewMemStore()}
	sessionID := cancelWaitsTestSession
	_, terminalIDs := seedCancelWaits(t, store.MemStore, 3, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.afterClose = func([]cancelWaitsCloseCall) { cancel() }

	nudgeIDs, _, err := contextCancelWaits(t, waitStoreOver(store)).CancelWaits(ctx, sessionID, time.Now().UTC())
	assertWaitCancelContextError(t, err, context.Canceled)
	if got := closeCallSizes(store.closes); !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("CloseAll batch sizes = %v, want only the pending class [3]", got)
	}
	if len(nudgeIDs) != 5 {
		t.Fatalf("nudge ID count = %d, want 5", len(nudgeIDs))
	}
	for _, id := range terminalIDs {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("get terminal wait %s: %v", id, err)
		}
		if b.Status != "open" {
			t.Fatalf("terminal wait %s status = %q, want open", id, b.Status)
		}
	}
}

func TestCancelWaitsStopsBeforeNextCappedLookup(t *testing.T) {
	store := &cancelWaitsOpStore{MemStore: beads.NewMemStore()}
	sessionID := cancelWaitsTestSession
	seedCancelWaits(t, store.MemStore, SessionWaitLookupLimit+1, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	closed := 0
	store.afterClose = func(calls []cancelWaitsCloseCall) {
		closed += len(calls[len(calls)-1].ids)
		if closed == SessionWaitLookupLimit {
			cancel()
		}
	}

	nudgeIDs, capped, err := contextCancelWaits(t, waitStoreOver(store)).CancelWaits(ctx, sessionID, time.Now().UTC())
	assertWaitCancelContextError(t, err, context.Canceled)
	if beads.IsLookupLimitError(err) {
		t.Fatalf("CancelWaits error = %v, want the context error, not the lookup limit", err)
	}
	if !capped {
		t.Fatal("capped = false, want the first capped page recorded")
	}
	if store.lists != 1 {
		t.Fatalf("lookups = %d, want 1 (no lookup after cancellation)", store.lists)
	}
	if len(nudgeIDs) != SessionWaitLookupLimit {
		t.Fatalf("nudge ID count = %d, want %d", len(nudgeIDs), SessionWaitLookupLimit)
	}
	if got := countOpenWaits(t, store.MemStore, sessionID); got != 1 {
		t.Fatalf("open waits = %d, want 1", got)
	}
}

func TestCancelWaitsKeepsStoreErrorIdentityUnderLiveContext(t *testing.T) {
	mem := beads.NewMemStore()
	sessionID := cancelWaitsTestSession
	pendingIDs, _ := seedCancelWaits(t, mem, 2, 0)
	store := cancelWaitMetadataFailStore{MemStore: mem, failID: pendingIDs[0]}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nudgeIDs, _, err := contextCancelWaits(t, waitStoreOver(store)).CancelWaits(ctx, sessionID, time.Now().UTC())
	if err == nil || err.Error() != "cancel wait metadata failed" {
		t.Fatalf("CancelWaits error = %v, want the bare store error", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("CancelWaits error = %v, want no context error", err)
	}
	if len(nudgeIDs) != 2 {
		t.Fatalf("nudge ID count = %d, want 2", len(nudgeIDs))
	}
}
