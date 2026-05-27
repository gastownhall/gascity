package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/storehealth"
)

type testIndexedRowCounterStore struct {
	*beads.MemStore
	count int
	err   error
	calls int
	query beads.ListQuery
}

func (s *testIndexedRowCounterStore) CountIndexed(_ context.Context, query beads.ListQuery) (int, error) {
	s.calls++
	s.query = query
	return s.count, s.err
}

func TestCachedStoreHealthServesMemoized(t *testing.T) {
	var calls int
	want := &StatusStoreHealth{Path: "/c/.beads/dolt", SizeBytes: 123}
	s := &Server{}
	s.storeHealthComputer = func() *StatusStoreHealth {
		calls++
		return want
	}

	now := time.Unix(1_000_000, 0)
	got := s.cachedStoreHealth(now)
	if got != want {
		t.Fatalf("cachedStoreHealth = %+v, want %+v", got, want)
	}
	if calls != 1 {
		t.Fatalf("computer called %d times, want 1", calls)
	}

	// Within TTL: no recomputation.
	got2 := s.cachedStoreHealth(now.Add(storeHealthCacheTTL - time.Second))
	if got2 != want {
		t.Fatalf("second cachedStoreHealth = %+v, want %+v", got2, want)
	}
	if calls != 1 {
		t.Fatalf("computer called %d times within TTL, want 1", calls)
	}
}

