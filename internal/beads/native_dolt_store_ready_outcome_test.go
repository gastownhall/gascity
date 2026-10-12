package beads

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

// readyOutcomeBatchStorage is a beadslib.Storage that answers the three reads
// the native Ready work-outcome filter can make — the per-candidate
// GetDependenciesWithMetadata, and the batched GetDependencyRecordsForIssues +
// GetIssuesByIDs pair — from one in-memory edge/issue graph, counting every
// storage API call by kind.
type readyOutcomeBatchStorage struct {
	beadslib.Storage
	edges  map[string][]*beadslib.Dependency
	issues map[string]*beadslib.Issue

	perCandidateCalls int
	edgeCalls         int
	issueCalls        int
}

// GetDependenciesWithMetadata answers the per-candidate read the fallback path
// makes: the candidate's edges joined to their target rows, skipping a target
// the graph does not hold, the way the Dolt store does.
func (f *readyOutcomeBatchStorage) GetDependenciesWithMetadata(_ context.Context, issueID string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	f.perCandidateCalls++
	var out []*beadslib.IssueWithDependencyMetadata
	for _, dep := range f.edges[issueID] {
		if dep == nil {
			continue
		}
		issue, ok := f.issues[dep.DependsOnID]
		if !ok || issue == nil {
			continue
		}
		out = append(out, &beadslib.IssueWithDependencyMetadata{Issue: *issue, DependencyType: dep.Type})
	}
	return out, nil
}

// GetDependencyRecordsForIssues answers the batched source-keyed edge read,
// returning each held id's edges exactly as stored (nil entries included).
func (f *readyOutcomeBatchStorage) GetDependencyRecordsForIssues(_ context.Context, issueIDs []string) (map[string][]*beadslib.Dependency, error) {
	f.edgeCalls++
	out := make(map[string][]*beadslib.Dependency, len(issueIDs))
	for _, id := range issueIDs {
		if deps, ok := f.edges[id]; ok {
			out[id] = deps
		}
	}
	return out, nil
}

// GetIssuesByIDs answers the batched issue read, returning each held id's row
// exactly as stored (a nil row included) and nothing for an unknown id.
func (f *readyOutcomeBatchStorage) GetIssuesByIDs(_ context.Context, ids []string) ([]*beadslib.Issue, error) {
	f.issueCalls++
	var out []*beadslib.Issue
	for _, id := range ids {
		if issue, ok := f.issues[id]; ok {
			out = append(out, issue)
		}
	}
	return out, nil
}

// readyOutcomePerCandidateOnlyStorage hides the batched edge read behind a
// plain beadslib.Storage so the same graph can be run through the fallback
// path. It is a value wrapper, not an embedding, so the capability probe
// cannot see through it.
type readyOutcomePerCandidateOnlyStorage struct {
	beadslib.Storage
	inner *readyOutcomeBatchStorage
}

// GetDependenciesWithMetadata forwards the only read the fallback path makes.
func (f *readyOutcomePerCandidateOnlyStorage) GetDependenciesWithMetadata(ctx context.Context, issueID string) ([]*beadslib.IssueWithDependencyMetadata, error) {
	return f.inner.GetDependenciesWithMetadata(ctx, issueID)
}

func readyOutcomeIssue(id string, status beadslib.Status, metadata string) *beadslib.Issue {
	issue := &beadslib.Issue{ID: id, Title: id, Status: status, IssueType: beadslib.TypeTask, Priority: 2}
	if metadata != "" {
		issue.Metadata = json.RawMessage(metadata)
	}
	return issue
}

func readyOutcomeEdge(from, to, depType string) *beadslib.Dependency {
	return &beadslib.Dependency{IssueID: from, DependsOnID: to, Type: beadslib.DependencyType(depType)}
}

const readyOutcomeBlockedMeta = `{"gc.work_outcome":"blocked"}`

