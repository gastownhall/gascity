package dispatch

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestLoadDownDepsForScopeSkipReadsEachAnchorWhenBdRefusesTheBatch pins the
// scope-skip dependency read over a cached bd store whose backend refuses the
// batched dep read (the bd/Postgres backend answers it with `operation
// "IssueRelations" not supported`, ga-7i7ts) but answers a one-id read. The
// refusal must reach this caller as ErrDepListBatchUnsupported so it reads each
// anchor, not as an empty map that reports every anchor edge-free.
func TestLoadDownDepsForScopeSkipReadsEachAnchorWhenBdRefusesTheBatch(t *testing.T) {
	t.Parallel()

	fake := beadstest.NewFakeBd()
	blocker, err := fake.Seed(beads.Bead{Title: "blocker"})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := fake.Seed(beads.Bead{Title: "first"})
	second, _ := fake.Seed(beads.Bead{Title: "second"})
	for _, id := range []string{first.ID, second.ID} {
		if err := fake.SeedDep(id, blocker.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	var refused int
	runner := func(dir, name string, args ...string) ([]byte, error) {
		if args[0] == "dep" && args[1] == "list" && !slices.Contains(args, "--direction=up") {
			ids := 0
			for _, arg := range args[2:] {
				if !strings.HasPrefix(arg, "-") {
					ids++
				}
			}
			if ids > 1 {
				refused++
				return nil, errors.New(`exit status 1: Error: operation "IssueRelations" not supported by the postgres backend`)
			}
		}
		return fake.Run(dir, name, args...)
	}
	store := beads.NewCachingStoreForTest(beads.NewBdStore("/fake-bd", runner), nil)

	deps, err := loadDownDepsForScopeSkip(store, []string{first.ID, second.ID})
	if err != nil {
		t.Fatalf("loadDownDepsForScopeSkip: %v", err)
	}
	if refused == 0 {
		t.Fatal("the batch was never asked for, so this test does not exercise the refusal")
	}
	for _, id := range []string{first.ID, second.ID} {
		if got := deps[id]; len(got) != 1 || got[0].DependsOnID != blocker.ID {
			t.Errorf("deps[%s] = %v, want the blocks edge to %s; the refused batch was read as no edges", id, got, blocker.ID)
		}
	}
}
