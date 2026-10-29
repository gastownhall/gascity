package molecule

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// blockValidatingStore wraps a beads.Store and rejects CloseAll when the
// target has any open "blocks"-type blocker still in the store. This models a
// strict Store implementation so CloseSubtree's ordering contract stays covered
// even when the concrete store permits force-closing blocked beads.
type blockValidatingStore struct {
	beads.Store
}

func (b *blockValidatingStore) Close(id string) error {
	if err := b.assertNoOpenBlockers(id); err != nil {
		return err
	}
	return b.Store.Close(id)
}

func (b *blockValidatingStore) CloseAll(ids []string, _ map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		bead, err := b.Get(id)
		if err != nil {
			return closed, err
		}
		if bead.Status == "closed" {
			continue
		}
		if err := b.assertNoOpenBlockers(id); err != nil {
			return closed, err
		}
		if err := b.Store.Close(id); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

func (b *blockValidatingStore) assertNoOpenBlockers(id string) error {
	deps, err := b.DepList(id, "down")
	if err != nil {
		return err
	}
	for _, d := range deps {
		if d.IssueID != id || d.Type != "blocks" {
			continue
		}
		blocker, err := b.Get(d.DependsOnID)
		if err != nil {
			continue
		}
		if blocker.Status != "closed" {
			return fmt.Errorf("cannot close %s: blocked by open %s", id, d.DependsOnID)
		}
	}
	return nil
}

func TestCloseSubtreeClosesOpenDescendantThroughClosedParent(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "child", ParentID: root.ID})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	grandchild, err := store.Create(beads.Bead{Title: "grandchild", ParentID: child.ID})
	if err != nil {
		t.Fatalf("create grandchild: %v", err)
	}
	if err := store.Close(child.ID); err != nil {
		t.Fatalf("close child: %v", err)
	}

	closed, err := CloseSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("CloseSubtree: %v", err)
	}
	if closed != 2 {
		t.Fatalf("CloseSubtree closed %d beads, want 2", closed)
	}

	for _, id := range []string{root.ID, child.ID, grandchild.ID} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, b.Status)
		}
	}
}

func TestCloseSubtreeClosesLogicalRootMembersAndTheirChildren(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	detached, err := store.Create(beads.Bead{
		Title: "detached control",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("create detached: %v", err)
	}
	logicalChild, err := store.Create(beads.Bead{
		Title:    "logical child",
		ParentID: detached.ID,
	})
	if err != nil {
		t.Fatalf("create logical child: %v", err)
	}

	closed, err := CloseSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("CloseSubtree: %v", err)
	}
	if closed != 3 {
		t.Fatalf("CloseSubtree closed %d beads, want 3", closed)
	}

	for _, id := range []string{root.ID, detached.ID, logicalChild.ID} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, b.Status)
		}
	}
}

func TestCloseSubtreeClosesLogicalMembersAcrossStorageTiers(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	detached, err := store.Create(beads.Bead{
		Title:     "ephemeral detached control",
		Ephemeral: true,
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("create detached: %v", err)
	}
	ephemeralChild, err := store.Create(beads.Bead{
		Title:     "ephemeral child",
		ParentID:  detached.ID,
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("create ephemeral child: %v", err)
	}
	noHistoryChild, err := store.Create(beads.Bead{
		Title:     "no-history child",
		ParentID:  ephemeralChild.ID,
		NoHistory: true,
	})
	if err != nil {
		t.Fatalf("create no-history child: %v", err)
	}

	closed, err := CloseSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("CloseSubtree: %v", err)
	}
	if closed != 4 {
		t.Fatalf("CloseSubtree closed %d beads, want 4", closed)
	}

	for _, id := range []string{root.ID, detached.ID, ephemeralChild.ID, noHistoryChild.ID} {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, b.Status)
		}
	}
}

