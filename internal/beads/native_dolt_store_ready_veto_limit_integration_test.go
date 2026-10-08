//go:build integration

package beads

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// TestNativeDoltStoreReadySurvivesMoreThanFiftyDistinctBlockers pins, against
// real upstream storage, that every blocker's outcome reaches the ready veto.
// filterReadyByWorkOutcomeListBlockers builds its List+IDFilter request from
// nativeListReadRequest(), which leaves Limit nil, and beads 1.3.1 answers a
// nil Limit as its own shared list default — workapi.DefaultListLimit, fifty
// rows — not unlimited. Left at nil, a ready frontier with more than fifty
// DISTINCT blockers would see only the first page's worth of blocker outcomes,
// so a blocker past that page could never veto its candidate's readiness: with
// 60 blockers, each closed with work_outcome=blocked and each gating exactly
// one otherwise-ready candidate, 10 candidates would be wrongly readied.
//
// The fast unit-level regression
// (TestNativeDoltStoreReadyWorkOutcomeListBlockersSetsAnExplicitUnlimitedLimit,
// in native_dolt_store_ready_outcome_fanout_test.go) proves the same thing in
// milliseconds against a fake; this test proves it against the REAL
// beads-1.3.1 embedded Dolt storage, including its real default-page
// behavior, which the fake only stands in for.
func TestNativeDoltStoreReadySurvivesMoreThanFiftyDistinctBlockers(t *testing.T) {
	store := openRealNativeDoltStoreForFacade(t, "veto-limit")
	const blockerCount = 60
	for i := 0; i < blockerCount; i++ {
		blocker, err := store.Create(Bead{Title: fmt.Sprintf("blocker %d", i)})
		if err != nil {
			t.Fatalf("Create blocker %d: %v", i, err)
		}
		if err := store.SetMetadata(blocker.ID, beadmeta.WorkOutcomeMetadataKey, beadmeta.WorkOutcomeBlocked); err != nil {
			t.Fatalf("SetMetadata blocker %d: %v", i, err)
		}
		if err := store.Close(blocker.ID); err != nil {
			t.Fatalf("Close blocker %d: %v", i, err)
		}
		if _, err := store.Create(Bead{
			Title:        fmt.Sprintf("candidate %d", i),
			Dependencies: []Dep{{DependsOnID: blocker.ID, Type: "blocks"}},
		}); err != nil {
			t.Fatalf("Create candidate %d: %v", i, err)
		}
	}
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(ready) != 0 {
		ids := make([]string, 0, len(ready))
		for _, b := range ready {
			ids = append(ids, b.ID)
		}
		t.Fatalf("Ready returned %d candidates whose only blocker closed with work_outcome=blocked (%v); want 0 (the veto must survive beyond a 50-row default page over %d distinct blockers)", len(ready), ids, blockerCount)
	}
}
