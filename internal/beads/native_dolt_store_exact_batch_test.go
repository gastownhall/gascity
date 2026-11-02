package beads

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// What the native exact batch read must ASK the reader role for: a listing
// keyed on the ids alone, with every default suppression lifted and an explicit
// unlimited page, then the edges of the rows that listing found, both chunked
// at the served backend's anchor cap. The answer-equals-Get proof runs on a
// real workspace (native_dolt_store_exact_batch_embedded_test.go).

func TestNativeDoltStoreGetExactBatchAsksForTheIDsAlone(t *testing.T) {
	const n = nativeServerEdgeAnchorCap + 30
	rows := make(map[string]*beadslib.Issue, n)
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("gc-%d", i)
		ids = append(ids, id)
		rows[id] = &beadslib.Issue{ID: id, Title: id, Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2}
	}
	var lists []issueops.ListRequest
	var edgeReads [][]string
	storage := &nativeDoltReaderSpy{
		list: func(_ context.Context, req issueops.ListRequest) (issueops.IssuePage, error) {
			lists = append(lists, req)
			var page issueops.IssuePage
			for _, id := range strings.Split(req.IDFilter, ",") {
				if row, ok := rows[id]; ok {
					page.Items = append(page.Items, &issueops.IssueWithCounts{Issue: cloneNativeIssueForTest(row)})
				}
			}
			return page, nil
		},
		edges: func(ctx context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
			edgeReads = append(edgeReads, slices.Clone(req.IDs))
			return edgelessReader{}.ReadEdges(ctx, req)
		},
	}
	store := newNativeDoltStoreForTest(storage)

	found, unresolved, err := store.GetExactBatch(append(slices.Clone(ids), "gc-nope"))
	if err != nil {
		t.Fatalf("GetExactBatch: %v", err)
	}
	if len(found) != n || !slices.Equal(unresolved, []string{"gc-nope"}) {
		t.Fatalf("GetExactBatch found %d (want %d), unresolved %v (want [gc-nope])", len(found), n, unresolved)
	}
	if len(lists) != 2 || len(edgeReads) != 2 {
		t.Fatalf("GetExactBatch sent %d listing(s) and %d edge read(s), want 2 of each: one per %d-id chunk", len(lists), len(edgeReads), nativeServerEdgeAnchorCap)
	}
	all := append(slices.Clone(ids), "gc-nope")
	chunks := [][]string{all[:nativeServerEdgeAnchorCap], all[nativeServerEdgeAnchorCap:]}
	held := [][]string{ids[:nativeServerEdgeAnchorCap], ids[nativeServerEdgeAnchorCap:]}
	base := nativeListReadRequest()
	for i, req := range lists {
		if req.IDFilter != strings.Join(chunks[i], ",") {
			t.Errorf("listing %d IDFilter = %q, want chunk %d's ids", i, req.IDFilter, i)
		}
		if req.Limit == nil || *req.Limit != 0 {
			t.Errorf("listing %d Limit = %v, want an explicit 0 (a nil limit is a 50-row page)", i, req.Limit)
		}
		if req.AllFlag != base.AllFlag || req.IncludeTemplates != base.IncludeTemplates || req.IncludeGates != base.IncludeGates ||
			req.IncludeInfra != base.IncludeInfra || req.IncludeEphemeral != base.IncludeEphemeral {
			t.Errorf("listing %d = %+v keeps a default suppression nativeListReadRequest lifts", i, req)
		}
		if !NativeListRequestKeyed(req) {
			t.Errorf("listing %d reads as unkeyed; an IDFilter narrows the backend's set to the ids", i)
		}
		if !slices.Equal(edgeReads[i], held[i]) {
			t.Errorf("edge read %d = %v, want the ids chunk %d's listing found, in input order", i, edgeReads[i], i)
		}
	}
	stats := store.listRequestStats()
	if stats.Requests != 2 || stats.Unkeyed != 0 {
		t.Errorf("listing stats = %+v, want 2 keyed requests", stats)
	}
}

// An anchor the edge read reports missing (deleted between the two reads), a
// row whose metadata does not parse and a row answering an id nobody asked for
// are left for Get: unresolved, never answered.
func TestNativeDoltStoreGetExactBatchLeavesWhatGetMustSettleUnresolved(t *testing.T) {
	storage := &nativeDoltReaderSpy{
		list: func(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
			return issueops.IssuePage{Items: []*issueops.IssueWithCounts{
				{Issue: &beadslib.Issue{ID: "gc-ok", Title: "ok", Status: beadslib.StatusClosed, IssueType: beadslib.TypeTask, Priority: 2}},
				{Issue: &beadslib.Issue{ID: "gc-gone", Title: "gone", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2}},
				{Issue: &beadslib.Issue{ID: "gc-bad", Title: "bad", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2, Metadata: []byte("{not json")}},
				{Issue: &beadslib.Issue{ID: "gc-other", Title: "other", Status: beadslib.StatusOpen, IssueType: beadslib.TypeTask, Priority: 2}},
			}}, nil
		},
		edges: func(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
			var out issueops.EdgeReadResult
			for _, id := range req.IDs {
				out.Anchors = append(out.Anchors, issueops.AnchorEdges{ID: id, Missing: id == "gc-gone", Edges: []*beadslib.Dependency{}})
			}
			return out, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	found, unresolved, err := store.GetExactBatch([]string{"gc-ok", "gc-gone", "gc-bad", "gc-absent"})
	if err != nil {
		t.Fatalf("GetExactBatch: %v", err)
	}
	if len(found) != 1 || found["gc-ok"].Status != "closed" {
		t.Errorf("found = %+v, want gc-ok alone", found)
	}
	if want := []string{"gc-gone", "gc-bad", "gc-absent"}; !slices.Equal(unresolved, want) {
		t.Errorf("unresolved = %v, want %v in input order", unresolved, want)
	}
}

// An http-native reader refuses an IDFilter on a listing client-side, before
// any round trip (filterReadyByWorkOutcome): the batch reports itself
// unsupported, so a caller reads those ids with Get. A real failure is an
// error.
func TestNativeDoltStoreGetExactBatchReportsAnHTTPRefusalAsUnsupported(t *testing.T) {
	refused := &nativeDoltReaderSpy{
		list: func(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
			return issueops.IssuePage{}, fmt.Errorf("http client: %w", &beadslib.ErrUnsupported{Op: "List.IDFilter", Backend: "http"})
		},
	}
	if _, _, err := newNativeDoltStoreForTest(refused).GetExactBatch([]string{"gc-1", "gc-2"}); !errors.Is(err, ErrExactBatchGetUnsupported) {
		t.Fatalf("GetExactBatch over a refusing reader = %v, want ErrExactBatchGetUnsupported", err)
	}
	broken := errors.New("scan: driver: bad connection")
	failing := &nativeDoltReaderSpy{
		list: func(context.Context, issueops.ListRequest) (issueops.IssuePage, error) {
			return issueops.IssuePage{}, broken
		},
	}
	if _, _, err := newNativeDoltStoreForTest(failing).GetExactBatch([]string{"gc-1"}); !errors.Is(err, broken) || errors.Is(err, ErrExactBatchGetUnsupported) {
		t.Fatalf("GetExactBatch over a failing reader = %v, want the read's own error", err)
	}
}
