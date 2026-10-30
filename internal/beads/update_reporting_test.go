package beads_test

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// updateReportingRunner answers `bd update` with updateOut and `bd show` with
// showOut, recording every call.
func updateReportingRunner(updateOut, showOut string, calls *[][]string) beads.CommandRunner {
	return func(_, _ string, args ...string) ([]byte, error) {
		*calls = append(*calls, slices.Clone(args))
		switch args[0] {
		case "update":
			return []byte(updateOut), nil
		case "show":
			return []byte(showOut), nil
		default:
			return nil, fmt.Errorf("unexpected bd %v", args)
		}
	}
}

func TestBdStoreUpdateReportingReadsTheWrittenRowFromUpdateOutput(t *testing.T) {
	var calls [][]string
	s := beads.NewBdStore("/city", updateReportingRunner(
		`[{"id":"bd-1","title":"x","status":"closed","issue_type":"task","metadata":{"gc.outcome":"pass","keep":"v"}}]`,
		"", &calls))
	status := "closed"
	row, err := s.UpdateReporting("bd-1", beads.UpdateOpts{Status: &status, Metadata: map[string]string{"gc.outcome": "pass"}})
	if err != nil {
		t.Fatalf("UpdateReporting: %v", err)
	}
	if row.Status != "closed" || row.Metadata["gc.outcome"] != "pass" || row.Metadata["keep"] != "v" {
		t.Fatalf("row = %+v, want the status and full metadata bd printed", row)
	}
	if len(calls) != 1 || calls[0][0] != "update" {
		t.Fatalf("bd calls = %v, want exactly the one update (no read-back show)", calls)
	}
	if want := []string{"update", "--json", "bd-1", "--status", "closed", "--set-metadata", "gc.outcome=pass"}; !slices.Equal(calls[0], want) {
		t.Fatalf("update args = %v, want %v (the same argv Update runs)", calls[0], want)
	}
}

func TestBdStoreUpdateReportingFallsBackToShowWhenOutputHasNoRow(t *testing.T) {
	for name, out := range map[string]string{
		"empty output":       "",
		"other bead only":    `[{"id":"bd-2","status":"closed"}]`,
		"row without status": `[{"id":"bd-1","title":"x"}]`,
		"unparseable output": `Updated bd-1`,
	} {
		t.Run(name, func(t *testing.T) {
			var calls [][]string
			s := beads.NewBdStore("/city", updateReportingRunner(out,
				`[{"id":"bd-1","title":"x","status":"open","issue_type":"task","metadata":{"a":"b"}}]`, &calls))
			status := "closed"
			row, err := s.UpdateReporting("bd-1", beads.UpdateOpts{Status: &status})
			if err != nil {
				t.Fatalf("UpdateReporting: %v", err)
			}
			if row.Status != "open" || row.Metadata["a"] != "b" {
				t.Fatalf("row = %+v, want the row bd show reports", row)
			}
			if len(calls) != 2 || calls[1][0] != "show" {
				t.Fatalf("bd calls = %v, want update then a read-back show", calls)
			}
		})
	}
}

func TestBdStoreUpdateReportingMapsNotFound(t *testing.T) {
	runner := func(_, _ string, _ ...string) ([]byte, error) {
		return nil, fmt.Errorf("exit status 1: Error resolving x: no issue found matching \"x\"")
	}
	s := beads.NewBdStore("/city", runner)
	status := "closed"
	if _, err := s.UpdateReporting("x", beads.UpdateOpts{Status: &status}); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestBdStoreUpdateReportingWithNoFieldsOnlyReadsTheRow(t *testing.T) {
	var calls [][]string
	s := beads.NewBdStore("/city", updateReportingRunner("",
		`[{"id":"bd-1","title":"x","status":"open","issue_type":"task"}]`, &calls))
	row, err := s.UpdateReporting("bd-1", beads.UpdateOpts{})
	if err != nil {
		t.Fatalf("UpdateReporting: %v", err)
	}
	if row.Status != "open" || len(calls) != 1 || calls[0][0] != "show" {
		t.Fatalf("row = %+v calls = %v, want only a show for an empty update", row, calls)
	}
}

// updateCountingStore counts Update and Get on a store with no UpdateReporting.
type updateCountingStore struct {
	beads.Store
	updates, gets int
}

func (s *updateCountingStore) Update(id string, opts beads.UpdateOpts) error {
	s.updates++
	return s.Store.Update(id, opts)
}

func (s *updateCountingStore) Get(id string) (beads.Bead, error) {
	s.gets++
	return s.Store.Get(id)
}

func TestUpdateAndReadBackWithoutTheCapabilityUpdatesThenReads(t *testing.T) {
	mem := beads.NewMemStore()
	b, err := mem.Create(beads.Bead{Title: "x", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	store := &updateCountingStore{Store: mem}
	status := "closed"
	row, err := beads.UpdateAndReadBack(store, b.ID, beads.UpdateOpts{Status: &status, Metadata: map[string]string{"k": "v"}})
	if err != nil {
		t.Fatalf("UpdateAndReadBack: %v", err)
	}
	if row.Status != "closed" || row.Metadata["k"] != "v" {
		t.Fatalf("row = %+v, want closed with k=v", row)
	}
	if store.updates != 1 || store.gets != 1 {
		t.Fatalf("updates=%d gets=%d, want 1 and 1", store.updates, store.gets)
	}
}

func TestUpdateAndReadBackUsesTheCapability(t *testing.T) {
	var calls [][]string
	s := beads.NewBdStore("/city", updateReportingRunner(`[{"id":"bd-1","status":"closed"}]`, "", &calls))
	status := "closed"
	row, err := beads.UpdateAndReadBack(s, "bd-1", beads.UpdateOpts{Status: &status})
	if err != nil {
		t.Fatalf("UpdateAndReadBack: %v", err)
	}
	if row.Status != "closed" || len(calls) != 1 {
		t.Fatalf("row = %+v calls = %v, want one update call", row, calls)
	}
}
