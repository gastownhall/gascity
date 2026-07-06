package orders

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

type concurrentLastRunStore struct {
	beads.Store

	mu        sync.Mutex
	active    int
	maxActive int
}

func (s *concurrentLastRunStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	s.active++
	if s.active > s.maxActive {
		s.maxActive = s.active
	}
	s.mu.Unlock()

	time.Sleep(10 * time.Millisecond)

	s.mu.Lock()
	s.active--
	s.mu.Unlock()
	return []beads.Bead{{
		ID:        query.Label,
		CreatedAt: time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC),
		Labels:    []string{query.Label},
	}}, nil
}

func TestLoadLastRunsBoundsConcurrentStoreQueries(t *testing.T) {
	store := &concurrentLastRunStore{Store: beads.NewMemStore()}
	requests := make([]LastRunRequest, 16)
	for i := range requests {
		requests[i] = LastRunRequest{
			Name:   fmt.Sprintf("order-%02d", i),
			Stores: []beads.Store{store},
		}
	}

	results := LoadLastRuns(requests, 4)
	if len(results) != len(requests) {
		t.Fatalf("LoadLastRuns returned %d results, want %d", len(results), len(requests))
	}
	for i, result := range results {
		if result.Name != requests[i].Name {
			t.Fatalf("result[%d].Name = %q, want %q", i, result.Name, requests[i].Name)
		}
		if result.Err != nil {
			t.Fatalf("result[%d].Err = %v", i, result.Err)
		}
		if result.LastRun.IsZero() {
			t.Fatalf("result[%d].LastRun is zero", i)
		}
	}

	store.mu.Lock()
	maxActive := store.maxActive
	store.mu.Unlock()
	if maxActive <= 1 {
		t.Fatalf("max concurrent List calls = %d, want > 1", maxActive)
	}
	if maxActive > 4 {
		t.Fatalf("max concurrent List calls = %d, want <= 4", maxActive)
	}
}

type rowsErrorStore struct {
	*beads.MemStore
	rows []beads.Bead
	err  error
}

func (s *rowsErrorStore) List(_ beads.ListQuery) ([]beads.Bead, error) {
	return s.rows, s.err
}

func TestLastRunFuncForStoreReturnsLatestRun(t *testing.T) {
	store := beads.NewMemStore()

	first, err := store.Create(beads.Bead{
		Title:  "order:digest",
		Status: "closed",
		Labels: []string{"order-run:digest"},
	})
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Millisecond)

	second, err := store.Create(beads.Bead{
		Title:  "order:digest",
		Status: "closed",
		Labels: []string{"order-run:digest", "wisp-failed"},
	})
	if err != nil {
		t.Fatal(err)
	}

	got, err := LastRunFuncForStore(store)("digest")
	if err != nil {
		t.Fatalf("LastRunFuncForStore(): %v", err)
	}
	if !got.Equal(second.CreatedAt) {
		t.Fatalf("LastRunFuncForStore() = %s, want %s (latest run should remain authoritative)", got, second.CreatedAt)
	}
	if !second.CreatedAt.After(first.CreatedAt) {
		t.Fatalf("test setup invalid: second.CreatedAt=%s, first.CreatedAt=%s", second.CreatedAt, first.CreatedAt)
	}
}

func TestLastRunFuncForStoreReturnsZeroWhenNoRunsExist(t *testing.T) {
	store := beads.NewMemStore()

	got, err := LastRunFuncForStore(store)("digest")
	if err != nil {
		t.Fatalf("LastRunFuncForStore(): %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("LastRunFuncForStore() = %s, want zero time", got)
	}
}

func TestLastRunFuncForStoreUsesRowsFromPartialTierError(t *testing.T) {
	want := time.Date(2026, 5, 15, 7, 0, 0, 0, time.UTC)
	store := &rowsErrorStore{
		MemStore: beads.NewMemStore(),
		rows: []beads.Bead{{
			ID:        "run-1",
			Title:     "digest",
			CreatedAt: want,
			Labels:    []string{"order-run:digest"},
		}},
		err: errors.New("wisps tier unavailable"),
	}

	got, err := LastRunFuncForStore(store)("digest")
	if err != nil {
		t.Fatalf("LastRunFuncForStore(): %v", err)
	}
	if !got.Equal(want) {
		t.Fatalf("LastRunFuncForStore() = %s, want %s from surviving rows", got, want)
	}
}

func TestCursorFuncForStoreUsesRowsAndLogsPartialTierError(t *testing.T) {
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
			Labels: []string{"order-run:digest", "seq:42"},
		}},
		err: errors.New("wisps tier unavailable"),
	}

	got := CursorFuncForStore(store)("digest")
	if got != 42 {
		t.Fatalf("CursorFuncForStore() = %d, want 42 from surviving rows", got)
	}
	if len(logs) == 0 || !strings.Contains(logs[0], "partially failed") {
		t.Fatalf("logs = %#v, want partial failure log", logs)
	}
}
