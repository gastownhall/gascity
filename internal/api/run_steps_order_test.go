package api

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func stepBead(id string, needs ...string) beads.Bead {
	return beads.Bead{ID: id, Title: "Step " + id, Status: "open", Type: "task", Needs: needs}
}

func orderedIDs(t *testing.T, members []beads.Bead) []string {
	t.Helper()
	ids := make([]string, len(members))
	for i, m := range members {
		ids[i] = m.ID
	}
	return ids
}

func assertOrder(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("order = %v, want %v (length mismatch)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v (diverges at index %d)", got, want, i)
		}
	}
}

// TestTopoSortRunStepsLinearChain reproduces gascity#4699's own repro: a
// 9-step chain declared in one order, whose fold-order arrival is scrambled
// (mirroring the arbitrary order the issue observed), must come back out in
// declaration order once topologically sorted on the Needs edges alone.
func TestTopoSortRunStepsLinearChain(t *testing.T) {
	// Declared order: prep -> author-en -> translate-ro -> feature-pr ->
	// review -> gate-merge-feature -> archive-materialize -> merge-archive -> land.
	prep := stepBead("prep")
	authorEn := stepBead("author-en", "prep")
	translateRo := stepBead("translate-ro", "author-en")
	featurePr := stepBead("feature-pr", "translate-ro")
	review := stepBead("review", "feature-pr")
	gateMergeFeature := stepBead("gate-merge-feature", "review")
	archiveMaterialize := stepBead("archive-materialize", "gate-merge-feature")
	mergeArchive := stepBead("merge-archive", "archive-materialize")
	land := stepBead("land", "merge-archive")

	// Scrambled arrival order, matching the issue's own observed output.
	scrambled := []beads.Bead{
		mergeArchive, authorEn, translateRo, gateMergeFeature, archiveMaterialize,
		featurePr, land, review, prep,
	}

	got := orderedIDs(t, topoSortRunSteps(scrambled))
	want := []string{
		"prep", "author-en", "translate-ro", "feature-pr", "review",
		"gate-merge-feature", "archive-materialize", "merge-archive", "land",
	}
	assertOrder(t, got, want)
}

// TestTopoSortRunStepsFanOutFanInTieBreaksOnID confirms two independent
// branches (b, c both depending only on a) land in a deterministic order —
// bead ID — rather than an arbitrary one, and that the fan-in step (d) still
// waits for both.
func TestTopoSortRunStepsFanOutFanIn(t *testing.T) {
	a := stepBead("a")
	c := stepBead("c", "a")
	b := stepBead("b", "a")
	d := stepBead("d", "b", "c")

	got := orderedIDs(t, topoSortRunSteps([]beads.Bead{d, c, b, a}))
	assertOrder(t, got, []string{"a", "b", "c", "d"})
}

// TestTopoSortRunStepsNoDependencyDataFallsBackToIDOrder covers steps with
// no Needs/Dependencies at all (e.g. a v1/wisp member never carrying
// step-order metadata) — the sort must still return every member, ID-sorted,
// rather than leaving fold-arrival order in place.
func TestTopoSortRunStepsNoDependencyDataFallsBackToIDOrder(t *testing.T) {
	got := orderedIDs(t, topoSortRunSteps([]beads.Bead{
		stepBead("z"), stepBead("a"), stepBead("m"),
	}))
	assertOrder(t, got, []string{"a", "m", "z"})
}

// TestTopoSortRunStepsIgnoresEdgesOutsideMemberSet proves a Needs entry
// pointing outside the member set (a non-blocking type-prefixed entry like
// "tracks:x", or a bead this run doesn't actually hold) is silently dropped
// rather than crashing or corrupting the order of the beads that ARE members.
func TestTopoSortRunStepsIgnoresEdgesOutsideMemberSet(t *testing.T) {
	got := orderedIDs(t, topoSortRunSteps([]beads.Bead{
		stepBead("second", "first", "tracks:some-other-run.step9", "ghost-id"),
		stepBead("first"),
	}))
	assertOrder(t, got, []string{"first", "second"})
}

// TestTopoSortRunStepsHandlesCycleWithoutHanging is a defensive regression
// guard: a formula-produced graph should never contain a cycle, but a
// malformed one must not hang the endpoint. Cycle members are appended,
// ID-sorted, once the frontier empties.
func TestTopoSortRunStepsHandlesCycleWithoutHanging(t *testing.T) {
	x := stepBead("x", "y")
	y := stepBead("y", "x")
	entry := stepBead("entry")

	done := make(chan []string, 1)
	go func() {
		done <- orderedIDs(t, topoSortRunSteps([]beads.Bead{y, x, entry}))
	}()
	select {
	case got := <-done:
		if len(got) != 3 {
			t.Fatalf("order = %v, want 3 beads", got)
		}
		if got[0] != "entry" {
			t.Fatalf("order = %v, want entry first (only non-cycle member)", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("topoSortRunSteps did not return on a cyclic graph")
	}
}
