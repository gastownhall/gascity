package session

import (
	"context"
	"fmt"
	"reflect"
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

// seedCancelWaits creates pending waits (each with its own nudge) followed by
// terminal waits for sessionID and returns the pending and terminal wait IDs.
func seedCancelWaits(t *testing.T, store beads.Store, sessionID string, pending, terminal int) ([]string, []string) {
	t.Helper()
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
	sessionID := "gc-session"
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	pendingIDs, terminalIDs := seedCancelWaits(t, store.MemStore, sessionID, 25, 12)
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
	sessionID := "gc-session"
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	pendingIDs, terminalIDs := seedCancelWaits(t, store.MemStore, sessionID, 25, 12)

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
