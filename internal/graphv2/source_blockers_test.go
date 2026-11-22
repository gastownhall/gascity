package graphv2

import (
	"slices"
	"testing"

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