// readyOutcomeShapedGraph builds the graph every shape of the veto rule sees:
// no edges; closed+blocked (vetoed); closed without an outcome (kept); open
// with the blocked outcome (kept); a non-ready-blocking edge to a vetoing
// target (ignored); mixed edges where one vetoes (vetoed); a dangling target
// (kept); a nil edge entry and a nil issue row (tolerated); plus 25 bulk
// candidates behind two closed-without-outcome blockers each. It returns the
// storage, the candidates in order, and the ids the rule must remove.
func readyOutcomeShapedGraph() (*readyOutcomeBatchStorage, []Bead, map[string]bool) {
	storage := &readyOutcomeBatchStorage{
		edges: map[string][]*beadslib.Dependency{
			"gc-vetoed":       {readyOutcomeEdge("gc-vetoed", "gc-gaveup", "blocks")},
			"gc-closed-plain": {readyOutcomeEdge("gc-closed-plain", "gc-done", "blocks")},
			"gc-open-blocked": {readyOutcomeEdge("gc-open-blocked", "gc-open-gaveup", "waits-for")},
			"gc-nonblocking":  {readyOutcomeEdge("gc-nonblocking", "gc-gaveup", "related")},
			"gc-mixed": {
				readyOutcomeEdge("gc-mixed", "gc-done", "blocks"),
				readyOutcomeEdge("gc-mixed", "gc-gaveup", "conditional-blocks"),
			},
			"gc-dangling":  {readyOutcomeEdge("gc-dangling", "gc-missing", "blocks")},
			"gc-nil-edge":  {nil, readyOutcomeEdge("gc-nil-edge", "gc-done", "blocks")},
			"gc-nil-issue": {readyOutcomeEdge("gc-nil-issue", "gc-nil-row", "blocks")},
		},
		issues: map[string]*beadslib.Issue{
			"gc-gaveup":      readyOutcomeIssue("gc-gaveup", beadslib.StatusClosed, readyOutcomeBlockedMeta),
			"gc-done":        readyOutcomeIssue("gc-done", beadslib.StatusClosed, ""),
			"gc-open-gaveup": readyOutcomeIssue("gc-open-gaveup", beadslib.StatusOpen, readyOutcomeBlockedMeta),
			"gc-nil-row":     nil,
		},
	}
	candidates := []Bead{
		{ID: "gc-none"},
		{ID: "gc-vetoed"},
		{ID: "gc-closed-plain"},
		{ID: "gc-open-blocked"},
		{ID: "gc-nonblocking"},
		{ID: "gc-mixed"},
		{ID: "gc-dangling"},
		{ID: "gc-nil-edge"},
		{ID: "gc-nil-issue"},
	}
	for i := range 25 {
		id := fmt.Sprintf("gc-bulk-%02d", i)
		blocker := fmt.Sprintf("gc-bulk-blocker-%02d", i)
		candidates = append(candidates, Bead{ID: id})
		storage.edges[id] = []*beadslib.Dependency{
			readyOutcomeEdge(id, "gc-done", "blocks"),
			readyOutcomeEdge(id, blocker, "blocks"),
		}
		storage.issues[blocker] = readyOutcomeIssue(blocker, beadslib.StatusClosed, "")
	}
	return storage, candidates, map[string]bool{"gc-vetoed": true, "gc-mixed": true}
}

func readyOutcomeIDs(beads []Bead) []string {
	ids := make([]string, 0, len(beads))
	for _, b := range beads {
		ids = append(ids, b.ID)
	}
	return ids
}

