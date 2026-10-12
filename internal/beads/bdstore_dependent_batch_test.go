package beads

import (
	"errors"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestBdStoreDependentIDsBatchReadsBothEdgeTablesInOneCall(t *testing.T) {
	var calls [][]string
	s := NewBdStore("/city", func(_, name string, args ...string) ([]byte, error) {
		calls = append(calls, append([]string{name}, args...))
		return []byte(`[{"issue_id":"gc-7"},{"issue_id":"gc-9"},{"issue_id":"gc-7"}]`), nil
	})

	got, err := s.DependentIDsBatch([]string{"gc-1", "gc-2"}, "parent-child")
	if err != nil {
		t.Fatalf("DependentIDsBatch: %v", err)
	}
	if !reflect.DeepEqual(got, []string{"gc-7", "gc-9"}) {
		t.Errorf("dependents = %v, want [gc-7 gc-9] (deduplicated)", got)
	}
	if len(calls) != 1 {
		t.Fatalf("want 1 bd call, got %d: %v", len(calls), calls)
	}
	if !reflect.DeepEqual(calls[0][:3], []string{"bd", "sql", "--json"}) {
		t.Fatalf("call = %v, want bd sql --json <query>", calls[0])
	}
	query := calls[0][3]
	for _, want := range []string{
		"SELECT issue_id FROM dependencies WHERE type = 'parent-child'",
		"UNION SELECT issue_id FROM wisp_dependencies WHERE type = 'parent-child'",
		"COALESCE(depends_on_issue_id, depends_on_wisp_id, depends_on_external) IN ('gc-1','gc-2')",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q missing %q", query, want)
		}
	}
}

func TestBdStoreDependentIDsBatchChunksLargeSets(t *testing.T) {
	var calls int
	s := NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
		calls++
		return []byte(`[]`), nil
	})
	ids := make([]string, bdDependentBatchChunk+5)
	for i := range ids {
		ids[i] = "gc-" + strconv.Itoa(i)
	}
	got, err := s.DependentIDsBatch(ids, "parent-child")
	if err != nil {
		t.Fatalf("DependentIDsBatch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("dependents = %v, want none", got)
	}
	if calls != 2 {
		t.Errorf("bd calls = %d for %d ids, want 2", calls, len(ids))
	}
}

func TestBdStoreDependentIDsBatchQuotesIDs(t *testing.T) {
	var query string
	s := NewBdStore("/city", func(_, _ string, args ...string) ([]byte, error) {
		query = args[len(args)-1]
		return []byte(`[]`), nil
	})
	if _, err := s.DependentIDsBatch([]string{`gc-'1`, "gc-2"}, "parent-child"); err != nil {
		t.Fatalf("DependentIDsBatch: %v", err)
	}
	if !strings.Contains(query, `IN ('gc-''1','gc-2')`) {
		t.Errorf("query %q does not quote the ids as SQL literals", query)
	}
}

func TestBdStoreDependentIDsBatchReportsWhenBdCannotAnswer(t *testing.T) {
	cases := map[string]bool{
		"Error: 'bd sql' is not yet supported in embedded mode":                   true,
		`Error: table "dependencies" does not have column "depends_on_issue_id"`:  true,
		"Error: table not found: wisp_dependencies":                               true,
		`Error: operation "IssueRelations" not supported by the postgres backend`: true,
		"Error: syntax error at position 12":                                      false,
	}
	for stderr, unsupported := range cases {
		s := NewBdStore("/city", func(_, _ string, _ ...string) ([]byte, error) {
			return nil, errors.New("exit status 1: " + stderr)
		})
		_, err := s.DependentIDsBatch([]string{"gc-1", "gc-2"}, "parent-child")
		if err == nil {
			t.Errorf("%q: want an error", stderr)
			continue
		}
		if got := errors.Is(err, ErrDepListBatchUnsupported); got != unsupported {
			t.Errorf("%q: errors.Is(ErrDepListBatchUnsupported) = %v, want %v (err %v)", stderr, got, unsupported, err)
		}
	}
}
