package beads

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	beadslib "github.com/steveyegge/beads"
	"github.com/steveyegge/beads/issueops"
)

// The role-accessor re-point tests. Each raw storage method these replace is
// OFF the v0 served surface — AddDependency, RemoveDependency,
// GetDependenciesWithMetadata, GetDependentsWithMetadata and DeleteIssue all
// refuse on the http backend — so a store that still calls them cannot serve a
// city over the wire. GetStatistics is served, but only as a degraded off-role
// bridge whose own doc says it must never hard-fail, which is the one thing a
// Ping needs it to do.
//
// The doubles below refuse every raw method loudly, so a re-point that is only
// half done fails here rather than silently keeping the raw call.

type roleSpyEdges struct {
	requests []issueops.EdgeReadRequest
	result   issueops.EdgeReadResult
	err      error
}

func (r *roleSpyEdges) ReadEdges(_ context.Context, req issueops.EdgeReadRequest) (issueops.EdgeReadResult, error) {
	r.requests = append(r.requests, req)
	return r.result, r.err
}

type roleSpyRelations struct {
	requests []issueops.RelatedRequest
	result   []*issueops.RelatedIssue
	err      error
}

func (r *roleSpyRelations) Related(_ context.Context, req issueops.RelatedRequest) ([]*issueops.RelatedIssue, error) {
	r.requests = append(r.requests, req)
	return r.result, r.err
}

type roleSpyDependencyEditor struct {
	added   []issueops.AddDependenciesRequest
	removed []issueops.RemoveDependencyRequest
	err     error
}

func (r *roleSpyDependencyEditor) AddDependencies(_ context.Context, req issueops.AddDependenciesRequest) (issueops.AddDependenciesResult, error) {
	r.added = append(r.added, req)
	return issueops.AddDependenciesResult{}, r.err
}

func (r *roleSpyDependencyEditor) RemoveDependency(_ context.Context, req issueops.RemoveDependencyRequest) (issueops.RemoveDependencyResult, error) {
	r.removed = append(r.removed, req)
	return issueops.RemoveDependencyResult{}, r.err
}

type roleSpyDeleter struct {
	requests []issueops.DeleteRequest
	err      error
	// failAfterCall, when non-zero, fails every call after the Nth.
	failAfterCall int
}

func (r *roleSpyDeleter) Delete(_ context.Context, req issueops.DeleteRequest) (issueops.DeleteResult, error) {
	r.requests = append(r.requests, req)
	if r.failAfterCall > 0 && len(r.requests) > r.failAfterCall {
		return issueops.DeleteResult{}, errors.New("server went away")
	}
	return issueops.DeleteResult{Deleted: len(req.IDs)}, r.err
}

type roleSpyStats struct {
	calls int
	err   error
}

func (r *roleSpyStats) Stats(context.Context, issueops.StatsRequest) (issueops.StatsResult, error) {
	r.calls++
	return issueops.StatsResult{}, r.err
}

func (r *roleSpyStats) AssigneeStats(context.Context, issueops.AssigneeStatsRequest) (issueops.StatsResult, error) {
	return issueops.StatsResult{}, errors.New("AssigneeStats is not the ping probe")
}

// roleStorageSpy answers only through role accessors. Every raw method a
// re-point is supposed to have left behind refuses, so a partial re-point is a
// test failure rather than a silent regression on the served backend.
type roleStorageSpy struct {
	beadslib.Storage
	edges     *roleSpyEdges
	relations *roleSpyRelations
	deps      *roleSpyDependencyEditor
	deleter   *roleSpyDeleter
	stats     *roleSpyStats
}

func newRoleStorageSpy() *roleStorageSpy {
	return &roleStorageSpy{
		edges:     &roleSpyEdges{},
		relations: &roleSpyRelations{},
		deps:      &roleSpyDependencyEditor{},
		deleter:   &roleSpyDeleter{},
		stats:     &roleSpyStats{},
	}
}

func (s *roleStorageSpy) EdgeReader() (issueops.EdgeReader, error) { return s.edges, nil }
func (s *roleStorageSpy) IssueRelations() (issueops.Relations, error) {
	return s.relations, nil
}
func (s *roleStorageSpy) DependencyEditor() (issueops.DependencyEditor, error) { return s.deps, nil }
func (s *roleStorageSpy) Deleter() (issueops.Deleter, error)                   { return s.deleter, nil }
func (s *roleStorageSpy) StatsReporter() (issueops.StatsReporter, error)       { return s.stats, nil }