func assertReadyOutcomeFiltered(t *testing.T, got, candidates []Bead, removed map[string]bool) {
	t.Helper()
	gotIDs := make(map[string]bool, len(got))
	for _, b := range got {
		gotIDs[b.ID] = true
	}
	for _, c := range candidates {
		if want := !removed[c.ID]; gotIDs[c.ID] != want {
			t.Errorf("candidate %s kept = %v, want %v", c.ID, gotIDs[c.ID], want)
		}
	}
	if len(got) != len(candidates)-len(removed) {
		t.Errorf("filtered set has %d beads, want %d", len(got), len(candidates)-len(removed))
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFilterBatchesBlockerRead pins the cost
// of the gc.work_outcome veto on a batch-capable storage: N candidates cost
// exactly one edge read and one issue read, not one storage API call per
// candidate (#6491 — on a served store each call is a round trip, and one
// per candidate exhausted the whole native read-retry budget every controller
// tick). The filtered set must be exactly what the veto rule says.
func TestNativeDoltStoreReadyWorkOutcomeFilterBatchesBlockerRead(t *testing.T) {
	storage, candidates, removed := readyOutcomeShapedGraph()
	store := newNativeDoltStoreForTest(storage)

	got, err := store.filterReadyByWorkOutcome(context.Background(), storage, candidates)
	if err != nil {
		t.Fatalf("filterReadyByWorkOutcome: %v", err)
	}
	if storage.edgeCalls != 1 || storage.issueCalls != 1 || storage.perCandidateCalls != 0 {
		t.Errorf("storage API calls for %d candidates: edge=%d issue=%d per-candidate=%d, want edge=1 issue=1 per-candidate=0: the blocked-outcome blocker read must be batched, not one call per candidate",
			len(candidates), storage.edgeCalls, storage.issueCalls, storage.perCandidateCalls)
	}
	assertReadyOutcomeFiltered(t, got, candidates, removed)
}

// TestNativeDoltStoreReadyWorkOutcomeFilterPathsAgree runs the SAME shaped
// graph through the batched path and, behind a wrapper that hides the batched
// edge read, through the per-candidate fallback, and pins that the two return
// the identical filtered set — the fallback is the reference the batch must
// reproduce.
func TestNativeDoltStoreReadyWorkOutcomeFilterPathsAgree(t *testing.T) {
	storage, candidates, removed := readyOutcomeShapedGraph()
	store := newNativeDoltStoreForTest(storage)

	batched, err := store.filterReadyByWorkOutcome(context.Background(), storage, candidates)
	if err != nil {
		t.Fatalf("batched filterReadyByWorkOutcome: %v", err)
	}
	perCandidate := &readyOutcomePerCandidateOnlyStorage{inner: storage}
	fallback, err := store.filterReadyByWorkOutcome(context.Background(), perCandidate, candidates)
	if err != nil {
		t.Fatalf("fallback filterReadyByWorkOutcome: %v", err)
	}
	if storage.perCandidateCalls != len(candidates) {
		t.Errorf("fallback per-candidate calls = %d, want %d (one per candidate): the wrapper did not hide the batched read", storage.perCandidateCalls, len(candidates))
	}
	if got, want := readyOutcomeIDs(fallback), readyOutcomeIDs(batched); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("fallback filtered = %v, batched filtered = %v: the two paths must agree", got, want)
	}
	assertReadyOutcomeFiltered(t, fallback, candidates, removed)
}

// TestNativeDoltStoreReadyWorkOutcomeFilterBatchIsEdgeOrderIndependent pins
// that the batched verdict — kept, vetoed, or which malformed blocker the error
// names — does not change with the order the store returns a candidate's
// edges in. The batched edge read is store-sorted and the per-candidate read
// is not, so an order-sensitive veto would let the two paths disagree.
func TestNativeDoltStoreReadyWorkOutcomeFilterBatchIsEdgeOrderIndependent(t *testing.T) {
	// gc-dep references a vetoing blocker and a malformed one; the error must
	// win in both permutations, naming the same candidate and blocker.
	// gc-two-bad references two malformed blockers; the lower id must be named
	// in both permutations.
	vetoEdge := readyOutcomeEdge("gc-dep", "gc-gaveup", "blocks")
	badEdge := readyOutcomeEdge("gc-dep", "gc-garbled", "blocks")
	badA := readyOutcomeEdge("gc-two-bad", "gc-garbled-a", "blocks")
	badB := readyOutcomeEdge("gc-two-bad", "gc-garbled-b", "blocks")
	issues := map[string]*beadslib.Issue{
		"gc-gaveup":    readyOutcomeIssue("gc-gaveup", beadslib.StatusClosed, readyOutcomeBlockedMeta),
		"gc-garbled":   readyOutcomeIssue("gc-garbled", beadslib.StatusClosed, `{not json`),
		"gc-garbled-a": readyOutcomeIssue("gc-garbled-a", beadslib.StatusClosed, `{not json`),
		"gc-garbled-b": readyOutcomeIssue("gc-garbled-b", beadslib.StatusClosed, `{not json`),
	}
	tests := []struct {
		name       string
		edges      map[string][]*beadslib.Dependency
		candidates []Bead
		wantErr    []string
	}{
		{
			name:       "veto before malformed",
			edges:      map[string][]*beadslib.Dependency{"gc-dep": {vetoEdge, badEdge}},
			candidates: []Bead{{ID: "gc-dep"}},
			wantErr:    []string{"gc-dep", "gc-garbled"},
		},
		{
			name:       "malformed before veto",
			edges:      map[string][]*beadslib.Dependency{"gc-dep": {badEdge, vetoEdge}},
			candidates: []Bead{{ID: "gc-dep"}},
			wantErr:    []string{"gc-dep", "gc-garbled"},
		},
		{
			name:       "two malformed, a first",
			edges:      map[string][]*beadslib.Dependency{"gc-two-bad": {badA, badB}},
			candidates: []Bead{{ID: "gc-two-bad"}},
			wantErr:    []string{"gc-two-bad", "gc-garbled-a"},
		},
		{
			name:       "two malformed, b first",
			edges:      map[string][]*beadslib.Dependency{"gc-two-bad": {badB, badA}},
			candidates: []Bead{{ID: "gc-two-bad"}},
			wantErr:    []string{"gc-two-bad", "gc-garbled-a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			storage := &readyOutcomeBatchStorage{edges: tt.edges, issues: issues}
			store := newNativeDoltStoreForTest(storage)
			_, err := store.filterReadyByWorkOutcome(context.Background(), storage, tt.candidates)
			if err == nil {
				t.Fatal("filterReadyByWorkOutcome: want an error for unparsable blocker metadata, got nil")
			}
			for _, want := range tt.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err, want)
				}
			}
		})
	}

	// And with no malformed blocker in play, the verdict itself: a vetoing
	// edge is found whether it sorts first or last among a candidate's edges.
	for _, edges := range [][]*beadslib.Dependency{
		{readyOutcomeEdge("gc-mixed", "gc-done", "blocks"), readyOutcomeEdge("gc-mixed", "gc-gaveup", "blocks")},
		{readyOutcomeEdge("gc-mixed", "gc-gaveup", "blocks"), readyOutcomeEdge("gc-mixed", "gc-done", "blocks")},
	} {
		storage := &readyOutcomeBatchStorage{
			edges:  map[string][]*beadslib.Dependency{"gc-mixed": edges},
			issues: map[string]*beadslib.Issue{"gc-gaveup": issues["gc-gaveup"], "gc-done": readyOutcomeIssue("gc-done", beadslib.StatusClosed, "")},
		}
		store := newNativeDoltStoreForTest(storage)
		got, err := store.filterReadyByWorkOutcome(context.Background(), storage, []Bead{{ID: "gc-mixed"}})
		if err != nil {
			t.Fatalf("filterReadyByWorkOutcome: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("edges %s,%s: filtered = %v, want empty (vetoed regardless of edge order)", edges[0].DependsOnID, edges[1].DependsOnID, readyOutcomeIDs(got))
		}
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFilterBatchReportsBadBlockerMetadata pins
// that the batched path keeps the per-candidate path's failure contract for a
// hydrated blocker: metadata that does not parse is an error naming the
// candidate and the blocker, never a silently-kept or silently-dropped
// candidate.
func TestNativeDoltStoreReadyWorkOutcomeFilterBatchReportsBadBlockerMetadata(t *testing.T) {
	storage := &readyOutcomeBatchStorage{
		edges:  map[string][]*beadslib.Dependency{"gc-dep": {readyOutcomeEdge("gc-dep", "gc-garbled", "blocks")}},
		issues: map[string]*beadslib.Issue{"gc-garbled": readyOutcomeIssue("gc-garbled", beadslib.StatusClosed, `{not json`)},
	}
	store := newNativeDoltStoreForTest(storage)
	_, err := store.filterReadyByWorkOutcome(context.Background(), storage, []Bead{{ID: "gc-dep"}})
	if err == nil {
		t.Fatal("filterReadyByWorkOutcome: want an error for unparsable blocker metadata, got nil")
	}
	for _, want := range []string{"gc-dep", "gc-garbled"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// TestNativeDoltStoreReadyWorkOutcomeFilterFallsBackPerCandidate pins that a
// storage without the batched dependency-record read still gets the veto,
// through the original per-candidate GetDependenciesWithMetadata path, one
// storage API call per candidate.
func TestNativeDoltStoreReadyWorkOutcomeFilterFallsBackPerCandidate(t *testing.T) {
	calls := 0
	storage := &nativeDoltStorageSpy{
		getDependenciesWithMetadata: func(_ context.Context, id string) ([]*beadslib.IssueWithDependencyMetadata, error) {
			calls++
			if id != "gc-vetoed" {
				return nil, nil
			}
			return []*beadslib.IssueWithDependencyMetadata{
				nativeReadyGateBlocker("gc-gaveup", beadslib.StatusClosed, beadslib.DependencyType("blocks"), readyOutcomeBlockedMeta),
			}, nil
		},
	}
	store := newNativeDoltStoreForTest(storage)
	got, err := store.filterReadyByWorkOutcome(context.Background(), storage, []Bead{{ID: "gc-kept"}, {ID: "gc-vetoed"}})
	if err != nil {
		t.Fatalf("filterReadyByWorkOutcome: %v", err)
	}
	if calls != 2 {
		t.Errorf("per-candidate storage API calls = %d, want 2 (one per candidate on a storage with no batch read)", calls)
	}
	if len(got) != 1 || got[0].ID != "gc-kept" {
		t.Errorf("filtered = %+v, want [gc-kept]", got)
	}
}
