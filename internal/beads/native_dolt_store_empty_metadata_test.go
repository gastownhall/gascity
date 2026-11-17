package beads

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// roleKeepsRow answers the reader role's metadata filter the way its SQL does:
// JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.k')) = v is NULL, not a match, for a row
// that lacks k.
func roleKeepsRow(t *testing.T, fields map[string]string, issue *beadslib.Issue) bool {
	t.Helper()
	var stored map[string]string
	if err := json.Unmarshal(issue.Metadata, &stored); err != nil {
		t.Fatalf("decoding test metadata: %v", err)
	}
	for k, v := range fields {
		if got, ok := stored[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// ListQuery.Matches reads an absent metadata key as "", so {k: ""} keeps rows
// that lack k. The reader role's metadata filter cannot match an absent key, so
// an empty-valued pair is never sent: the role returns a superset and
// ApplyListQuery cuts it exactly.
func TestNativeDoltStoreListDoesNotPushEmptyMetadataValue(t *testing.T) {
	rows := []*beadslib.Issue{
		nativeIssueWithMetadata(t, "gc-absent", map[string]string{"other": "x"}),
		nativeIssueWithMetadata(t, "gc-empty", map[string]string{"other": "x", "k": ""}),
		nativeIssueWithMetadata(t, "gc-set", map[string]string{"other": "x", "k": "v"}),
	}
	for _, tc := range []struct {
		name       string
		metadata   map[string]string
		wantFields map[string]string
		wantRows   []string
	}{
		{"empty only", map[string]string{"k": ""}, nil, []string{"gc-absent", "gc-empty"}},
		{"empty beside pushable", map[string]string{"k": "", "other": "x"}, map[string]string{"other": "x"}, []string{"gc-absent", "gc-empty"}},
		{"non-empty only", map[string]string{"k": "v"}, map[string]string{"k": "v"}, []string{"gc-set"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured issueops.ListRequest
			storage := &nativeDoltReaderSpy{
				list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
					captured = req
					var page issueops.IssuePage
					for _, issue := range rows {
						if roleKeepsRow(t, req.MetadataFields, issue) {
							page.Items = append(page.Items, &issueops.IssueWithCounts{Issue: cloneNativeIssueForTest(issue)})
						}
					}
					return page, nil
				},
			}
			store := newNativeDoltStoreForTest(storage)
			items, err := store.List(ListQuery{Metadata: tc.metadata, AllowScan: true})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if !reflect.DeepEqual(captured.MetadataFields, tc.wantFields) {
				t.Fatalf("MetadataFields = %v, want %v", captured.MetadataFields, tc.wantFields)
			}
			var ids []string
			for _, b := range items {
				ids = append(ids, b.ID)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, tc.wantRows) {
				t.Fatalf("List rows = %v, want %v", ids, tc.wantRows)
			}
		})
	}
}

// Ready's MetadataEquals is the same advisory pushdown: an empty-valued pair is
// not sent, so the role cannot drop a ready bead that merely lacks the key.
func TestNativeDoltStoreReadyDoesNotPushEmptyMetadataValue(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{"empty only", map[string]string{"k": ""}, nil},
		{"empty beside pushable", map[string]string{"k": "", "gc.routed_to": "pool/a"}, map[string]string{"gc.routed_to": "pool/a"}},
		{"non-empty only", map[string]string{"gc.routed_to": "pool/a"}, map[string]string{"gc.routed_to": "pool/a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var captured issueops.ReadyRequest
			storage := &nativeDoltReaderSpy{
				ready: func(_ context.Context, req issueops.ReadyRequest) (issueops.IssuePage, error) {
					captured = req
					return issueops.IssuePage{}, nil
				},
			}
			store := newNativeDoltStoreForTest(storage)
			if _, err := store.Ready(ReadyQuery{MetadataEquals: tc.in}); err != nil {
				t.Fatalf("Ready: %v", err)
			}
			if !reflect.DeepEqual(captured.MetadataFields, tc.want) {
				t.Fatalf("MetadataFields = %v, want %v", captured.MetadataFields, tc.want)
			}
		})
	}
}

// Count cannot narrow Go-side, and the role's count would drop rows lacking the
// key that ListQuery.Matches accepts for {k: ""}: it refuses instead of
// returning a subset count, so callers fall back to the hydrating path.
func TestNativeDoltStoreCountRefusesEmptyMetadataValue(t *testing.T) {
	store := newNativeDoltStoreForTest(&nativeDoltStorageSpy{
		countIssues: func(context.Context, string, beadslib.IssueFilter) (int64, error) {
			t.Error("CountIssues called for an empty-valued metadata query")
			return 0, nil
		},
	})
	_, err := store.Count(context.Background(), ListQuery{AllowScan: true, Metadata: map[string]string{"k": ""}})
	if !errors.Is(err, ErrCountUnsupported) {
		t.Fatalf("Count err = %v, want ErrCountUnsupported", err)
	}
}
