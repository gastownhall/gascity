package runproj

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBuildRunDetailPreservesStepDependencies proves the bead-derived detail
// projection preserves real step→step dependency edges (the supervisor snapshot's
// deps), not just the synthesized root→member parent edges. A regression here is
// what dropped the workflow dependency graph from the run-detail view.
func TestBuildRunDetailPreservesStepDependencies(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "beads_fixture.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var beadList []beads.Bead
	if err := json.Unmarshal(raw, &beadList); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}

	// Give rebase-check (dt-adopt1.2) a real dependency on preflight (dt-adopt1.1):
	// the kind of edge the supervisor RunSnapshot carried and the bead-derived
	// projection previously discarded.
	found := false
	for i := range beadList {
		if beadList[i].ID == "dt-adopt1.2" {
			beadList[i].Dependencies = []beads.Dep{
				{IssueID: "dt-adopt1.2", DependsOnID: "dt-adopt1.1", Type: "blocks"},
			}
			found = true
		}
	}
	if !found {
		t.Fatal("fixture missing dt-adopt1.2; test needs updating")
	}

	detail, err := BuildRunDetail(beadList, "dt-adopt1", 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}

	// The dependency must surface as a display edge between the two steps' semantic
	// nodes (preflight → rebase-check), carrying the dep type.
	want := RunDisplayEdge{From: "preflight", To: "rebase-check", Kind: "blocks"}
	if !hasEdge(detail.Edges, want) {
		t.Errorf("step→step dependency edge missing: want %+v, got edges %+v", want, detail.Edges)
	}
	// The additive fix must not drop the existing root→member parent edges.
	parent := RunDisplayEdge{From: "dt-adopt1", To: "preflight", Kind: "parent"}
	if !hasEdge(detail.Edges, parent) {
		t.Errorf("parent edge dropped: want %+v, got edges %+v", parent, detail.Edges)
	}
}

// TestSnapshotDepsMergesRealAndParentEdges unit-tests the dep synthesis helper:
// real Dependencies and Needs edges (prerequisite → dependent) are merged with
// root→member parent edges, edges to non-members are dropped, and duplicate
// edges (same from|to|kind) collapse.
func TestSnapshotDepsMergesRealAndParentEdges(t *testing.T) {
	members := []beads.Bead{
		{ID: "root"},
		{ID: "a", Dependencies: []beads.Dep{{IssueID: "a", DependsOnID: "root", Type: "blocks"}}},
		{ID: "b", Needs: []string{"a"}},
		{ID: "c", Dependencies: []beads.Dep{
			{IssueID: "c", DependsOnID: "a", Type: "blocks"},
			{IssueID: "c", DependsOnID: "outsider", Type: "blocks"}, // dropped: not a member
			{IssueID: "c", DependsOnID: "root", Type: "parent"},     // dupes the synthesized parent edge
		}},
	}

	deps := snapshotDeps(members)

	// Real dependency edges: prerequisite → dependent, carrying the dep type
	// (Needs carries no type, so it projects as the default dependency kind).
	assertHasDep(t, deps, runSnapshotDep{from: "root", to: "a", kind: "blocks"})
	assertHasDep(t, deps, runSnapshotDep{from: "a", to: "b", kind: ""})
	assertHasDep(t, deps, runSnapshotDep{from: "a", to: "c", kind: "blocks"})
	// Parent edges: root → each non-root member.
	assertHasDep(t, deps, runSnapshotDep{from: "root", to: "a", kind: "parent"})
	assertHasDep(t, deps, runSnapshotDep{from: "root", to: "b", kind: "parent"})
	assertHasDep(t, deps, runSnapshotDep{from: "root", to: "c", kind: "parent"})

	for _, d := range deps {
		if d.from == "outsider" || d.to == "outsider" {
			t.Errorf("edge to non-member leaked: %+v", d)
		}
	}

	rootCParent := 0
	for _, d := range deps {
		if d.from == "root" && d.to == "c" && d.kind == "parent" {
			rootCParent++
		}
	}
	if rootCParent != 1 {
		t.Errorf("root→c parent edge not deduped: count=%d, deps=%+v", rootCParent, deps)
	}
}

func hasEdge(edges []RunDisplayEdge, want RunDisplayEdge) bool {
	for _, e := range edges {
		if e == want {
			return true
		}
	}
	return false
}

func assertHasDep(t *testing.T, deps []runSnapshotDep, want runSnapshotDep) {
	t.Helper()
	for _, d := range deps {
		if d == want {
			return
		}
	}
	t.Errorf("missing dep %+v in %+v", want, deps)
}