// TestCloseSubtreeOrdersBlockersBeforeBlocked models the typical
// formula step subtree: a molecule root with N child step beads chained
// by depends_on. CloseSubtree must emit closes in topological
// (blockers-first) order so strict stores can close the whole subtree in
// one pass without rejecting a bead whose in-batch blocker is still open.
//
// To make this test exercise the topological-vs-id-order distinction
// regardless of how MemStore assigns IDs, the chain is built so the
// blocker-first execution order is the *reverse* of the natural
// depth-then-id ordering CloseSubtree currently uses.
func TestCloseSubtreeOrdersBlockersBeforeBlocked(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	// Create steps in *reverse* of execution order: the last-to-run
	// (submit-and-exit) gets the smallest child ID, the first-to-run
	// (load-context) gets the largest. The depth-only sort visits
	// children ID-ascending, so it sees blocked beads before their
	// blockers.
	stepNames := []string{
		"submit-and-exit",
		"self-review",
		"implement",
		"preflight-tests",
		"workspace-setup",
		"load-context",
	}
	steps := make([]beads.Bead, 0, len(stepNames))
	for _, name := range stepNames {
		s, err := store.Create(beads.Bead{Title: name, ParentID: root.ID})
		if err != nil {
			t.Fatalf("create step %q: %v", name, err)
		}
		steps = append(steps, s)
	}
	// Each earlier-to-execute step is the blocker of the next-to-execute
	// step (the classic depends_on chain a formula emits): load-context
	// blocks workspace-setup blocks preflight-tests blocks implement
	// blocks self-review blocks submit-and-exit.
	for i := 0; i < len(steps)-1; i++ {
		blocked := steps[i]   // later in execution
		blocker := steps[i+1] // earlier in execution
		if err := store.DepAdd(blocked.ID, blocker.ID, "blocks"); err != nil {
			t.Fatalf("DepAdd(%s blocks-on %s): %v", blocked.ID, blocker.ID, err)
		}
	}

	guarded := &blockValidatingStore{Store: store}
	closed, err := CloseSubtree(guarded, root.ID)
	if err != nil {
		t.Fatalf("CloseSubtree: %v", err)
	}
	wantClosed := 1 + len(steps)
	if closed != wantClosed {
		t.Fatalf("CloseSubtree closed %d beads, want %d", closed, wantClosed)
	}

	for _, id := range append([]string{root.ID}, idsOf(steps)...) {
		b, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if b.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, b.Status)
		}
	}
}

type depListFailingStore struct {
	beads.Store
	failID string
}

func (s *depListFailingStore) DepList(id, direction string) ([]beads.Dep, error) {
	if id == s.failID {
		return nil, fmt.Errorf("dependency list unavailable for %s", id)
	}
	return s.Store.DepList(id, direction)
}

func TestCloseSubtreeReturnsDepListError(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "child", ParentID: root.ID})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	guarded := &depListFailingStore{Store: store, failID: child.ID}
	if _, err := CloseSubtree(guarded, root.ID); err == nil {
		t.Fatalf("CloseSubtree succeeded, want dependency list error")
	}
}

func idsOf(bs []beads.Bead) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}

func TestCloseSubtreeHandlesParentCycles(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "child", ParentID: root.ID})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	if err := store.Update(root.ID, beads.UpdateOpts{ParentID: &child.ID}); err != nil {
		t.Fatalf("Update(root.ParentID): %v", err)
	}

	closed, err := CloseSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("CloseSubtree: %v", err)
	}
	if closed != 2 {
		t.Fatalf("CloseSubtree closed %d beads, want 2", closed)
	}
	for _, id := range []string{root.ID, child.ID} {
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if bead.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, bead.Status)
		}
	}
}

