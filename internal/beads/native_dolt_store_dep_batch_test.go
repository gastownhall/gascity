package beads

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The batched DOWN-edge read (ga-50tsx).
//
// The per-anchor loop these tests replace costs one read transaction per bead —
// and against a remote server whose pool has gone cold, or whose link drops
// handshakes, one CONNECT per bead. `gc storage recover-stranded` walks a few
// hundred beads three times, which is why it could not finish over the Tailscale
// work-store path. The fix is not "retry harder": it is to ask the question the
// storage seam already answers in one round trip.
//
// Cost is asserted as ROUND TRIPS (ReadEdges calls), because that is the thing
// the defect is about and the thing a spy can count without a network.

// edgeFixtureReader answers ReadEdges from a fixture, honoring the request the
// way a real backend does: one anchor per DISTINCT requested id, in first-mention
// order, missing anchors reported per anchor rather than failing the call.
//
// It is a faithful oracle rather than a canned result so DepList and
// DepListBatch can be compared against the SAME facts — a spy that replayed one
// stored answer would make the comparison agree with itself.
type edgeFixtureReader struct {
	edges    map[string][]*issueops.Dependency
	requests []issueops.EdgeReadRequest
	err      error

	// maxIDs, when nonzero, mirrors the server's own ReadEdges anchor cap
	// (nativeServerEdgeAnchorCap / bd-enterprise's maxDependencyAnchors): a
	// request naming more than this many ids is refused outright, the way a
	// real bd-serve refuses the WHOLE call rather than serving the first
	// maxIDs and dropping the rest. Zero means unbounded, matching every
	// fixture above that predates the server's cap being enforced here.
	maxIDs int
}

func (r *edgeFixtureReader) ReadEdges(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
	r.requests = append(r.requests, req)
	if r.err != nil {
		return issueops.EdgeReadResult{}, r.err
	}
	if r.maxIDs > 0 && len(req.IDs) > r.maxIDs {
		return issueops.EdgeReadResult{}, fmt.Errorf("invalid_argument: at most %d issue_id values per request, got %d", r.maxIDs, len(req.IDs))
	}
	seen := make(map[string]bool, len(req.IDs))
	result := issueops.EdgeReadResult{}
	for _, id := range req.IDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		edges, held := r.edges[id]
		if !held {
			result.Anchors = append(result.Anchors, issueops.AnchorEdges{ID: id, Missing: true})
			continue
		}
		if edges == nil {
			edges = []*issueops.Dependency{}
		}
		result.Anchors = append(result.Anchors, issueops.AnchorEdges{ID: id, Edges: edges})
	}
	return result, nil
}

// newEdgeFixtureStore returns a store whose EdgeReader answers from edges, with
// the reader exposed so a test can count round trips.
func newEdgeFixtureStore(edges map[string][]*issueops.Dependency) (*NativeDoltStore, *edgeFixtureReader) {
	reader := &edgeFixtureReader{edges: edges}
	spy := newRoleStorageSpy()
	spy.edges = nil
	return newNativeDoltStoreForTest(&edgeFixtureStorage{Storage: spy, reader: reader}), reader
}

type edgeFixtureStorage struct {
	beadslib.Storage
	reader *edgeFixtureReader
}

func (s *edgeFixtureStorage) EdgeReader() (issueops.EdgeReader, error) { return s.reader, nil }

