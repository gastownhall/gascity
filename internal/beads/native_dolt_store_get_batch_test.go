package beads

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeDoltStoreGetBatchMatchesPointGetInOneSearch(t *testing.T) {
	createdAt := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	issues := map[string]*beadslib.Issue{
		"gc-a": {
			ID: "gc-a", Title: "alpha", Status: beadslib.StatusOpen,
			IssueType: beadslib.TypeTask, Priority: 1, CreatedAt: createdAt,
			Labels: []string{"one", "two"}, Metadata: json.RawMessage(`{"kind":"alpha"}`),
			Dependencies: []*beadslib.Dependency{{IssueID: "gc-a", DependsOnID: "gc-parent", Type: beadslib.DepParentChild}},
		},
		"gc-b": {
			ID: "gc-b", Title: "bravo", Status: beadslib.StatusClosed,
			IssueType: beadslib.IssueType("message"), Priority: 2, CreatedAt: createdAt.Add(time.Second),
			Ephemeral: true, Metadata: json.RawMessage(`{"kind":"bravo"}`),
		},
	}

	var calls int
	var gotFilter beadslib.IssueFilter
	batchStorage := &nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			calls++
			gotFilter = filter
			rows := make([]*beadslib.Issue, 0, len(filter.IDs))
			for i := len(filter.IDs) - 1; i >= 0; i-- {
				if issue := issues[filter.IDs[i]]; issue != nil {
					rows = append(rows, cloneNativeIssueForTest(issue))
				}
			}
			return rows, nil
		},
	}
	store := newNativeDoltStoreForTest(batchStorage)

	got, err := store.GetBatch([]string{"gc-b", "gc-a", "gc-b"})
	if err != nil {
		t.Fatalf("GetBatch: %v", err)
	}
	if calls != 1 {
		t.Fatalf("SearchIssues calls = %d, want one", calls)
	}
	if !slices.Equal(gotFilter.IDs, []string{"gc-b", "gc-a"}) {
		t.Fatalf("SearchIssues IDs = %v, want stable unique input", gotFilter.IDs)
	}
	if !gotFilter.IncludeDependencies {
		t.Fatal("SearchIssues IncludeDependencies = false, want Get-equivalent hydration")
	}
	if gotIDs := batchRowIDs(got); !slices.Equal(gotIDs, []string{"gc-b", "gc-a"}) {
		t.Fatalf("GetBatch IDs = %v, want [gc-b gc-a]", gotIDs)
	}

	pointStore := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		searchIssues: func(_ context.Context, _ string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			issue := issues[filter.IDs[0]]
			if issue == nil {
				return nil, nil
			}
			return []*beadslib.Issue{cloneNativeIssueForTest(issue)}, nil
		},
	})
	for i, id := range []string{"gc-b", "gc-a"} {
		want, err := pointStore.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if !reflect.DeepEqual(got[i], want) {
			t.Fatalf("GetBatch(%s) = %#v\npoint Get = %#v", id, got[i], want)
		}
	}
}

func TestNativeDoltStoreGetBatchRejectsNonAuthoritativeResults(t *testing.T) {
	partialCause := errors.New("dependency hydration stopped")
	tests := []struct {
		name        string
		issues      []*beadslib.Issue
		searchErr   error
		want        string
		wantIs      error
		wantPartial bool
	}{
		{
			name: "missing", issues: []*beadslib.Issue{{ID: "gc-a"}},
			want: "missing", wantIs: ErrNotFound,
		},
		{
			name: "duplicate", issues: []*beadslib.Issue{{ID: "gc-a"}, {ID: "gc-a"}, {ID: "gc-b"}},
			want: "duplicate",
		},
		{
			name: "unexpected", issues: []*beadslib.Issue{{ID: "gc-a"}, {ID: "gc-other"}},
			want: "unexpected", wantIs: ErrIDCollision,
		},
		{
			name: "nil row", issues: []*beadslib.Issue{{ID: "gc-a"}, nil},
			want: "malformed",
		},
		{
			name: "empty id", issues: []*beadslib.Issue{{ID: "gc-a"}, {ID: ""}},
			want: "malformed",
		},
		{
			name: "malformed metadata", issues: []*beadslib.Issue{{ID: "gc-a"}, {ID: "gc-b", Metadata: json.RawMessage(`{`)}},
			want: "parsing metadata",
		},
		{
			name:      "partial error",
			issues:    []*beadslib.Issue{{ID: "gc-a"}},
			searchErr: &PartialResultError{Op: "native batch hydrate", Err: partialCause},
			want:      "native batch hydrate", wantIs: partialCause, wantPartial: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
				searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
					return tc.issues, tc.searchErr
				},
			})
			got, err := store.GetBatch([]string{"gc-a", "gc-b"})
			if err == nil {
				t.Fatal("GetBatch error = nil, want whole-batch failure")
			}
			if got != nil {
				t.Fatalf("GetBatch rows = %#v, want nil", got)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("GetBatch error = %v, want substring %q", err, tc.want)
			}
			if tc.wantIs != nil && !errors.Is(err, tc.wantIs) {
				t.Fatalf("GetBatch error = %v, want errors.Is(%v)", err, tc.wantIs)
			}
			if tc.wantPartial != IsPartialResult(err) {
				t.Fatalf("IsPartialResult(%v) = %v, want %v", err, IsPartialResult(err), tc.wantPartial)
			}
		})
	}
}

func TestNativeDoltStoreGetBatchEmptyDoesNotSearch(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		searchIssues: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
			t.Fatal("empty GetBatch must not search storage")
			return nil, nil
		},
	})
	got, err := store.GetBatch(nil)
	if err != nil || got != nil {
		t.Fatalf("GetBatch(nil) = (%#v, %v), want (nil, nil)", got, err)
	}
}

var _ BatchGetter = (*NativeDoltStore)(nil)
