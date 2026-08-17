package beads_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBdStoreAuthoritativeSnapshotHydratesOnlyWispPossibleRows(t *testing.T) {
	var calls []string
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		full := name + " " + strings.Join(args, " ")
		calls = append(calls, full)
		switch args[0] {
		case "list":
			return []byte(`[
				{"id":"bd-durable","title":"durable","status":"closed","issue_type":"task","metadata":{"source":"durable"}},
				{"id":"bd-collision","title":"wisp winner","status":"closed","issue_type":"task","ephemeral":true,"metadata":{"source":"wisp"}}
			]`), nil
		case "query":
			return []byte(`[
				{"id":"bd-collision","title":"wisp winner","status":"closed","issue_type":"task","ephemeral":true,"metadata":{"source":"wisp"}}
			]`), nil
		case "show":
			if len(args) != 3 || args[2] != "bd-collision" {
				t.Fatalf("bd show args = %v, want only collision candidate", args)
			}
			return []byte(`[{"id":"bd-collision","title":"durable authority","status":"open","issue_type":"task","metadata":{"source":"durable"}}]`), nil
		default:
			return nil, fmt.Errorf("unexpected command %q", full)
		}
	}
	store := beads.NewBdStore("/city", runner)
	rows, err := store.AuthoritativeSnapshot()
	if err != nil {
		t.Fatalf("AuthoritativeSnapshot: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != "bd-collision" || rows[1].ID != "bd-durable" {
		t.Fatalf("snapshot rows = %#v", rows)
	}
	if rows[0].Status != "open" || rows[0].Metadata["source"] != "durable" || rows[0].Ephemeral {
		t.Fatalf("collision row = %#v, want point-Get durable authority", rows[0])
	}
	showCalls := 0
	for _, call := range calls {
		if strings.HasPrefix(call, "bd show ") {
			showCalls++
			if strings.Contains(call, "bd-durable") {
				t.Fatalf("durable-only row was point hydrated: %q", call)
			}
		}
	}
	if showCalls != 1 {
		t.Fatalf("calls = %#v, want one bounded show", calls)
	}
}

func TestBdStoreAuthoritativeSnapshotUsesNoShowForDurableCorpus(t *testing.T) {
	var calls []string
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		full := name + " " + strings.Join(args, " ")
		calls = append(calls, full)
		switch args[0] {
		case "list":
			return []byte(`[
				{"id":"bd-a","status":"open","issue_type":"task"},
				{"id":"bd-b","status":"closed","issue_type":"task"}
			]`), nil
		case "query":
			return []byte(`[]`), nil
		default:
			return nil, fmt.Errorf("unexpected command %q", full)
		}
	}
	store := beads.NewBdStore("/city", runner, beads.WithBdStoreListSkipLabels(true))
	rows, err := store.AuthoritativeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("snapshot rows = %#v", rows)
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "bd list ") || !strings.HasPrefix(calls[1], "bd query ") {
		t.Fatalf("calls = %#v, want one list + one query + zero show", calls)
	}
	if !strings.Contains(calls[0], "--skip-labels") {
		t.Fatalf("primary snapshot call = %q, want configured --skip-labels", calls[0])
	}
}

func TestBdStoreAuthoritativeSnapshotHydratesNoHistoryCandidate(t *testing.T) {
	var calls []string
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		full := name + " " + strings.Join(args, " ")
		calls = append(calls, full)
		switch args[0] {
		case "list":
			return []byte(`[{"id":"bd-nohistory","status":"closed","issue_type":"task","no_history":true,"metadata":{"source":"list"}}]`), nil
		case "query":
			return []byte(`[]`), nil
		case "show":
			if len(args) != 3 || args[2] != "bd-nohistory" {
				t.Fatalf("bd show args = %v, want no-history candidate", args)
			}
			return []byte(`[{"id":"bd-nohistory","status":"open","issue_type":"task","no_history":true,"metadata":{"source":"point"}}]`), nil
		default:
			return nil, fmt.Errorf("unexpected command %q", full)
		}
	}
	store := beads.NewBdStore("/city", runner)
	rows, err := store.AuthoritativeSnapshot()
	if err != nil {
		t.Fatalf("AuthoritativeSnapshot: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "bd-nohistory" || rows[0].Status != "open" || rows[0].Metadata["source"] != "point" {
		t.Fatalf("snapshot rows = %#v, want point-authoritative no-history row", rows)
	}
	showCalls := 0
	for _, call := range calls {
		if strings.HasPrefix(call, "bd show ") {
			showCalls++
		}
	}
	if showCalls != 1 {
		t.Fatalf("calls = %#v, want one bounded show for no-history candidate", calls)
	}
}

func TestBdStoreAuthoritativeSnapshotRejectsPartialTierRead(t *testing.T) {
	var calls []string
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		full := name + " " + strings.Join(args, " ")
		calls = append(calls, full)
		switch args[0] {
		case "list":
			return []byte(`[
				{"id":"bd-good","status":"closed","issue_type":"task"},
				{"id":"bd-corrupt","status":"closed","issue_type":"task","created_at":"not-a-time"}
			]`), nil
		case "query":
			return []byte(`[]`), nil
		default:
			return nil, errors.New("show must not run after partial census")
		}
	}
	store := beads.NewBdStore("/city", runner)
	if _, err := store.AuthoritativeSnapshot(); err == nil || !beads.IsPartialResult(err) {
		t.Fatalf("AuthoritativeSnapshot error = %v, want partial-result refusal", err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "bd show ") {
			t.Fatalf("show ran after partial census: %q", call)
		}
	}
}

func TestBdStoreAuthoritativeSnapshotRejectsUnsupportedWispQuery(t *testing.T) {
	var calls []string
	runner := func(_ string, name string, args ...string) ([]byte, error) {
		full := name + " " + strings.Join(args, " ")
		calls = append(calls, full)
		switch args[0] {
		case "list":
			return []byte(`[{"id":"bd-durable","status":"closed","issue_type":"task"}]`), nil
		case "query":
			return nil, errors.New(`unknown command "query"`)
		default:
			return nil, fmt.Errorf("unexpected command %q", full)
		}
	}
	store := beads.NewBdStore("/city", runner)
	if _, err := store.AuthoritativeSnapshot(); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("AuthoritativeSnapshot error = %v, want unsupported-query refusal", err)
	}
	for _, call := range calls {
		if strings.HasPrefix(call, "bd show ") {
			t.Fatalf("show ran after unreadable wisp tier: %q", call)
		}
	}
}