// TestCloseSubtree_StampsCloseReason verifies that CloseSubtree stamps
// close_reason=SubtreeClosedReason on every bead in the closed subtree.
// Without this, bd's validation.on-close=error rejects the close, the
// subtree stays open, and sling.CloseAttachedSubtree (which delegates
// here for non-workflow molecule attachments) silently leaves
// descendants alive.
func TestCloseSubtree_StampsCloseReason(t *testing.T) {
	if got := len(SubtreeClosedReason); got < 20 {
		t.Fatalf("SubtreeClosedReason = %q (%d chars), want >=20", SubtreeClosedReason, got)
	}

	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "child", ParentID: root.ID})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}

	closed, err := CloseSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("CloseSubtree: %v", err)
	}
	if closed != 2 {
		t.Fatalf("CloseSubtree closed %d beads, want 2", closed)
	}

	for _, id := range []string{root.ID, child.ID} {
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
		if bead.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", id, bead.Status)
		}
		if got := bead.Metadata["close_reason"]; got != SubtreeClosedReason {
			t.Errorf("%s close_reason = %q, want %q", id, got, SubtreeClosedReason)
		}
	}
}

// batchedChildrenStore answers ChildrenOfAny in one call per frontier, with
// the union deliberately reversed so ListSubtree's own ordering is what puts
// the members in walk order.
type batchedChildrenStore struct {
	beads.Store
	batches  int
	children int
}

func (s *batchedChildrenStore) ChildrenOfAny(parentIDs []string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.batches++
	var out []beads.Bead
	seen := map[string]bool{}
	for _, id := range parentIDs {
		kids, err := s.Store.Children(id, append([]beads.QueryOpt{beads.IncludeClosed}, opts...)...)
		if err != nil {
			return nil, err
		}
		for _, k := range kids {
			if !seen[k.ID] {
				seen[k.ID] = true
				out = append(out, k)
			}
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

func (s *batchedChildrenStore) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	s.children++
	return s.Store.Children(parentID, opts...)
}

// TestListSubtreeBatchedWalkMatchesPerParentWalk pins that reading children a
// frontier at a time returns the same members in the same order as reading
// them a parent at a time, over closed intermediates, parent-only descendants
// (no gc.root_bead_id) and several levels, and that it reads once per level.
func TestListSubtreeBatchedWalkMatchesPerParentWalk(t *testing.T) {
	store := beads.NewMemStore()
	mk := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("create %s: %v", b.Title, err)
		}
		return created
	}
	root := mk(beads.Bead{Title: "root", Type: "molecule"})
	member := func(title string) beads.Bead {
		return mk(beads.Bead{Title: title, Metadata: map[string]string{"gc.root_bead_id": root.ID}})
	}
	m1 := member("m1")
	m2 := member("m2")
	c1 := mk(beads.Bead{Title: "c1", ParentID: m1.ID})
	mk(beads.Bead{Title: "g1", ParentID: c1.ID})
	mk(beads.Bead{Title: "c2", ParentID: m1.ID})
	p1 := mk(beads.Bead{Title: "p1", ParentID: root.ID})
	mk(beads.Bead{Title: "p2", ParentID: p1.ID})
	mk(beads.Bead{Title: "m2child", ParentID: m2.ID})
	for _, id := range []string{m1.ID, c1.ID} {
		if err := store.Close(id); err != nil {
			t.Fatalf("close %s: %v", id, err)
		}
	}

	perParent, err := ListSubtree(store, root.ID)
	if err != nil {
		t.Fatalf("ListSubtree (per parent): %v", err)
	}
	batched := &batchedChildrenStore{Store: store}
	got, err := ListSubtree(batched, root.ID)
	if err != nil {
		t.Fatalf("ListSubtree (batched): %v", err)
	}
	if want := idsOf(perParent); fmt.Sprint(idsOf(got)) != fmt.Sprint(want) {
		t.Fatalf("batched walk = %v, want the per-parent walk %v", idsOf(got), want)
	}
	if len(got) != 9 {
		t.Fatalf("walk found %d members, want 9 (root, 2 members, 6 descendants)", len(got))
	}
	if batched.children != 0 {
		t.Fatalf("batched walk made %d per-parent Children reads, want 0", batched.children)
	}
	// Frontiers {root, m1, m2}, {c1, c2, p1, m2child} and {g1, p2}: one read
	// each, where the per-parent walk reads once per member (9).
	if batched.batches != 3 {
		t.Fatalf("batched walk made %d batched reads, want one per frontier (3)", batched.batches)
	}
}