// The raw refusals. Reaching any of these is the bug this file guards.
func unservedRaw(op string) error {
	return &beadslib.ErrUnsupported{Op: op, Backend: "http"}
}

func (s *roleStorageSpy) AddDependency(context.Context, *beadslib.Dependency, string) error {
	return unservedRaw("AddDependency")
}

func (s *roleStorageSpy) RemoveDependency(context.Context, string, string, string) error {
	return unservedRaw("RemoveDependency")
}

func (s *roleStorageSpy) GetDependenciesWithMetadata(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	return nil, unservedRaw("GetDependenciesWithMetadata")
}

func (s *roleStorageSpy) GetDependentsWithMetadata(context.Context, string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	return nil, unservedRaw("GetDependentsWithMetadata")
}

func (s *roleStorageSpy) DeleteIssue(context.Context, string) error {
	return unservedRaw("DeleteIssue")
}

// GetStatistics is deliberately NOT overridden: the embedded nil
// beadslib.Storage panics if a Ping still reaches it, which is louder than a
// returned refusal and cannot be swallowed by an errors.Is arm.

func TestDepAddGoesThroughTheDependencyEditor(t *testing.T) {
	spy := newRoleStorageSpy()
	store := newNativeDoltStoreForTest(spy)

	if err := store.DepAdd("gc-1", "gc-2", "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if len(spy.deps.added) != 1 {
		t.Fatalf("AddDependencies called %d times, want 1", len(spy.deps.added))
	}
	req := spy.deps.added[0]
	if req.Actor != "native-test" {
		t.Errorf("actor = %q, want the store actor", req.Actor)
	}
	want := []issueops.DependencyEdge{{IssueID: "gc-1", DependsOnID: "gc-2", Type: beadslib.DepBlocks}}
	if !reflect.DeepEqual(req.Edges, want) {
		t.Errorf("edges = %+v, want %+v", req.Edges, want)
	}
}

// An empty type must reach the role as the same default the raw path used, or
// a gc caller that omits it writes an edge with an empty type on the wire.
func TestDepAddDefaultsTheEdgeTypeTheWayTheRawPathDid(t *testing.T) {
	spy := newRoleStorageSpy()
	store := newNativeDoltStoreForTest(spy)

	if err := store.DepAdd("gc-1", "gc-2", ""); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if got := spy.deps.added[0].Edges[0].Type; got != beadslib.DepBlocks {
		t.Errorf("edge type = %q, want the blocks default", got)
	}
}

func TestDepRemoveGoesThroughTheDependencyEditor(t *testing.T) {
	spy := newRoleStorageSpy()
	store := newNativeDoltStoreForTest(spy)

	if err := store.DepRemove("gc-1", "gc-2"); err != nil {
		t.Fatalf("DepRemove: %v", err)
	}
	want := []issueops.RemoveDependencyRequest{{Actor: "native-test", IssueID: "gc-1", DependsOnID: "gc-2"}}
	if !reflect.DeepEqual(spy.deps.removed, want) {
		t.Errorf("removals = %+v, want %+v", spy.deps.removed, want)
	}
}

