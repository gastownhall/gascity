package graphv2

import (
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

func createSourceBlockersBead(t *testing.T, store beads.Store, title string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{Title: title, Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("creating %s: %v", title, err)
	}
	return b
}

// TestSourceBlockersReadsReadyBlockingDeps pins which of a source bead's
// dependencies a launched workflow inherits: every ready-blocking type, once
// each, sorted, and nothing else.
func TestSourceBlockersReadsReadyBlockingDeps(t *testing.T) {
	store := beads.NewMemStore()
	source := createSourceBlockersBead(t, store, "source")
	want := make([]string, 0, 3)
	for _, depType := range []string{"waits-for", "blocks", "conditional-blocks"} {
		target := createSourceBlockersBead(t, store, depType)
		if err := store.DepAdd(source.ID, target.ID, depType); err != nil {
			t.Fatalf("DepAdd %s: %v", depType, err)
		}
		want = append(want, target.ID)
	}
	for _, depType := range []string{"parent-child", "tracks", "related"} {
		target := createSourceBlockersBead(t, store, depType)
		if err := store.DepAdd(source.ID, target.ID, depType); err != nil {
			t.Fatalf("DepAdd %s: %v", depType, err)
		}
	}
	slices.Sort(want)

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
	want := []string{closedBlocked.ID, open.ID, "external:other:gc-99", "gc-404"}
	slices.Sort(want)
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
	if want := []string{foreign.ID}; !slices.Equal(got, want) {
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
	want := []string{waiter.ID, conditional.ID}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("SourceBlockers = %v, want sorted %v", got, want)
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
	if !slices.Equal(got, []string{blocker.ID}) {
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
	if !slices.Equal(got, []string{foreign.ID}) {
		t.Fatalf("SourceBlockers = %v, want [%s]: a foreign workflow root keeps holding", got, foreign.ID)
	}
}
