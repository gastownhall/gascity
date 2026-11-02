package session

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestCancelWaitsTakesContextFirstParameter pins the SESSION-RUNTIME-034
// contract that wait cancellation is context-first: (*Store).CancelWaits takes
// a context.Context as its first parameter, so a caller can bound the store
// work a cancellation issues between calls. It reads the method by reflection
// so the contract's RED run fails on this assertion instead of on compilation.
func TestCancelWaitsTakesContextFirstParameter(t *testing.T) {
	method, ok := reflect.TypeOf(&Store{}).MethodByName("CancelWaits")
	if !ok {
		t.Fatal("(*Store).CancelWaits: method not found")
	}
	ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
	if method.Type.NumIn() != 4 || method.Type.In(1) != ctxType {
		t.Fatalf("(*Store).CancelWaits does not take a context.Context as its first parameter")
	}
}

// ctxTestSessionID is the single session these tests cancel waits for.
const ctxTestSessionID = "gc-1"

// ctxCancelingStore cancels a caller-supplied context from inside its store
// calls — after the nth List or after the nth CloseAll — so a context stop
// lands deterministically between CancelWaits' cancellation checkpoints. The
// call in progress always finishes; the counts also prove which store calls a
// pass issued at all.
type ctxCancelingStore struct {
	*beads.MemStore
	cancel         func()
	listCalls      int
	listCallsAfter int // cancel once this many List calls returned (0 = never)
	closeAllCalls  int
	closeAllAfter  int // cancel once this many CloseAll calls returned (0 = never)
}

func (s *ctxCancelingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	waits, err := s.MemStore.List(q)
	s.listCalls++
	if s.listCallsAfter > 0 && s.listCalls >= s.listCallsAfter {
		s.cancel()
	}
	return waits, err
}

func (s *ctxCancelingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	n, err := s.MemStore.CloseAll(ids, metadata)
	s.closeAllCalls++
	if s.closeAllAfter > 0 && s.closeAllCalls >= s.closeAllAfter {
		s.cancel()
	}
	return n, err
}

// seedCtxTestWaits creates n pending waits with distinct nudge IDs for one
// session.
func seedCtxTestWaits(t *testing.T, mem *beads.MemStore, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := mem.Create(beads.Bead{
			Title:  fmt.Sprintf("wait-%d", i),
			Type:   WaitBeadType,
			Labels: []string{WaitBeadLabel, "session:" + ctxTestSessionID},
			Metadata: map[string]string{
				"session_id": ctxTestSessionID,
				"state":      waitStatePending,
				"nudge_id":   fmt.Sprintf("nudge-%d", i),
			},
		}); err != nil {
			t.Fatalf("create wait %d: %v", i, err)
		}
	}
}

// openCtxTestWaits counts the session's still-open wait beads.
func openCtxTestWaits(t *testing.T, mem *beads.MemStore) int {
	t.Helper()
	waits, err := mem.List(beads.ListQuery{Label: "session:" + ctxTestSessionID, Limit: 5000})
	if err != nil {
		t.Fatalf("list waits: %v", err)
	}
	open := 0
	for _, wait := range waits {
		if IsWaitBead(wait) && wait.Status == "open" {
			open++
		}
	}
	return open
}

func TestCancelWaitsPrecancelledContextMakesNoStoreCall(t *testing.T) {
	mem := beads.NewMemStore()
	seedCtxTestWaits(t, mem, 3)
	store := &ctxCancelingStore{MemStore: mem}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	nudgeIDs, capped, err := waitStoreOver(store).CancelWaits(ctx, ctxTestSessionID, waitStoreNow)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CancelWaits error = %v, want context.Canceled", err)
	}
	if !strings.Contains(err.Error(), ctxTestSessionID) {
		t.Fatalf("CancelWaits error = %q, want it to name the session", err.Error())
	}
	if len(nudgeIDs) != 0 {
		t.Fatalf("nudgeIDs = %#v, want none observed before any store call", nudgeIDs)
	}
	if capped {
		t.Fatal("capped = true, want false: no lookup ran")
	}
	if store.listCalls != 0 || store.closeAllCalls != 0 {
		t.Fatalf("pre-canceled context made %d list and %d CloseAll store calls, want none", store.listCalls, store.closeAllCalls)
	}
}

func TestCancelWaitsPreexpiredDeadlineMakesNoStoreCall(t *testing.T) {
	mem := beads.NewMemStore()
	seedCtxTestWaits(t, mem, 3)
	store := &ctxCancelingStore{MemStore: mem}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
	defer cancel()

	_, _, err := waitStoreOver(store).CancelWaits(ctx, ctxTestSessionID, waitStoreNow)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CancelWaits error = %v, want context.DeadlineExceeded", err)
	}
	if !strings.Contains(err.Error(), ctxTestSessionID) {
		t.Fatalf("CancelWaits error = %q, want it to name the session", err.Error())
	}
	if store.listCalls != 0 || store.closeAllCalls != 0 {
		t.Fatalf("pre-expired deadline made %d list and %d CloseAll store calls, want none", store.listCalls, store.closeAllCalls)
	}
}

