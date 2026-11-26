package graphv2

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/molecule"
)

func createSourceBlockersBead(t *testing.T, store beads.Store, title string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: title, Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("creating %s: %v", title, err)
	}
	return b
}

// blocksOn returns ids as blocks-typed blockers, sorted by id.
func blocksOn(ids ...string) []molecule.Blocker {
	out := make([]molecule.Blocker, 0, len(ids))
	for _, id := range ids {
		out = append(out, molecule.Blocker{ID: id, Type: "blocks"})
	}
	return sortedBlockers(out)
}

// TestSourceBlockersReadsReadyBlockingDeps pins which of a source bead's
// dependencies a launched workflow inherits: every ready-blocking edge, once
// each, with its own type, sorted, and nothing else.
func TestSourceBlockersReadsReadyBlockingDeps(t *testing.T) {
	store := beads.NewMemStore()
	source := createSourceBlockersBead(t, store, "source")
	want := make([]molecule.Blocker, 0, 3)
	for _, depType := range []string{"waits-for", "blocks", "conditional-blocks"} {
		target := createSourceBlockersBead(t, store, depType)
		if err := store.DepAdd(source.ID, target.ID, depType); err != nil {
			t.Fatalf("DepAdd %s: %v", depType, err)
		}
		want = append(want, molecule.Blocker{ID: target.ID, Type: depType})
	}
	for _, depType := range []string{"parent-child", "tracks", "related"} {
		target := createSourceBlockersBead(t, store, depType)
		if err := store.DepAdd(source.ID, target.ID, depType); err != nil {
			t.Fatalf("DepAdd %s: %v", depType, err)
		}
	}
	want = sortedBlockers(want)

	got, err := SourceBlockers(beadstest.Work(store), source.ID)
	if err != nil {
		t.Fatalf("SourceBlockers: %v", err)
	}
	if !slices.Equal(got, want) {
		t.Errorf("SourceBlockers = %v, want %v", got, want)
	}
}

