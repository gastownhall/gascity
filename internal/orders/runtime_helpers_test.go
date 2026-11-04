package orders

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
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
	// Relative to now, not a fixed date: the fallback only looks back a bounded
	// window, so a calendar date would age out of it and fail on its own.
	want := time.Now().Add(-36 * time.Hour)
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
	older := time.Now().Add(-60 * time.Hour)
	newer := time.Now().Add(-36 * time.Hour)
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
		// Recent, so only the subject mismatch can explain a zero result.
		Ts: time.Now().Add(-36 * time.Hour),
	})

	got, err := LastRunFuncWithEventFallback(storeFn, ep)("digest-generate")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("got %s, want zero (event is for a different order)", got)
	}
}

// The fallback's query must be bounded in time: an unbounded List makes every
// index miss walk every rotated archive. Bounded does not mean tail-limited —
// the lookback has to be wide enough to reach an order.fired that rotated into
// an archive, which is the long-cooldown case the fallback exists for. These
// tests bracket the window: 10 days back must be found, 400 days back must not.
// They hold for any lookback between those two.
const (
	withinEventFallbackLookback  = 10 * 24 * time.Hour
	beyondEventFallbackLookback  = 400 * 24 * time.Hour
	eventFallbackLookbackSubject = "digest-generate"
)

func TestLastRunFuncWithEventFallback_IgnoresEventsBeyondLookback(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	ep := events.NewFake()
	ep.Record(events.Event{
		Type:    events.OrderFired,
		Subject: eventFallbackLookbackSubject,
		Ts:      time.Now().Add(-beyondEventFallbackLookback),
	})

	got, err := LastRunFuncWithEventFallback(storeFn, ep)(eventFallbackLookbackSubject)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("got %s, want zero (event is %s old, past the bounded lookback)", got, beyondEventFallbackLookback)
	}
}

// rotatedEventLog records one order.fired at ts, rotates it into a gzip
// archive, and returns the live recorder (whose active log now holds only the
// rotation anchor). The event is readable only through the archive walk.
func rotatedEventLog(t *testing.T, subject string, ts time.Time) *events.FileRecorder {
	t.Helper()
	rec, err := events.NewFileRecorder(filepath.Join(t.TempDir(), "events.jsonl"), io.Discard)
	if err != nil {
		t.Fatalf("NewFileRecorder: %v", err)
	}
	t.Cleanup(func() { _ = rec.Close() })

	rec.Record(events.Event{Type: events.OrderFired, Actor: "controller", Subject: subject, Ts: ts})
	res, err := rec.ForceRotate()
	if err != nil {
		t.Fatalf("ForceRotate: %v", err)
	}
	if !res.Rotated {
		t.Fatalf("expected the event to rotate into an archive, got %+v", res)
	}
	select {
	case <-res.Done:
	case <-time.After(5 * time.Second):
		t.Fatal("archive compression did not finish within 5s")
	}
	return rec
}

// Guards the TRAP in the bead: a tail read would miss this event, because it
// lives in a rotated .gz, and every Fake-backed test would stay green.
func TestLastRunFuncWithEventFallback_FindsEventInRotatedArchiveWithinLookback(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	want := time.Now().Add(-withinEventFallbackLookback)
	rec := rotatedEventLog(t, eventFallbackLookbackSubject, want)

	got, err := LastRunFuncWithEventFallback(storeFn, rec)(eventFallbackLookbackSubject)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s (the order.fired sits in a rotated archive, inside the lookback)", got, want)
	}
}

func TestLastRunFuncWithEventFallback_IgnoresArchivedEventBeyondLookback(t *testing.T) {
	storeFn := func(string) (time.Time, error) { return time.Time{}, nil }
	rec := rotatedEventLog(t, eventFallbackLookbackSubject, time.Now().Add(-beyondEventFallbackLookback))

	got, err := LastRunFuncWithEventFallback(storeFn, rec)(eventFallbackLookbackSubject)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("got %s, want zero (archived event is %s old, past the bounded lookback)", got, beyondEventFallbackLookback)
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