func TestCancelWaitsContextStopAfterLookupReturnsObservedNudgeIDs(t *testing.T) {
	mem := beads.NewMemStore()
	seedCtxTestWaits(t, mem, 5)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &ctxCancelingStore{MemStore: mem, cancel: cancel, listCallsAfter: 1}

	nudgeIDs, capped, err := waitStoreOver(store).CancelWaits(ctx, ctxTestSessionID, waitStoreNow)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CancelWaits error = %v, want the context stop wrapped", err)
	}
	if !strings.Contains(err.Error(), ctxTestSessionID) {
		t.Fatalf("CancelWaits error = %q, want it to name the session", err.Error())
	}
	if len(nudgeIDs) != 5 {
		t.Fatalf("nudgeIDs = %d IDs, want all 5 observed on the completed lookup page", len(nudgeIDs))
	}
	if capped {
		t.Fatal("capped = true, want false: the single lookup page was not capped")
	}
	if store.closeAllCalls != 0 {
		t.Fatalf("CloseAll calls after the context stop = %d, want none: no further store call may start", store.closeAllCalls)
	}
	if open := openCtxTestWaits(t, mem); open != 5 {
		t.Fatalf("open waits = %d, want all 5 still open", open)
	}
}

func TestCancelWaitsContextStopBeforeCappedRelookupKeepsFirstPageEvidence(t *testing.T) {
	mem := beads.NewMemStore()
	seedCtxTestWaits(t, mem, SessionWaitLookupLimit+1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel after the first capped page's close batches all completed, so the
	// stop lands on the re-lookup checkpoint.
	firstPageBatches := SessionWaitLookupLimit / waitCancelCloseBatchLimit
	store := &ctxCancelingStore{MemStore: mem, cancel: cancel, closeAllAfter: firstPageBatches}

	nudgeIDs, capped, err := waitStoreOver(store).CancelWaits(ctx, ctxTestSessionID, waitStoreNow)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CancelWaits error = %v, want the context stop wrapped", err)
	}
	if !strings.Contains(err.Error(), ctxTestSessionID) {
		t.Fatalf("CancelWaits error = %q, want it to name the session", err.Error())
	}
	if !capped {
		t.Fatal("capped = false, want true: the first lookup page was capped and that must survive the stop")
	}
	if len(nudgeIDs) != SessionWaitLookupLimit {
		t.Fatalf("nudgeIDs = %d IDs, want the capped first page's %d", len(nudgeIDs), SessionWaitLookupLimit)
	}
	if store.closeAllCalls != firstPageBatches {
		t.Fatalf("CloseAll calls = %d, want %d: the re-lookup after the stop must not start", store.closeAllCalls, firstPageBatches)
	}
	if open := openCtxTestWaits(t, mem); open != 1 {
		t.Fatalf("open waits = %d, want the 1 wait outside the first capped page", open)
	}
}

var errCtxTestCloseWaitBoom = errors.New("close wait boom")

// failingCloseAllStore fails every batch close with a fixed bare store error.
type failingCloseAllStore struct {
	*beads.MemStore
}

func (s failingCloseAllStore) CloseAll(_ []string, _ map[string]string) (int, error) {
	return 0, errCtxTestCloseWaitBoom
}

