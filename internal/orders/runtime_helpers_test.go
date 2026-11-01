package orders

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

type rowsErrorStore struct {
	*beads.MemStore
	rows []beads.Bead
	err  error
}

func (s *rowsErrorStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return s.rows, s.err
}

func ordersStoreOver(store beads.Store) *Store {
	return NewStore(beads.OrdersStore{Store: store})
}

func TestLastRunReturnsLatestRun(t *testing.T) {
	store := beads.NewMemStore()

	first, err := store.Create(beads.Bead{
		Title:  "order:digest",
		Status: "closed",
		Labels: []string{"order-run:digest", "order-tracking"},
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Millisecond)

	second, err := store.Create(beads.Bead{
		Title:  "order:digest",
		Status: "closed",
		Labels: []string{"order-run:digest", "order-tracking", "wisp-failed"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := ordersStoreOver(store).LastRun("digest")
	if err != nil {
		t.Fatalf("LastRun(): %v", err)
	}
	if !got.Equal(second.CreatedAt) {
		t.Fatalf("LastRun() = %s, want %s (latest run should remain authoritative)", got, second.CreatedAt)
	}
	if !second.CreatedAt.After(first.CreatedAt) {
		t.Fatalf("test setup invalid: second.CreatedAt=%s, first.CreatedAt=%s", second.CreatedAt, first.CreatedAt)
	}
}

func TestLastRunReturnsZeroWhenNoRunsExist(t *testing.T) {
	store := beads.NewMemStore()

	got, err := ordersStoreOver(store).LastRun("digest")
	if err != nil {
		t.Fatalf("LastRun(): %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("LastRun() = %s, want zero time", got)
	}
}

func TestLastRunUsesRowsFromPartialTierError(t *testing.T) {
	want := time.Date(2026, 5, 15, 7, 0, 0, 0, time.UTC)
	store := &rowsErrorStore{
		MemStore: beads.NewMemStore(),
		rows: []beads.Bead{{
			ID:        "run-1",
			Title:     "digest",
			CreatedAt: want,
			Labels:    []string{"order-run:digest", "order-tracking"},
		}},
		err: errors.New("wisps tier unavailable"),
	}

	got, err := ordersStoreOver(store).LastRun("digest")
	if err != nil {
		t.Fatalf("LastRun(): %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("LastRun() = %s, want %s from surviving rows", got, want)
	}
}

func TestLastRunFuncWithEventFallback_UsesStoreResult(t *testing.T) {
	want := time.Date(2026, 6, 9, 0, 23, 0, 0, time.UTC)
	storeFn := func(string) (time.Time, error) { return want, nil }
	ep := events.NewFake()
	// Event with a later timestamp — must NOT be chosen when store succeeds.
	ep.Record(events.Event{
		Type:    events.OrderFired,
		Subject: "digest-generate",
		Ts:      want.Add(time.Hour),
	})

	got, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("got %s, want store result %s", got, want)
	}
}

func TestLastRunFuncWithEventFallback_FallsBackToEvents(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	ep := events.NewFake()
	want := time.Date(2026, 6, 9, 0, 23, 0, 0, time.UTC)
	ep.Record(events.Event{
		Type:    events.OrderFired,
		Subject: "digest-generate",
		Ts:      want,
	})

	got, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("got %s, want event timestamp %s", got, want)
	}
}

func TestLastRunFuncWithEventFallback_FallsBackToLatestEvent(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	ep := events.NewFake()
	older := time.Date(2026, 6, 8, 0, 23, 0, 0, time.UTC)
	newer := time.Date(2026, 6, 9, 0, 23, 0, 0, time.UTC)
	ep.Record(events.Event{Type: events.OrderFired, Subject: "digest-generate", Ts: older})
	ep.Record(events.Event{Type: events.OrderFired, Subject: "digest-generate", Ts: newer})

	got, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(newer) {
		t.Fatalf("got %s, want newest event %s", got, newer)
	}
}

func TestLastRunFuncWithEventFallback_IgnoresOtherOrderEvents(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	ep := events.NewFake()
	ep.Record(events.Event{
		Type:    events.OrderFired,
		Subject: "other-order",
		Ts:      time.Date(2026, 6, 9, 0, 23, 0, 0, time.UTC),
	})

	got, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("got %s, want zero (event is for a different order)", got)
	}
}

func TestLastRunFuncWithEventFallback_NilProviderReturnsStoreResult(t *testing.T) {
	want := time.Date(2026, 6, 9, 0, 23, 0, 0, time.UTC)
	storeFn := func(string) (time.Time, error) { return want, nil }

	got, err := LastRunFuncWithEventFallback(storeFn, nil)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got, want)
	}
}

func TestLastRunFuncWithEventFallback_NilProviderZeroStore(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }

	got, err := LastRunFuncWithEventFallback(storeFn, nil)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("got %s, want zero (nil provider, no store result)", got)
	}
}