// DOWN is the EDGE question, and it must be answered by the edge reader rather
// than by Relations: an edge whose target is an "external:" reference or an id
// in another repository is a row this database holds and no issue it can join
// to, so Relations DROPS it. gc's dep tree walks those edges.
func TestDepListDownReadsEdgesNotNeighbours(t *testing.T) {
	spy := newRoleStorageSpy()
	spy.edges.result = issueops.EdgeReadResult{Anchors: []issueops.AnchorEdges{{
		ID: "gc-1",
		Edges: []*issueops.Dependency{
			{IssueID: "gc-1", DependsOnID: "gc-2", Type: beadslib.DepBlocks},
			{IssueID: "gc-1", DependsOnID: "external:jira-9", Type: beadslib.DepRelated},
		},
	}}}
	store := newNativeDoltStoreForTest(spy)

	got, err := store.DepList("gc-1", "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	want := []Dep{
		{IssueID: "gc-1", DependsOnID: "gc-2", Type: "blocks"},
		{IssueID: "gc-1", DependsOnID: "external:jira-9", Type: "related"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DepList(down) = %+v, want %+v", got, want)
	}
	if len(spy.edges.requests) != 1 || len(spy.edges.requests[0].IDs) != 1 || spy.edges.requests[0].IDs[0] != "gc-1" {
		t.Errorf("EdgeReadRequest = %+v, want the single anchor gc-1", spy.edges.requests)
	}
	if len(spy.relations.requests) != 0 {
		t.Errorf("the DOWN direction reached Relations: %+v", spy.relations.requests)
	}
}

// An anchor with no edges is a real answer, and an anchor the read did not
// return at all must not be mistaken for one: DepList's callers treat an empty
// slice as "no dependencies", so a dropped anchor would read as a clean graph.
func TestDepListDownReportsAMissingAnchorAsNotFound(t *testing.T) {
	spy := newRoleStorageSpy()
	spy.edges.result = issueops.EdgeReadResult{Anchors: []issueops.AnchorEdges{{ID: "gc-1", Missing: true}}}
	store := newNativeDoltStoreForTest(spy)

	if _, err := store.DepList("gc-1", "down"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("DepList over a missing anchor = %v, want ErrNotFound", err)
	}
}

// UP is the NEIGHBOR question: who depends on this. Relations answers it, and
// gc's Dep is oriented from the dependent toward the anchor.
func TestDepListUpReadsRelations(t *testing.T) {
	spy := newRoleStorageSpy()
	spy.relations.result = []*issueops.RelatedIssue{
		{Issue: beadslib.Issue{ID: "gc-9"}, DependencyType: beadslib.DepParentChild},
	}
	store := newNativeDoltStoreForTest(spy)

	got, err := store.DepList("gc-1", "up")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	want := []Dep{{IssueID: "gc-9", DependsOnID: "gc-1", Type: "parent-child"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DepList(up) = %+v, want %+v", got, want)
	}
	if len(spy.relations.requests) != 1 || spy.relations.requests[0].Direction != issueops.RelationIn {
		t.Fatalf("RelatedRequest = %+v, want direction in — RelationOut would answer the exact inverse graph with the same shape", spy.relations.requests)
	}
	if len(spy.edges.requests) != 0 {
		t.Errorf("the UP direction reached the edge reader: %+v", spy.edges.requests)
	}
}

func TestDeleteGoesThroughTheDeleterAndOrphansDependents(t *testing.T) {
	spy := newRoleStorageSpy()
	store := newNativeDoltStoreForTest(spy)

	if err := store.Delete("gc-1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(spy.deleter.requests) != 1 {
		t.Fatalf("Delete called the role %d times, want 1", len(spy.deleter.requests))
	}
	req := spy.deleter.requests[0]
	if !reflect.DeepEqual(req.IDs, []string{"gc-1"}) {
		t.Errorf("ids = %+v, want the single id", req.IDs)
	}
	if !req.Force {
		t.Error("Delete is not forced; the raw DeleteIssue it replaces applied no dependents guard, so an unforced role call would refuse deletes gc performs today")
	}
	if req.Cascade {
		t.Error("Delete cascades; the raw method deleted exactly the named row and gc's wisp GC must not reach live work outside its closure")
	}
	if req.ExpectedVersion != nil {
		t.Error("Delete carries an ExpectedVersion; the unconditional delete has no token to compare")
	}
}

func TestDeleteBatchChunksAtTheRequestCap(t *testing.T) {
	spy := newRoleStorageSpy()
	store := newNativeDoltStoreForTest(spy)

	ids := deleteBatchIDsForTest(maxNativeDeleteIDs + 1)
	if err := store.DeleteBatch(ids); err != nil {
		t.Fatalf("DeleteBatch: %v", err)
	}
	if len(spy.deleter.requests) != 2 {
		t.Fatalf("DeleteBatch made %d requests for %d ids, want 2 chunks at the %d cap", len(spy.deleter.requests), len(ids), maxNativeDeleteIDs)
	}
	if got := len(spy.deleter.requests[0].IDs); got != maxNativeDeleteIDs {
		t.Errorf("first chunk carried %d ids, want the full cap %d", got, maxNativeDeleteIDs)
	}
	if got := len(spy.deleter.requests[1].IDs); got != 1 {
		t.Errorf("second chunk carried %d ids, want the remainder", got)
	}
}

// A batch that commits some chunks and then fails must name what landed, or the
// caching layer above it leaves deleted beads present-but-stale.
func TestDeleteBatchReportsWhatCommittedBeforeAFailure(t *testing.T) {
	spy := newRoleStorageSpy()
	spy.deleter = &roleSpyDeleter{failAfterCall: 1}
	store := newNativeDoltStoreForTest(spy)

	ids := deleteBatchIDsForTest(maxNativeDeleteIDs + 1)
	err := store.DeleteBatch(ids)
	var partial *BatchDeleteError
	if !errors.As(err, &partial) {
		t.Fatalf("DeleteBatch error = %v, want a *BatchDeleteError naming the committed chunk", err)
	}
	if len(partial.Committed) != maxNativeDeleteIDs {
		t.Errorf("committed = %d ids, want the first full chunk (%d)", len(partial.Committed), maxNativeDeleteIDs)
	}
}

func deleteBatchIDsForTest(n int) []string {
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		ids = append(ids, fmt.Sprintf("gc-%d", i))
	}
	return ids
}

func TestPingProbesTheStatsRoleNotTheOffRoleBridge(t *testing.T) {
	spy := newRoleStorageSpy()
	store := newNativeDoltStoreForTest(spy)

	if err := store.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if spy.stats.calls != 1 {
		t.Fatalf("StatsReporter.Stats called %d times, want 1", spy.stats.calls)
	}
}

func TestPingReportsAnUnreachableStore(t *testing.T) {
	spy := newRoleStorageSpy()
	spy.stats.err = errors.New("dial tcp: connection refused")
	store := newNativeDoltStoreForTest(spy)

	if err := store.Ping(); err == nil {
		t.Fatal("Ping succeeded against an unreachable store")
	}
}

// A batch that commits some chunks and then fails must clean the SIDECAR of
// what it committed, not only report it. The clone-local strings live outside
// the ledger, so nothing else ever collects them: a row named in
// BatchDeleteError.Committed is a row the caller will tombstone, and its
// sidecar entry would then outlive every trace of the bead it belongs to.
func TestDeleteBatchCleansTheSidecarOfWhatItCommitted(t *testing.T) {
	spy := newRoleStorageSpy()
	spy.deleter = &roleSpyDeleter{failAfterCall: 1}
	store := newNativeDoltStoreForTest(spy)
	store.localStrings = newLocalSidecar(filepath.Join(t.TempDir(), "local-strings.json"))

	ids := deleteBatchIDsForTest(maxNativeDeleteIDs + 1)
	committed, doomed := ids[0], ids[len(ids)-1]
	for _, id := range []string{committed, doomed} {
		if err := store.SetLocalString(id, "worktree", "/tmp/"+id); err != nil {
			t.Fatalf("SetLocalString(%s): %v", id, err)
		}
	}

	var partial *BatchDeleteError
	if err := store.DeleteBatch(ids); !errors.As(err, &partial) {
		t.Fatalf("DeleteBatch error = %v, want a *BatchDeleteError", err)
	}
	if !slices.Contains(partial.Committed, committed) {
		t.Fatalf("the committed chunk does not name %s: %d ids", committed, len(partial.Committed))
	}

	got, err := store.GetLocalString(committed, "worktree")
	if err != nil {
		t.Fatalf("GetLocalString after a partial batch: %v", err)
	}
	if got != "" {
		t.Errorf("the sidecar of a COMMITTED id survived the partial batch: %q", got)
	}
	// The chunk that never landed keeps its sidecar: the bead is still there.
	if got, err := store.GetLocalString(doomed, "worktree"); err != nil || got == "" {
		t.Errorf("GetLocalString(%s) = (%q, %v), want the sidecar of an id the batch never deleted", doomed, got, err)
	}
}

// DEPMETADATA IS THE EDGE ROW'S OWN COLUMN, so it rides the same EdgeReader the
// DOWN leg of DepList does. The rows that carry a payload most often are
// precisely the ones Relations cannot answer for — an "external:" reference or
// an id belonging to another repository has no issue on the far end — so a
// neighbor read would report "no payload" for exactly the edges the payload
// exists to annotate.
func TestDepMetadataReadsTheEdgeRowThroughTheEdgeReader(t *testing.T) {
	spy := newRoleStorageSpy()
	spy.edges.result = issueops.EdgeReadResult{Anchors: []issueops.AnchorEdges{
		{
			ID: "gc-1",
			Edges: []*issueops.Dependency{
				{IssueID: "gc-1", DependsOnID: "gc-2", Type: beadslib.DepBlocks, Metadata: `{"wait":"step"}`},
				{IssueID: "gc-1", DependsOnID: "external:jira-9", Type: beadslib.DepRelated, Metadata: `{"src":"jira"}`},
				{IssueID: "gc-1", DependsOnID: "other-repo-4", Type: beadslib.DepRelated, Metadata: `{"src":"fork"}`},
				{IssueID: "gc-1", DependsOnID: "gc-3", Type: beadslib.DepBlocks},
			},
		},
		// A SECOND ANCHOR, holding an edge onto one of the same targets and one
		// of its own. The served leg regroups a flat edge array back into
		// anchors by hand, so an answer attributed to the wrong source is a live
		// failure mode; these two rows are what makes the anchor guard fail when
		// it is removed.
		{
			ID: "gc-9",
			Edges: []*issueops.Dependency{
				{IssueID: "gc-9", DependsOnID: "gc-2", Type: beadslib.DepBlocks, Metadata: `{"wait":"WRONG-ANCHOR"}`},
				{IssueID: "gc-9", DependsOnID: "gc-7", Type: beadslib.DepBlocks, Metadata: `{"wait":"NOT-OURS"}`},
			},
		},
	}}
	store := newNativeDoltStoreForTest(spy)

	for _, tc := range []struct {
		target  string
		want    string
		wantOK  bool
		comment string
	}{
		{target: "gc-2", want: `{"wait":"step"}`, wantOK: true, comment: "a resolved target"},
		{target: "external:jira-9", want: `{"src":"jira"}`, wantOK: true, comment: "an external reference"},
		{target: "other-repo-4", want: `{"src":"fork"}`, wantOK: true, comment: "a bare slug from another repository"},
		{target: "gc-3", want: "", wantOK: false, comment: "an edge that carried no payload"},
		{target: "gc-404", want: "", wantOK: false, comment: "an edge this anchor does not hold"},
		{target: "gc-7", want: "", wantOK: false, comment: "an edge another anchor holds"},
	} {
		got, ok, err := store.DepMetadata("gc-1", tc.target)
		if err != nil {
			t.Fatalf("DepMetadata(%s): %v", tc.comment, err)
		}
		if got != tc.want || ok != tc.wantOK {
			t.Errorf("DepMetadata(%s) = (%q, %v), want (%q, %v)", tc.comment, got, ok, tc.want, tc.wantOK)
		}
	}
	if len(spy.relations.requests) != 0 {
		t.Errorf("DepMetadata reached Relations: %+v — an edge onto an absent target is dropped there", spy.relations.requests)
	}
	for _, req := range spy.edges.requests {
		if len(req.IDs) != 1 || req.IDs[0] != "gc-1" {
			t.Errorf("EdgeReadRequest = %+v, want the single anchor gc-1", req)
		}
		if len(req.Types) != 0 {
			t.Errorf("EdgeReadRequest carries a type filter %+v; a payload lookup is keyed by target, and filtering by type would hide the edge whose type the caller does not know", req.Types)
		}
	}
}

// An anchor that is not there answers ABSENCE rather than ErrNotFound, unlike
// DepList. The (string, bool) shape already carries the absent channel, and the
// callers are edge-payload lookups rather than graph walks — the danger DepList
// guards against (a typo reading as a clean graph) has no analog here.
func TestDepMetadataReportsAMissingAnchorAsAbsence(t *testing.T) {
	spy := newRoleStorageSpy()
	spy.edges.result = issueops.EdgeReadResult{Anchors: []issueops.AnchorEdges{{ID: "gc-1", Missing: true}}}
	store := newNativeDoltStoreForTest(spy)

	got, ok, err := store.DepMetadata("gc-1", "gc-2")
	if err != nil {
		t.Fatalf("DepMetadata over a missing anchor: %v", err)
	}
	if got != "" || ok {
		t.Errorf("DepMetadata over a missing anchor = (%q, %v), want absence", got, ok)
	}
}

// A transport failure is an error, never absence: reporting an unreachable
// server as "this edge carried no payload" is the answer a witness acts on.
func TestDepMetadataReportsAReadFailureRatherThanAbsence(t *testing.T) {
	spy := newRoleStorageSpy()
	spy.edges.err = errors.New("server went away")
	store := newNativeDoltStoreForTest(spy)

	if _, ok, err := store.DepMetadata("gc-1", "gc-2"); err == nil || ok {
		t.Fatalf("DepMetadata over a failing read = (%v, %v), want the failure", ok, err)
	}
}