func TestCancelWaitsPreservesBareStoreErrorsUnderLiveContext(t *testing.T) {
	mem := beads.NewMemStore()
	seedCtxTestWaits(t, mem, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	nudgeIDs, capped, err := waitStoreOver(failingCloseAllStore{MemStore: mem}).CancelWaits(ctx, ctxTestSessionID, waitStoreNow)
	if !errors.Is(err, errCtxTestCloseWaitBoom) {
		t.Fatalf("CancelWaits error = %v, want the bare store error %v", err, errCtxTestCloseWaitBoom)
	}
	if len(nudgeIDs) != 2 {
		t.Fatalf("nudgeIDs = %d IDs, want the 2 observed before the close attempt", len(nudgeIDs))
	}
	if capped {
		t.Fatal("capped = true, want false: the store error is not a cap")
	}
}

// ctxBatchSeed builds n pending waits with distinct nudge IDs plus m terminal
// ones, as verbatim seed beads for a recording store.
func ctxBatchSeed(n, m int) []beads.Bead {
	seed := make([]beads.Bead, 0, n+m)
	for i := 0; i < n; i++ {
		seed = append(seed, waitBeadFixture(
			fmt.Sprintf("wp-%d", i), "open", ctxTestSessionID,
			map[string]string{"state": waitStatePending, "nudge_id": fmt.Sprintf("nudge-%d", i)}))
	}
	for i := 0; i < m; i++ {
		seed = append(seed, waitBeadFixture(
			fmt.Sprintf("wt-%d", i), "open", ctxTestSessionID,
			map[string]string{"state": waitStateCanceled, "nudge_id": fmt.Sprintf("nudge-t-%d", i)}))
	}
	return seed
}

// assertBatches checks the recorded CloseAll calls of one ID class: every call
// carries the wanted metadata and at most the batch limit's IDs, and together
// they cover wantIDs.
func assertBatches(t *testing.T, calls []beadstest.RecordedCall, wantMetadata map[string]string, wantIDs int) {
	t.Helper()
	gotIDs := 0
	for _, call := range calls {
		if len(call.IDs) > 10 {
			t.Fatalf("CloseAll batch of %d IDs, want at most 10 per call under a cancellable context", len(call.IDs))
		}
		if !maps.Equal(call.Metadata, wantMetadata) {
			t.Fatalf("CloseAll metadata = %#v, want %#v", call.Metadata, wantMetadata)
		}
		gotIDs += len(call.IDs)
	}
	if gotIDs != wantIDs {
		t.Fatalf("CloseAll batches covered %d IDs, want %d", gotIDs, wantIDs)
	}
}

func TestCancelWaitsCancellableContextClosesInBatchesOfTen(t *testing.T) {
	s, rec := recordingWaitStore(t, ctxBatchSeed(25, 5)...)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	nudgeIDs, _, err := s.CancelWaits(ctx, ctxTestSessionID, waitStoreNow)
	if err != nil {
		t.Fatalf("CancelWaits: %v", err)
	}
	if len(nudgeIDs) != 30 {
		t.Fatalf("nudgeIDs = %d, want every nudge from the 25 pending and 5 terminal waits", len(nudgeIDs))
	}
	canceledStamp := map[string]string{
		"state":       waitStateCanceled,
		"canceled_at": waitStoreNow.UTC().Format(time.RFC3339),
	}
	var pending, terminal []beadstest.RecordedCall
	for _, call := range rec.CallsForOp("CloseAll") {
		if call.Metadata == nil {
			terminal = append(terminal, call)
		} else {
			pending = append(pending, call)
		}
	}
	assertBatches(t, pending, canceledStamp, 25)
	assertBatches(t, terminal, nil, 5)
}

func TestCancelWaitsBackgroundKeepsOneClosePerClassPerPage(t *testing.T) {
	s, rec := recordingWaitStore(t, ctxBatchSeed(25, 5)...)

	if _, _, err := s.CancelWaits(context.Background(), ctxTestSessionID, waitStoreNow); err != nil {
		t.Fatalf("CancelWaits: %v", err)
	}
	calls := rec.CallsForOp("CloseAll")
	if len(calls) != 2 {
		t.Fatalf("CloseAll calls = %d, want 2: one per nonempty ID class per lookup page under Background", len(calls))
	}
	canceledStamp := map[string]string{
		"state":       waitStateCanceled,
		"canceled_at": waitStoreNow.UTC().Format(time.RFC3339),
	}
	var pending, terminal []beadstest.RecordedCall
	for _, call := range calls {
		if call.Metadata == nil {
			terminal = append(terminal, call)
		} else {
			pending = append(pending, call)
		}
	}
	if len(pending) != 1 || len(pending[0].IDs) != 25 || !maps.Equal(pending[0].Metadata, canceledStamp) {
		t.Fatalf("pending-class CloseAll calls = %#v, want one call with all 25 IDs and the canceled stamp", pending)
	}
	if len(terminal) != 1 || len(terminal[0].IDs) != 5 {
		t.Fatalf("terminal-class CloseAll calls = %#v, want one call with all 5 IDs", terminal)
	}
}

func TestCancelWaitsContextStopMidBatchesReturnsAllNudgeIDsAndLeavesUnclosed(t *testing.T) {
	mem := beads.NewMemStore()
	seedCtxTestWaits(t, mem, 25)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := &ctxCancelingStore{MemStore: mem, cancel: cancel, closeAllAfter: 1}

	nudgeIDs, capped, err := waitStoreOver(store).CancelWaits(ctx, ctxTestSessionID, waitStoreNow)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CancelWaits error = %v, want the context stop wrapped", err)
	}
	if !strings.Contains(err.Error(), ctxTestSessionID) {
		t.Fatalf("CancelWaits error = %q, want it to name the session", err.Error())
	}
	if len(nudgeIDs) != 25 {
		t.Fatalf("nudgeIDs = %d, want all 25 observed on the page, the unclosed batches included", len(nudgeIDs))
	}
	if capped {
		t.Fatal("capped = true, want false: the single lookup page was not capped")
	}
	if store.closeAllCalls != 1 {
		t.Fatalf("CloseAll calls = %d, want 1: the batch after the stop must not start", store.closeAllCalls)
	}
	if open := openCtxTestWaits(t, mem); open != 15 {
		t.Fatalf("open waits = %d, want the 15 waits whose batches never started", open)
	}
}
