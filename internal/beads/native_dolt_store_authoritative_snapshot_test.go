package beads

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeDoltStoreAuthoritativeSnapshotIsStrictAndMatchesSearchAuthority(t *testing.T) {
	var calls int
	var gotFilter beadslib.IssueFilter
	storage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			calls++
			gotFilter = filter
			return []*beadslib.Issue{
				{ID: "gc-z", Status: beadslib.StatusClosed, Metadata: json.RawMessage(`{"kind":"z"}`)},
				{ID: "gc-a", Status: beadslib.StatusOpen, Metadata: json.RawMessage(`{"kind":"a"}`)},
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	rows, err := store.AuthoritativeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !gotFilter.IncludeDependencies || len(gotFilter.IDs) != 0 || gotFilter.Status != nil || len(gotFilter.ExcludeStatus) != 0 {
		t.Fatalf("calls/filter = %d/%#v, want one unbounded all-status hydrated search", calls, gotFilter)
	}
	if got := []string{rows[0].ID, rows[1].ID}; !reflect.DeepEqual(got, []string{"gc-a", "gc-z"}) {
		t.Fatalf("snapshot IDs = %v", got)
	}

	storage.searchIssues = func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
		return []*beadslib.Issue{{ID: "gc-bad", Metadata: json.RawMessage(`{`)}}, nil
	}
	if _, err := store.AuthoritativeSnapshot(); err == nil {
		t.Fatal("malformed native metadata returned nil error")
	}
}