// TestSourceBlockersEmptyForUnblockedSource pins that a source bead with no
// ready-blocking dependency yields no blockers.
func TestSourceBlockersEmptyForUnblockedSource(t *testing.T) {
	store := beads.NewMemStore()
	source := createSourceBlockersBead(t, store, "source")
	got, err := SourceBlockers(beadstest.Work(store), source.ID)
	if err != nil {
		t.Fatalf("SourceBlockers: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("SourceBlockers = %v, want none", got)
	}
}

// TestSourceBlockersDropsSatisfiedBlockers pins that a blocker the store's
// readiness rule already judges satisfied is not inherited, while one closed
// with gc.work_outcome=blocked, an open one, an external ref and one the store
// does not hold still are.
func TestSourceBlockersDropsSatisfiedBlockers(t *testing.T) {
	store := beads.NewMemStore()
	source := createSourceBlockersBead(t, store, "source")
	closed := createSourceBlockersBead(t, store, "closed")
	closedBlocked := createSourceBlockersBead(t, store, "closed blocked")
	open := createSourceBlockersBead(t, store, "open")
	if err := store.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(closedBlocked.ID, beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkOutcomeBlocked); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(closedBlocked.ID); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{closed.ID, closedBlocked.ID, open.ID, "external:other:gc-99", "gc-404"} {
		if err := store.DepAdd(source.ID, target, "blocks"); err != nil {
			t.Fatalf("DepAdd %s: %v", target, err)
		}
	}

	got, err := SourceBlockers(beadstest.Work(store), source.ID)
	if err != nil {
		t.Fatalf("SourceBlockers: %v", err)
	}
	want := blocksOn(closedBlocked.ID, open.ID, "external:other:gc-99", "gc-404")
	if !slices.Equal(got, want) {
		t.Errorf("SourceBlockers = %v, want %v", got, want)
	}
}

// TestSourceBlockersSkipsOwnWorkflowRoots pins that a source bead's own
// workflow roots, linked by gc.source_bead_id or by a synthetic input convoy
// tracking the source, are never inherited: a replacement workflow must not
// wait on the one it replaces. Another bead's workflow root still holds.
func TestSourceBlockersSkipsOwnWorkflowRoots(t *testing.T) {
	store := beads.NewMemStore()
	work := beadstest.Work(store)
	source := createSourceBlockersBead(t, store, "source")
	other := createSourceBlockersBead(t, store, "other")
	inputConvoy, err := CreateSingleItemInputConvoy(work, source)
	if err != nil {
		t.Fatalf("CreateSingleItemInputConvoy: %v", err)
	}
	otherConvoy, err := CreateSingleItemInputConvoy(work, other)
	if err != nil {
		t.Fatalf("CreateSingleItemInputConvoy(other): %v", err)
	}
	newRoot := func(title string, meta map[string]string) beads.Bead {
		t.Helper()
		meta[beadmeta.KindMetadataKey] = beadmeta.KindWorkflow
		root, err := store.Create(beads.Bead{Title: title, Type: "task", Status: "open", Metadata: meta})
		if err != nil {
			t.Fatal(err)
		}
		return root
	}
	bySource := newRoot("own by source", map[string]string{beadmeta.SourceBeadIDMetadataKey: source.ID})
	byConvoy := newRoot("own by convoy", map[string]string{beadmeta.InputConvoyIDMetadataKey: inputConvoy.ID})
	foreign := newRoot("other's root", map[string]string{beadmeta.InputConvoyIDMetadataKey: otherConvoy.ID})
	for _, target := range []string{bySource.ID, byConvoy.ID, foreign.ID} {
		if err := store.DepAdd(source.ID, target, "blocks"); err != nil {
			t.Fatalf("DepAdd %s: %v", target, err)
		}
	}

	got, err := SourceBlockers(work, source.ID)
	if err != nil {
		t.Fatalf("SourceBlockers: %v", err)
	}
	if want := blocksOn(foreign.ID); !slices.Equal(got, want) {
		t.Errorf("SourceBlockers = %v, want %v", got, want)
	}
}

// TestSourceBlockersIgnoresNonBlockingDependencyTypes pins that tracks,
// parent-child and relates-to edges never hold a launched workflow.
func TestSourceBlockersIgnoresNonBlockingDependencyTypes(t *testing.T) {
	store := beads.NewMemStore()
	tracks := createSourceBlockersBead(t, store, "tracks-target")
	parent := createSourceBlockersBead(t, store, "parent")
	related := createSourceBlockersBead(t, store, "related")
	source := createSourceBlockersBead(t, store, "work")
	for _, dep := range []struct{ target, typ string }{
		{tracks.ID, "tracks"},
		{parent.ID, "parent-child"},
		{related.ID, "relates-to"},
	} {
		if err := store.DepAdd(source.ID, dep.target, dep.typ); err != nil {
			t.Fatal(err)
		}
	}

	got, err := SourceBlockers(beadstest.Work(store), source.ID)
	if err != nil {
		t.Fatalf("SourceBlockers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("SourceBlockers = %v, want none: only non-blocking dep types point at the source", got)
	}
}

// TestSourceBlockersHonorsWaitsForAndConditionalBlocks pins that the
// non-blocks ready-blocking types hold a launched workflow too.
func TestSourceBlockersHonorsWaitsForAndConditionalBlocks(t *testing.T) {
	store := beads.NewMemStore()
	waiter := createSourceBlockersBead(t, store, "waiter")
	conditional := createSourceBlockersBead(t, store, "conditional")
	source := createSourceBlockersBead(t, store, "work")
	if err := store.DepAdd(source.ID, waiter.ID, "waits-for"); err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(source.ID, conditional.ID, "conditional-blocks"); err != nil {
		t.Fatal(err)
	}

	got, err := SourceBlockers(beadstest.Work(store), source.ID)
	if err != nil {
		t.Fatalf("SourceBlockers: %v", err)
	}
	want := sortedBlockers([]molecule.Blocker{{ID: waiter.ID, Type: "waits-for"}, {ID: conditional.ID, Type: "conditional-blocks"}})
	if !slices.Equal(got, want) {
		t.Fatalf("SourceBlockers = %v, want sorted %v", got, want)
	}
}

// TestSourceBlockersKeepsWaitsForWhateverItsSpawnersStatus pins that a
// waits-for edge is inherited whatever its spawner's own status: bd and native
// Dolt judge it by the spawner's open children, so a closed spawner whose
// child is still open holds the source bead, and only an edge the store judges
// can hold the workflow by that rule. A closed blocks target, which every
// store judges by its status, is dropped.
func TestSourceBlockersKeepsWaitsForWhateverItsSpawnersStatus(t *testing.T) {
	store := beads.NewMemStore()
	source := createSourceBlockersBead(t, store, "work")
	closedSpawner := createSourceBlockersBead(t, store, "closed spawner")
	openChild := createSourceBlockersBead(t, store, "open child")
	openSpawner := createSourceBlockersBead(t, store, "open spawner")
	closedChild := createSourceBlockersBead(t, store, "closed child")
	closedBlocker := createSourceBlockersBead(t, store, "closed blocker")
	for _, edge := range []beads.Dep{
		{IssueID: openChild.ID, DependsOnID: closedSpawner.ID, Type: "parent-child"},
		{IssueID: closedChild.ID, DependsOnID: openSpawner.ID, Type: "parent-child"},
		{IssueID: source.ID, DependsOnID: closedSpawner.ID, Type: "waits-for"},
		{IssueID: source.ID, DependsOnID: openSpawner.ID, Type: "waits-for"},
		{IssueID: source.ID, DependsOnID: closedBlocker.ID, Type: "blocks"},
	} {
		if err := store.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{closedSpawner.ID, closedChild.ID, closedBlocker.ID} {
		if err := store.Close(id); err != nil {
			t.Fatal(err)
		}
	}

	got, err := SourceBlockers(beadstest.Work(store), source.ID)
	if err != nil {
		t.Fatalf("SourceBlockers: %v", err)
	}
	want := sortedBlockers([]molecule.Blocker{{ID: closedSpawner.ID, Type: "waits-for"}, {ID: openSpawner.ID, Type: "waits-for"}})
	if !slices.Equal(got, want) {
		t.Fatalf("SourceBlockers = %v, want %v: a waits-for edge is the store's to judge, a closed blocks target is satisfied", got, want)
	}
}

// TestWorkflowBlockersReadsWhatTheLaunchWired pins that a reused workflow
// reports the blockers its launch wired onto its root and still waits on: a
// blocks target that has closed since is dropped, a waits-for edge is kept for
// the store to judge, and the root's other edges, including one onto its own
// step, are not blockers.
func TestWorkflowBlockersReadsWhatTheLaunchWired(t *testing.T) {
	store := beads.NewMemStore()
	root := createSourceBlockersBead(t, store, "root")
	open := createSourceBlockersBead(t, store, "open blocker")
	closed := createSourceBlockersBead(t, store, "closed blocker")
	spawner := createSourceBlockersBead(t, store, "spawner")
	convoy := createSourceBlockersBead(t, store, "convoy")
	step, err := store.Create(beads.Bead{Title: "own step", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, edge := range []beads.Dep{
		{IssueID: root.ID, DependsOnID: open.ID, Type: "blocks"},
		{IssueID: root.ID, DependsOnID: closed.ID, Type: "blocks"},
		{IssueID: root.ID, DependsOnID: spawner.ID, Type: "waits-for"},
		{IssueID: root.ID, DependsOnID: convoy.ID, Type: "tracks"},
		{IssueID: root.ID, DependsOnID: step.ID, Type: "blocks"},
	} {
		if err := store.DepAdd(edge.IssueID, edge.DependsOnID, edge.Type); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{closed.ID, spawner.ID} {
		if err := store.Close(id); err != nil {
			t.Fatal(err)
		}
	}

	got, err := WorkflowBlockers(store, root.ID)
	if err != nil {
		t.Fatalf("WorkflowBlockers: %v", err)
	}
	want := sortedBlockers([]molecule.Blocker{{ID: open.ID, Type: "blocks"}, {ID: spawner.ID, Type: "waits-for"}})
	if !slices.Equal(got, want) {
		t.Fatalf("WorkflowBlockers = %v, want %v", got, want)
	}
}

// TestSourceBlockersKeepsBlockerClosedWithBlockedOutcome pins that a blocker
// closed with gc.work_outcome=blocked is not satisfied and keeps holding.
func TestSourceBlockersKeepsBlockerClosedWithBlockedOutcome(t *testing.T) {
	store := beads.NewMemStore()
	blocker := createSourceBlockersBead(t, store, "blocker")
	source := createSourceBlockersBead(t, store, "work")
	if err := store.DepAdd(source.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadata(blocker.ID, beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkOutcomeBlocked); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(blocker.ID); err != nil {
		t.Fatal(err)
	}

	got, err := SourceBlockers(beadstest.Work(store), source.ID)
	if err != nil {
		t.Fatalf("SourceBlockers: %v", err)
	}
	if !slices.Equal(got, blocksOn(blocker.ID)) {
		t.Fatalf("SourceBlockers = %v, want [%s]: a blocker closed with gc.work_outcome=blocked is not satisfied", got, blocker.ID)
	}
}

// TestSourceBlockersKeepsForeignWorkflowRoot pins that a workflow root hung
// off a real (non-synthetic) convoy that merely tracks the source is another
// launch's workflow, not the source's own, and keeps holding.
func TestSourceBlockersKeepsForeignWorkflowRoot(t *testing.T) {
	store := beads.NewMemStore()
	source := createSourceBlockersBead(t, store, "work")
	convoy, err := store.Create(beads.Bead{Title: "real convoy", Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(convoy.ID, source.ID, "tracks"); err != nil {
		t.Fatal(err)
	}
	foreign, err := store.Create(beads.Bead{Title: "workflow", Type: "task", Status: "in_progress", Metadata: map[string]string{
		beadmeta.KindMetadataKey:          beadmeta.KindWorkflow,
		"gc.formula_contract":             "graph.v2",
		beadmeta.InputConvoyIDMetadataKey: convoy.ID,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(source.ID, foreign.ID, "blocks"); err != nil {
		t.Fatal(err)
	}

	got, err := SourceBlockers(beadstest.Work(store), source.ID)
	if err != nil {
		t.Fatalf("SourceBlockers: %v", err)
	}
	if !slices.Equal(got, blocksOn(foreign.ID)) {
		t.Fatalf("SourceBlockers = %v, want [%s]: a foreign workflow root keeps holding", got, foreign.ID)
	}
}

// TestGraphHeldBlockersNormalizesAndRefusesUnheld pins the split-city
// holdability check: an external:<id> ref names <id>, an external capability
// is dropped, a held blocker is wired with its edge type, and an unheld one
// refuses the launch with an error naming the source bead and the blocker.
func TestGraphHeldBlockersNormalizesAndRefusesUnheld(t *testing.T) {
	graph := beads.NewMemStore()
	held := createSourceBlockersBead(t, graph, "held")
	other := createSourceBlockersBead(t, graph, "other")

	got, err := GraphHeldBlockers(graph, "src-1", []molecule.Blocker{
		{ID: "external:" + held.ID, Type: "blocks"},
		{ID: other.ID, Type: "waits-for"},
		{ID: "external:rig:capability", Type: "blocks"},
	})
	if err != nil {
		t.Fatalf("GraphHeldBlockers: %v", err)
	}
	want := sortedBlockers([]molecule.Blocker{{ID: held.ID, Type: "blocks"}, {ID: other.ID, Type: "waits-for"}})
	if !slices.Equal(got, want) {
		t.Errorf("GraphHeldBlockers = %v, want %v", got, want)
	}

	_, err = GraphHeldBlockers(graph, "src-1", blocksOn(held.ID, "wk-7"))
	var crossErr *CrossStoreBlockersError
	if !errors.As(err, &crossErr) {
		t.Fatalf("GraphHeldBlockers error = %v, want a *CrossStoreBlockersError", err)
	}
	if crossErr.SourceID != "src-1" || !slices.Equal(crossErr.BlockerIDs, []string{"wk-7"}) {
		t.Errorf("CrossStoreBlockersError = %+v, want source src-1 blockers [wk-7]", crossErr)
	}
	for _, part := range []string{"src-1", "wk-7", "bd dep remove src-1", "re-sling"} {
		if !strings.Contains(err.Error(), part) {
			t.Errorf("error %q does not contain %q", err, part)
		}
	}
}
