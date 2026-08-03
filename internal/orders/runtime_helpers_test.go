package orders

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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
			Labels:    []string{"order-run:digest"},
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
			Labels: []string{"order-run:digest", "seq:42"},
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
	if _, err := early.Create(beads.Bead{Title: "order:digest", Status: "closed", Labels: []string{"order-run:digest"}}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(time.Millisecond)
	late := beads.NewMemStore()
	lateRun, err := late.Create(beads.Bead{Title: "order:digest", Status: "closed", Labels: []string{"order-run:digest"}})
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