// TestDepListBatchReadsEveryAnchorInOneEdgeRead is the defect, stated as cost:
// N anchors must cost ONE round trip, not N.
//
// Red before the batch existed: NativeDoltStore had no DepListBatch at all, so
// every caller — internal/dispatch's scope-skip walk and the class-store
// recovery — fell back to a per-id DepList loop and paid N.
func TestDepListBatchReadsEveryAnchorInOneEdgeRead(t *testing.T) {
	store, reader := newEdgeFixtureStore(map[string][]*issueops.Dependency{
		"gc-1": {{IssueID: "gc-1", DependsOnID: "gc-2", Type: beadslib.DepBlocks}},
		"gc-2": nil,
		"gc-3": {{IssueID: "gc-3", DependsOnID: "external:jira-9", Type: beadslib.DepRelated}},
	})

	got, err := store.DepListBatch([]string{"gc-1", "gc-2", "gc-3"})
	if err != nil {
		t.Fatalf("DepListBatch: %v", err)
	}
	if len(reader.requests) != 1 {
		t.Fatalf("ReadEdges called %d times for 3 anchors, want 1: the read is still per-anchor", len(reader.requests))
	}
	if want := []string{"gc-1", "gc-2", "gc-3"}; !reflect.DeepEqual(reader.requests[0].IDs, want) {
		t.Errorf("EdgeReadRequest.IDs = %v, want every anchor in one request %v", reader.requests[0].IDs, want)
	}
	want := map[string][]Dep{
		"gc-1": {{IssueID: "gc-1", DependsOnID: "gc-2", Type: "blocks"}},
		"gc-2": {},
		"gc-3": {{IssueID: "gc-3", DependsOnID: "external:jira-9", Type: "related"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DepListBatch = %+v, want %+v", got, want)
	}
}

// TestDepListBatchAgreesWithDepListAnchorForAnchor is the correctness control
// for the cost fix: a cheaper read that answers differently is worse than the
// slow one it replaces.
//
// The comparison is run against the SAME fixture and is then deliberately
// falsified — one edge is added between the two reads and the same comparison
// must go red. Without that half the assertion could pass over two empty
// answers and prove nothing.
func TestDepListBatchAgreesWithDepListAnchorForAnchor(t *testing.T) {
	fixture := map[string][]*issueops.Dependency{
		"gc-1": {
			{IssueID: "gc-1", DependsOnID: "gc-2", Type: beadslib.DepBlocks},
			{IssueID: "gc-1", DependsOnID: "external:jira-9", Type: beadslib.DepRelated},
		},
		"gc-2": nil,
		"gc-3": {{IssueID: "gc-3", DependsOnID: "gc-1", Type: beadslib.DepParentChild}},
	}
	anchors := []string{"gc-1", "gc-2", "gc-3"}
	store, _ := newEdgeFixtureStore(fixture)

	batched, err := store.DepListBatch(anchors)
	if err != nil {
		t.Fatalf("DepListBatch: %v", err)
	}
	differs := func() string {
		for _, id := range anchors {
			single, err := store.DepList(id, "down")
			if err != nil {
				return fmt.Sprintf("DepList(%s): %v", id, err)
			}
			if len(single) == 0 && len(batched[id]) == 0 {
				continue
			}
			if !reflect.DeepEqual(single, batched[id]) {
				return fmt.Sprintf("anchor %s: DepList = %+v, DepListBatch = %+v", id, single, batched[id])
			}
		}
		return ""
	}
	if diff := differs(); diff != "" {
		t.Fatalf("the batch does not answer what the per-anchor read answers: %s", diff)
	}

	// The control. Mutate one edge and require the same comparison to fail: a
	// comparator that cannot see this difference could not have seen a real one.
	fixture["gc-2"] = []*issueops.Dependency{{IssueID: "gc-2", DependsOnID: "gc-7", Type: beadslib.DepBlocks}}
	if diff := differs(); diff == "" {
		t.Fatal("the comparison passed after an edge was added between the two reads, so it cannot fail and proves nothing")
	}
}

// TestDepListBatchDistinguishesAnEdgelessAnchorFromAMissingOne pins the miss
// semantics, which are the one place this role cannot copy DepList.
//
// DepList answers ErrNotFound for an anchor that is not there. A batch cannot:
// failing the call for one absent id would throw away the answers for every id
// that was found (issueops.EdgeReader says so). So a missing anchor gets no
// entry — matching MemStore, FileStore, BdStore and DoltliteReadStore — and an
// anchor that IS held with no edges gets an entry holding an empty slice.
func TestDepListBatchDistinguishesAnEdgelessAnchorFromAMissingOne(t *testing.T) {
	store, _ := newEdgeFixtureStore(map[string][]*issueops.Dependency{"gc-2": nil})

	got, err := store.DepListBatch([]string{"gc-1", "gc-2"})
	if err != nil {
		t.Fatalf("DepListBatch: %v", err)
	}
	if _, held := got["gc-1"]; held {
		t.Errorf("the missing anchor gc-1 got an entry (%+v); an absent anchor must not read as an edge-free one", got["gc-1"])
	}
	deps, held := got["gc-2"]
	if !held {
		t.Fatal("the held, edge-free anchor gc-2 got no entry, so it is indistinguishable from an anchor this store does not hold")
	}
	if len(deps) != 0 {
		t.Errorf("gc-2 = %+v, want no edges", deps)
	}
	// And DepList's own miss policy is untouched by the new method.
	if _, err := store.DepList("gc-1", "down"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DepList over a missing anchor = %v, want ErrNotFound", err)
	}
}

// TestDepListBatchChunksALargeAnchorListWithoutLosingAnchors keeps the fix from
// trading one unbounded cost for another: a caller with thousands of anchors
// must not build one statement the server refuses.
func TestDepListBatchChunksALargeAnchorListWithoutLosingAnchors(t *testing.T) {
	const anchors = nativeDepListBatchChunk + 100
	fixture := make(map[string][]*issueops.Dependency, anchors)
	ids := make([]string, 0, anchors)
	for i := range anchors {
		id := fmt.Sprintf("gc-%d", i)
		ids = append(ids, id)
		fixture[id] = []*issueops.Dependency{{IssueID: id, DependsOnID: "gc-root", Type: beadslib.DepBlocks}}
	}
	store, reader := newEdgeFixtureStore(fixture)

	got, err := store.DepListBatch(ids)
	if err != nil {
		t.Fatalf("DepListBatch: %v", err)
	}
	if len(reader.requests) != 2 {
		t.Fatalf("ReadEdges called %d times for %d anchors, want 2 chunks of at most %d", len(reader.requests), anchors, nativeDepListBatchChunk)
	}
	if len(got) != anchors {
		t.Fatalf("DepListBatch answered %d anchors, want %d: chunking dropped some", len(got), anchors)
	}
	for _, id := range ids {
		if len(got[id]) != 1 || got[id][0].DependsOnID != "gc-root" {
			t.Fatalf("anchor %s = %+v, want its one edge", id, got[id])
		}
	}
}

// realBdServeReadEdgesAnchorCap is the ReadEdges anchor cap a real bd-serve
// enforces at the pinned enterprise revision (bd-enterprise
// internal/httpapi/edges.go: maxDependencyAnchors = 100), confirmed live
// against a real server with a 130-id request. It is written as a LITERAL
// here, deliberately independent of nativeServerEdgeAnchorCap /
// nativeDepListBatchChunk, so this test exercises the real, fixed wire limit
// rather than a mutation-proof-defeating tautology against whatever value the
// production constant happens to hold.
const realBdServeReadEdgesAnchorCap = 100

// TestDepListBatchNeverExceedsTheServerAnchorCap pins nativeDepListBatchChunk
// to the server's own ReadEdges anchor cap rather than an arbitrary, larger
// client-side batch size.
//
// Red before S5b-portrev item 2: nativeDepListBatchChunk was 500, which rode
// fine against the embedded DoltliteReadStore's own (unrelated) cap of the
// same name, but exceeds the 100-issue_id-per-request cap a real bd-serve
// enforces over http — confirmed live, where a batch over the cap is refused
// outright ("400 invalid_argument: at most 100 issue_id values per request").
// A fake EdgeReader pinned to the real server's cap (not to whatever
// production constant this test is meant to be checking) catches any future
// drift back above it, the way nothing in this package could before this test
// existed.
func TestDepListBatchNeverExceedsTheServerAnchorCap(t *testing.T) {
	const anchors = realBdServeReadEdgesAnchorCap*2 + 30
	fixture := make(map[string][]*issueops.Dependency, anchors)
	ids := make([]string, 0, anchors)
	for i := range anchors {
		id := fmt.Sprintf("gc-%d", i)
		ids = append(ids, id)
		fixture[id] = []*issueops.Dependency{{IssueID: id, DependsOnID: "gc-root", Type: beadslib.DepBlocks}}
	}
	store, reader := newEdgeFixtureStore(fixture)
	reader.maxIDs = realBdServeReadEdgesAnchorCap

	got, err := store.DepListBatch(ids)
	if err != nil {
		t.Fatalf("DepListBatch: %v, want chunking to keep every ReadEdges call at or under the server's %d-anchor cap", err, realBdServeReadEdgesAnchorCap)
	}
	if len(got) != anchors {
		t.Fatalf("DepListBatch answered %d anchors, want %d", len(got), anchors)
	}
	for _, req := range reader.requests {
		if len(req.IDs) > realBdServeReadEdgesAnchorCap {
			t.Fatalf("one ReadEdges call carried %d anchors, want <= %d (the server's own cap)", len(req.IDs), realBdServeReadEdgesAnchorCap)
		}
	}
}

// TestDepListBatchReportsAFailedChunkRatherThanAPartialAnswer keeps a dropped
// link from reading as a clean graph. A partial map with a nil error is how a
// recovery walk decides a bead has no edges and copies it edge-free.
func TestDepListBatchReportsAFailedChunkRatherThanAPartialAnswer(t *testing.T) {
	store, reader := newEdgeFixtureStore(map[string][]*issueops.Dependency{"gc-1": nil})
	reader.err = errors.New("dial tcp 100.109.51.65:3306: i/o timeout")

	got, err := store.DepListBatch([]string{"gc-1"})
	if err == nil {
		t.Fatalf("DepListBatch over a dropped link returned %+v and no error", got)
	}
	if got != nil {
		t.Errorf("DepListBatch returned a %d-entry map alongside its error; a caller reading it sees a clean graph", len(got))
	}
}

// TestDepListBatchOverNoAnchorsAsksNothing keeps a caller that filtered its list
// to nothing from paying a round trip to be told so.
func TestDepListBatchOverNoAnchorsAsksNothing(t *testing.T) {
	store, reader := newEdgeFixtureStore(nil)

	got, err := store.DepListBatch(nil)
	if err != nil {
		t.Fatalf("DepListBatch: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("DepListBatch(nil) = %+v, want an empty answer", got)
	}
	if len(reader.requests) != 0 {
		t.Errorf("ReadEdges called %d times for no anchors, want 0", len(reader.requests))
	}
}
