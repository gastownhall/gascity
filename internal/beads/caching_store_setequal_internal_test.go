package beads

import "testing"

// Reordered label/needs/dependency sets must NOT read as a change: the Dolt gcg
// rig store does not guarantee a stable element order across scans, so an
// order-sensitive comparison re-fired bead.updated every reconcile pass and
// flooded cache-reconcile (ga-ocypq2).
func TestBeadChangedIgnoresSetOrder(t *testing.T) {
	base := Bead{
		ID:     "gcg-wisp-x",
		Title:  "t",
		Status: "open",
		Type:   "task",
		Labels: []string{"a", "b", "c"},
		Needs:  []string{"n1", "n2"},
		Dependencies: []Dep{
			{IssueID: "x", DependsOnID: "d1", Type: "blocks"},
			{IssueID: "x", DependsOnID: "d2", Type: "tracks"},
		},
	}
	reordered := base
	reordered.Labels = []string{"c", "a", "b"}
	reordered.Needs = []string{"n2", "n1"}
	reordered.Dependencies = []Dep{
		{IssueID: "x", DependsOnID: "d2", Type: "tracks"},
		{IssueID: "x", DependsOnID: "d1", Type: "blocks"},
	}
	if beadChanged(base, reordered, false) {
		t.Error("beadChanged = true for a pure label/needs/dep reorder; want false")
	}
	if depsChanged(base.Dependencies, reordered.Dependencies) {
		t.Error("depsChanged = true for a pure dependency reorder; want false")
	}

	// A real content change must still be detected.
	labelAdded := base
	labelAdded.Labels = []string{"a", "b", "c", "d"}
	if !beadChanged(base, labelAdded, false) {
		t.Error("beadChanged = false when a label was added; want true")
	}
	depChanged := base
	depChanged.Dependencies = []Dep{
		{IssueID: "x", DependsOnID: "d1", Type: "blocks"},
		{IssueID: "x", DependsOnID: "d3", Type: "tracks"}, // d3 != d2
	}
	if !depsChanged(base.Dependencies, depChanged.Dependencies) {
		t.Error("depsChanged = false when a dependency target changed; want true")
	}
}

func TestStringSetEqual(t *testing.T) {
	cases := []struct {
		a, b []string
		want bool
	}{
		{nil, nil, true},
		{[]string{}, nil, true},
		{[]string{"a", "b"}, []string{"b", "a"}, true},
		{[]string{"a", "a", "b"}, []string{"a", "b", "a"}, true},
		{[]string{"a", "a"}, []string{"a", "b"}, false}, // multiset, not just set
		{[]string{"a"}, []string{"a", "b"}, false},
	}
	for _, c := range cases {
		if got := stringSetEqual(c.a, c.b); got != c.want {
			t.Errorf("stringSetEqual(%v,%v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestDepSetEqual(t *testing.T) {
	d1 := Dep{IssueID: "x", DependsOnID: "d1", Type: "blocks"}
	d2 := Dep{IssueID: "x", DependsOnID: "d2", Type: "tracks"}
	if !depSetEqual([]Dep{d1, d2}, []Dep{d2, d1}) {
		t.Error("depSetEqual = false for reordered equal sets; want true")
	}
	if depSetEqual([]Dep{d1, d1}, []Dep{d1, d2}) {
		t.Error("depSetEqual = true for different multisets; want false")
	}
}
