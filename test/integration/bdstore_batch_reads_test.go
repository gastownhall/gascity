//go:build integration

package integration

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestBdStoreBatchedDispatchReadsMatchPerCallReads pins the two BdStore
// capabilities the control dispatcher uses to save bd calls (ga-vnycm2.18)
// against a real bd: ChildrenOfAny answers exactly the union of the per-parent
// Children reads (closed children included, ephemeral children excluded as
// `bd list --parent` excludes them), and UpdateReporting reports the row the
// update wrote, which a follow-up Get confirms.
func TestBdStoreBatchedDispatchReadsMatchPerCallReads(t *testing.T) {
	requireDoltIntegration(t)
	store := newRealBdStoreWorkspace(t, "bt")

	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("create %s: %v", b.Title, err)
		}
		return created
	}
	p1 := mk(beads.Bead{Title: "parent one", Type: "task"})
	p2 := mk(beads.Bead{Title: "parent two", Type: "task"})
	c1 := mk(beads.Bead{Title: "child one", Type: "task", ParentID: p1.ID})
	mk(beads.Bead{Title: "child two", Type: "task", ParentID: p1.ID})
	mk(beads.Bead{Title: "child three", Type: "task", ParentID: p2.ID})
	mk(beads.Bead{Title: "ephemeral child", Type: "task", ParentID: p2.ID, Ephemeral: true})
	if err := store.Close(c1.ID); err != nil {
		t.Fatalf("close %s: %v", c1.ID, err)
	}

	var perParent []string
	for _, parent := range []string{p1.ID, p2.ID} {
		children, err := store.Children(parent, beads.IncludeClosed, beads.WithBothTiers)
		if err != nil {
			t.Fatalf("Children(%s): %v", parent, err)
		}
		for _, c := range children {
			perParent = append(perParent, c.ID+"<"+c.ParentID+":"+c.Status)
		}
	}
	batched, err := store.ChildrenOfAny([]string{p1.ID, p2.ID})
	if err != nil {
		t.Fatalf("ChildrenOfAny: %v", err)
	}
	var got []string
	for _, c := range batched {
		got = append(got, c.ID+"<"+c.ParentID+":"+c.Status)
	}
	slices.Sort(perParent)
	slices.Sort(got)
	if !slices.Equal(got, perParent) {
		t.Fatalf("ChildrenOfAny = %v, want the per-parent Children union %v", got, perParent)
	}
	if len(got) != 3 {
		t.Fatalf("children = %v, want the 3 non-ephemeral children", got)
	}

	// At both tiers the batch must answer what a List over each ParentID at
	// TierBoth answers (bd list --parent unioned with the wisp query), which is
	// how the bead-policy layer reads children: the ephemeral child included.
	var bothTiers []string
	for _, parent := range []string{p1.ID, p2.ID} {
		children, err := store.List(beads.ListQuery{ParentID: parent, IncludeClosed: true, TierMode: beads.TierBoth})
		if err != nil {
			t.Fatalf("List(%s, TierBoth): %v", parent, err)
		}
		for _, c := range children {
			bothTiers = append(bothTiers, c.ID+"<"+c.ParentID+":"+c.Status)
		}
	}
	batchedBoth, err := store.ChildrenOfAny([]string{p1.ID, p2.ID}, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("ChildrenOfAny(WithBothTiers): %v", err)
	}
	var gotBoth []string
	for _, c := range batchedBoth {
		gotBoth = append(gotBoth, c.ID+"<"+c.ParentID+":"+c.Status)
	}
	slices.Sort(bothTiers)
	slices.Sort(gotBoth)
	if !slices.Equal(gotBoth, bothTiers) || len(gotBoth) != 4 {
		t.Fatalf("ChildrenOfAny(WithBothTiers) = %v, want the TierBoth List union %v (4 children, the ephemeral one included)", gotBoth, bothTiers)
	}

	// DepListBatch answers each anchor as DepList does, for one id (whose
	// `bd dep list <id> --json` prints rows, not records) and for several.
	blocked := mk(beads.Bead{Title: "blocked", Type: "task", ParentID: p1.ID})
	if err := store.DepAdd(blocked.ID, p2.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	single, err := store.DepList(blocked.ID, "down")
	if err != nil || len(single) != 2 {
		t.Fatalf("DepList(%s) = (%v, %v), want its parent-child and blocks edges", blocked.ID, single, err)
	}
	depKeys := func(deps []beads.Dep) []string {
		var out []string
		for _, d := range deps {
			out = append(out, d.IssueID+">"+d.DependsOnID+":"+d.Type)
		}
		slices.Sort(out)
		return out
	}
	for _, ids := range [][]string{{blocked.ID}, {blocked.ID, p1.ID}} {
		batch, err := store.DepListBatch(ids)
		if err != nil {
			t.Fatalf("DepListBatch(%v): %v", ids, err)
		}
		if got, want := depKeys(batch[blocked.ID]), depKeys(single); !slices.Equal(got, want) {
			t.Fatalf("DepListBatch(%v)[%s] = %v, want DepList's %v (whole answer %v)", ids, blocked.ID, got, want, batch)
		}
	}

	leaf := mk(beads.Bead{Title: "leaf control", Type: "task", Metadata: map[string]string{"keep": "v"}})
	status := "closed"
	row, err := store.UpdateReporting(leaf.ID, beads.UpdateOpts{Status: &status, Metadata: map[string]string{"gc.outcome": "pass"}})
	if err != nil {
		t.Fatalf("UpdateReporting: %v", err)
	}
	after, err := store.Get(leaf.ID)
	if err != nil {
		t.Fatalf("Get after update: %v", err)
	}
	if row.Status != "closed" || row.Status != after.Status || row.Metadata["gc.outcome"] != "pass" || row.Metadata["keep"] != "v" || after.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("UpdateReporting row = %+v, Get = status %q metadata %v; want both closed with gc.outcome=pass", row, after.Status, after.Metadata)
	}
}