func TestCachedStoreHealthReturnsStaleWhileRefreshingAfterTTL(t *testing.T) {
	var calls int
	refreshStarted := make(chan struct{})
	allowRefresh := make(chan struct{})
	s := &Server{}
	s.storeHealthComputer = func() *StatusStoreHealth {
		calls++
		if calls == 2 {
			close(refreshStarted)
			<-allowRefresh
		}
		return &StatusStoreHealth{SizeBytes: int64(calls)}
	}

	now := time.Unix(1_000_000, 0)
	_ = s.cachedStoreHealth(now)
	later := now.Add(storeHealthCacheTTL + time.Second)
	returned := make(chan *StatusStoreHealth, 1)
	go func() {
		returned <- s.cachedStoreHealth(later)
	}()

	select {
	case got := <-returned:
		if got.SizeBytes != 1 {
			t.Fatalf("expired cachedStoreHealth SizeBytes = %d, want stale 1", got.SizeBytes)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expired cachedStoreHealth blocked on refresh")
	}

	select {
	case <-refreshStarted:
	case <-time.After(time.Second):
		t.Fatal("background refresh did not start")
	}
	close(allowRefresh)

	deadline := time.After(time.Second)
	for {
		s.storeHealthMu.Lock()
		refreshed := s.storeHealthEntry != nil && s.storeHealthEntry.SizeBytes == 2 && !s.storeHealthRefreshing
		s.storeHealthMu.Unlock()
		if refreshed {
			return
		}
		select {
		case <-deadline:
			t.Fatal("background refresh did not update cached entry")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestCachedStoreHealthDoesNotHoldMutexDuringRefreshCompute(t *testing.T) {
	s := &Server{}
	canLockDuringCompute := make(chan bool, 1)
	s.storeHealthComputer = func() *StatusStoreHealth {
		locked := make(chan struct{})
		go func() {
			s.storeHealthMu.Lock()
			defer s.storeHealthMu.Unlock()
			close(locked)
		}()
		select {
		case <-locked:
			canLockDuringCompute <- true
		case <-time.After(100 * time.Millisecond):
			canLockDuringCompute <- false
		}
		return &StatusStoreHealth{SizeBytes: 1}
	}

	_ = s.cachedStoreHealth(time.Unix(1_000_000, 0))
	if !<-canLockDuringCompute {
		t.Fatal("cachedStoreHealth held storeHealthMu while running the refresh computer")
	}
}

func TestStatusStoreHealthFromDomainOmitsEmptyLastGC(t *testing.T) {
	h := storehealth.Health{Path: "/c/.beads/dolt"}
	out := statusStoreHealthFromDomain(h)
	if out.LastGCAt != "" || out.LastGCStatus != "" {
		t.Fatalf("LastGC fields = (%q,%q), want empty", out.LastGCAt, out.LastGCStatus)
	}
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(data), "last_gc_at") {
		t.Errorf("JSON contains last_gc_at when zero: %s", data)
	}
}

func TestStatusStoreHealthFromDomainFormatsLastGC(t *testing.T) {
	ts := time.Date(2026, 4, 1, 3, 15, 30, 0, time.UTC)
	h := storehealth.Health{
		Path:         "/c/.beads/dolt",
		LastGCAt:     ts,
		LastGCStatus: "failed",
	}
	out := statusStoreHealthFromDomain(h)
	if out.LastGCAt != "2026-04-01T03:15:30Z" {
		t.Errorf("LastGCAt = %q, want 2026-04-01T03:15:30Z", out.LastGCAt)
	}
	if out.LastGCStatus != "failed" {
		t.Errorf("LastGCStatus = %q, want failed", out.LastGCStatus)
	}
}

func TestComputeStoreHealthServerIntegration(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	for i := 0; i < 5; i++ {
		if _, err := store.Create(beads.Bead{Title: "x"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}
	ep := events.NewFake()
	ts := time.Date(2026, 4, 8, 0, 0, 0, 0, time.UTC)
	payload, _ := json.Marshal(events.StoreMaintenanceDonePayload{DurationSeconds: 1})
	ep.Record(events.Event{Type: events.StoreMaintenanceDone, Ts: ts, Payload: payload})

	state := &fakeState{
		cityPath:      cityPath,
		eventProv:     ep,
		cityBeadStore: store,
	}
	s := &Server{state: state}
	got := s.computeStoreHealth()
	if got == nil {
		t.Fatal("computeStoreHealth returned nil")
	}
	if got.LiveRows != 5 {
		t.Errorf("LiveRows = %d, want 5", got.LiveRows)
	}
	if got.ThresholdMB != 1.0 {
		t.Errorf("ThresholdMB = %v, want 1.0", got.ThresholdMB)
	}
	if got.LastGCAt != "2026-04-08T00:00:00Z" {
		t.Errorf("LastGCAt = %q, want 2026-04-08T00:00:00Z", got.LastGCAt)
	}
}

func TestComputeStoreHealthEmptyCityPath(t *testing.T) {
	state := &fakeState{cityPath: ""}
	s := &Server{state: state}
	if got := s.computeStoreHealth(); got != nil {
		t.Fatalf("computeStoreHealth = %+v, want nil for empty city path", got)
	}
}

func TestCountBeadStoreRowsNil(t *testing.T) {
	if got := countBeadStoreRows(nil); got != 0 {
		t.Fatalf("countBeadStoreRows(nil) = %d, want 0", got)
	}
}

func TestCountBeadStoreRowsIncludesClosedBeads(t *testing.T) {
	store := beads.NewMemStore()
	open, err := store.Create(beads.Bead{Title: "open"})
	if err != nil {
		t.Fatalf("Create open: %v", err)
	}
	closed, err := store.Create(beads.Bead{Title: "closed"})
	if err != nil {
		t.Fatalf("Create closed: %v", err)
	}
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := countBeadStoreRows(store); got != 2 {
		t.Fatalf("countBeadStoreRows = %d, want 2 including closed bead %s and open bead %s", got, closed.ID, open.ID)
	}
}

func TestCountBeadStoreRowsUsesIndexedCounter(t *testing.T) {
	store := &testIndexedRowCounterStore{
		MemStore: beads.NewMemStore(),
		count:    9,
	}
	if got := countBeadStoreRows(store); got != 9 {
		t.Fatalf("countBeadStoreRows = %d, want indexed count 9", got)
	}
	if store.calls != 1 {
		t.Fatalf("CountIndexed calls = %d, want 1", store.calls)
	}
	if !store.query.IncludeClosed || !store.query.AllowScan {
		t.Fatalf("CountIndexed query = %+v, want all-status scan count", store.query)
	}
}

func TestCountBeadStoreRowsDoesNotFallbackToHydratedListAfterIndexedError(t *testing.T) {
	store := &testIndexedRowCounterStore{
		MemStore: beads.NewMemStore(),
		err:      errors.New("indexed count unavailable"),
	}
	if _, err := store.Create(beads.Bead{Title: "would be counted by fallback"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := countBeadStoreRows(store); got != 0 {
		t.Fatalf("countBeadStoreRows = %d, want 0 after indexed count error", got)
	}
	if store.calls != 1 {
		t.Fatalf("CountIndexed calls = %d, want 1", store.calls)
	}
}

func TestBuildStatusBodyIncludesStoreHealth(t *testing.T) {
	state := newFakeState(t)
	s := &Server{state: state}

	body := s.buildStatusBody()
	if body.StoreHealth == nil {
		t.Fatal("StoreHealth = nil, want populated")
	}
	if body.StoreHealth.ThresholdMB != 1.0 {
		t.Errorf("ThresholdMB = %v, want 1.0", body.StoreHealth.ThresholdMB)
	}
	if !strings.HasSuffix(body.StoreHealth.Path, "/.beads/dolt") {
		t.Errorf("Path = %q, want .beads/dolt suffix", body.StoreHealth.Path)
	}
}