func TestLastRunFuncWithEventFallback_StoreErrorPropagated(t *testing.T) {
	wantErr := errors.New("store error")
	storeFn := func(string) (time.Time, error) { return time.Time{}, wantErr }
	ep := events.NewFake()

	_, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if !errors.Is(err, wantErr) {
		t.Fatalf("got err %v, want %v", err, wantErr)
	}
}

func TestLastRunFuncWithEventFallback_EventErrorFailsOpen(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	ep := events.NewFailFake()

	got, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if err != nil {
		t.Fatalf("expected nil error (fail-open), got: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("got %s, want zero time (fail-open on broken provider)", got)
	}
}

func TestCursorUsesRowsAndLogsPartialTierError(t *testing.T) {
	oldLogf := runtimeHelpersLogf
	var logs []string
	runtimeHelpersLogf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	t.Cleanup(func() {
		runtimeHelpersLogf = oldLogf
	})
	store := &rowsErrorStore{
		MemStore: beads.NewMemStore(),
		rows: []beads.Bead{{
			ID:     "run-1",
			Labels: []string{"order-run:digest", "order-tracking", "seq:42"},
		}},
		err: errors.New("wisps tier unavailable"),
	}

	got := ordersStoreOver(store).Cursor("digest")
	if got != 42 {
		t.Fatalf("Cursor() = %d, want 42 from surviving rows", got)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "partially failed") {
		t.Fatalf("logs = %#v, want partial failure log", logs)
	}
}

// TestLastRunAcrossReturnsMaxScope proves the federation helper takes the most
// recent run across scopes.
func TestLastRunAcrossReturnsMaxScope(t *testing.T) {
	early := beads.NewMemStore()
	if _, err := early.Create(beads.Bead{Title: "order:digest", Status: "closed", Labels: []string{"order-run:digest", "order-tracking"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	late := beads.NewMemStore()
	lateRun, err := late.Create(beads.Bead{Title: "order:digest", Status: "closed", Labels: []string{"order-run:digest", "order-tracking"}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := LastRunAcross([]*Store{ordersStoreOver(early), ordersStoreOver(late)})("digest")
	if err != nil {
		t.Fatalf("LastRunAcross(): %v", err)
	}
	if !got.Equal(lateRun.CreatedAt) {
		t.Fatalf("LastRunAcross() = %s, want %s (max across scopes)", got, lateRun.CreatedAt)
	}
}

// closedHistoryFailsStore serves every active-row read and fails, hard, every
// read that includes closed history: the shape of a backing whose closed rows
// are unreachable while the controller's cache still holds the open ones.
type closedHistoryFailsStore struct{ *beads.MemStore }

func (s closedHistoryFailsStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.IncludeClosed {
		return nil, errors.New("closed history unavailable")
	}
	return s.MemStore.List(q)
}

// Over a CachingStore, a hard backing error on the order-run history read must
// stay an error. Kills: a cached read that turns it into a partial result of
// the open rows the cache holds, which LastRun and Cursor would trust as
// surviving rows — a cooldown clock from an open run, a cursor that replays
// consumed events.
func TestLastRunAndCursorKeepAHardBackingErrorThroughACache(t *testing.T) {
	backing := beads.NewMemStore()
	if _, err := backing.Create(beads.Bead{Title: "order:digest", Labels: []string{"order-run:digest", "order-tracking", "seq:3"}}); err != nil {
		t.Fatal(err)
	}
	cache := beads.NewCachingStore(closedHistoryFailsStore{backing}, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	store := ordersStoreOver(cache)
	if got, err := store.LastRun("digest"); err == nil {
		t.Fatalf("LastRun() = %s, nil; a hard backing error was answered from the cache's open rows", got)
	}
	if got := store.Cursor("digest"); got != 0 {
		t.Fatalf("Cursor() = %d, want 0 (unread); a hard backing error was answered from the cache's open rows", got)
	}
}

// digestRunBead is a closed digest run stamped at a fixed CreatedAt, so a test
// orders runs across stores without waiting for the clock to tick.
func digestRunBead(id string, at time.Time, labels ...string) beads.Bead {
	return beads.Bead{
		ID:        id,
		Title:     "order:digest",
		Status:    "closed",
		Labels:    append([]string{"order-run:digest", "order-tracking"}, labels...),
		CreatedAt: at,
		UpdatedAt: at,
	}
}

// TestRecentRunsAcrossMergesNewestFirstAndTruncates proves the federated read
// interleaves each front door's runs by CreatedAt descending and cuts the merged
// list to limit, so a rig-scoped order's history is not hidden behind the city's.
func TestRecentRunsAcrossMergesNewestFirstAndTruncates(t *testing.T) {
	base := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	oldest := digestRunBead("gc-1", base)
	second := digestRunBead("gc-2", base.Add(time.Minute))
	third := digestRunBead("gc-3", base.Add(2*time.Minute))
	newest := digestRunBead("gc-4", base.Add(3*time.Minute), "wisp-failed")
	cityBacking := beads.NewMemStoreFrom(0, []beads.Bead{oldest, third}, nil)
	rigBacking := beads.NewMemStoreFrom(0, []beads.Bead{second, newest}, nil)

	across := RecentRunsAcross([]*Store{ordersStoreOver(cityBacking), nil, ordersStoreOver(rigBacking)})

	all, err := across("digest", 10)
	if err != nil {
		t.Fatalf("RecentRunsAcross(10): %v", err)
	}
	want := []string{newest.ID, third.ID, second.ID, oldest.ID}
	if len(all) != len(want) {
		t.Fatalf("RecentRunsAcross(10) = %d runs, want %d", len(all), len(want))
	}
	for i, id := range want {
		if all[i].ID != id {
			t.Fatalf("run[%d] = %s, want %s (newest-first by CreatedAt)", i, all[i].ID, id)
		}
	}

	top2, err := across("digest", 2)
	if err != nil {
		t.Fatalf("RecentRunsAcross(2): %v", err)
	}
	if len(top2) != 2 || top2[0].ID != newest.ID || top2[1].ID != third.ID {
		t.Fatalf("RecentRunsAcross(2) = %+v, want the two newest runs [%s %s]", top2, newest.ID, third.ID)
	}
}

// TestRecentRunsAcrossReturnsNothingWithoutRuns proves the zero case is an
// empty result, not an error, including when every entry is nil.
func TestRecentRunsAcrossReturnsNothingWithoutRuns(t *testing.T) {
	for name, stores := range map[string][]*Store{
		"no stores":  nil,
		"nil stores": {nil, nil},
		"empty":      {ordersStoreOver(beads.NewMemStore())},
	} {
		got, err := RecentRunsAcross(stores)("digest", 5)
		if err != nil {
			t.Fatalf("%s: RecentRunsAcross: %v", name, err)
		}
		if len(got) != 0 {
			t.Fatalf("%s: RecentRunsAcross = %+v, want none", name, got)
		}
	}
}

// TestRecentRunsAcrossPropagatesStoreError proves one failing front door aborts
// the read, the LastRunAcross convention: a partial history would let a streak
// walk reach a wrong verdict.
func TestRecentRunsAcrossPropagatesStoreError(t *testing.T) {
	wantErr := errors.New("tier unavailable")
	healthy := beads.NewMemStoreFrom(0, []beads.Bead{digestRunBead("gc-1", time.Now())}, nil)
	broken := &rowsErrorStore{MemStore: beads.NewMemStore(), err: wantErr}

	got, err := RecentRunsAcross([]*Store{ordersStoreOver(healthy), ordersStoreOver(broken)})("digest", 5)

	if !errors.Is(err, wantErr) {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
	if got != nil {
		t.Fatalf("runs = %+v, want nil alongside the error", got)
	}
}
