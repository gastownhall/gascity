package beads

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// TestCachingStoreReadyDefersToAPassedStep checks the cached ready verdict
// against both shapes of a closed blocker that recorded
// gc.work_outcome=blocked: alone it withholds its dependent, but when the
// blocker's control-plane step passed (gc.outcome=pass) the dependent is ready,
// matching the backing stores.
func TestCachingStoreReadyDefersToAPassedStep(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		want     bool
	}{
		{
			"blocked work outcome withholds the dependent",
			map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked},
			false,
		},
		{
			"passed step releases the dependent despite a blocked work outcome",
			map[string]string{beadmeta.StepRefMetadataKey: "review", beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked},
			true,
		},
		{
			"passed work bead without a step ref still withholds the dependent",
			map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked},
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := NewCachingStoreForTest(NewMemStore(), nil)
			if err := cs.Prime(context.Background()); err != nil {
				t.Fatal(err)
			}
			blocker, err := cs.Create(Bead{Title: "plan review", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			dependent, err := cs.Create(Bead{Title: "decompose", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			if err := cs.DepAdd(dependent.ID, blocker.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			if err := cs.SetMetadataBatch(blocker.ID, tc.metadata); err != nil {
				t.Fatal(err)
			}
			if err := cs.Close(blocker.ID); err != nil {
				t.Fatal(err)
			}
			got, err := cs.Ready()
			if err != nil {
				t.Fatal(err)
			}
			ready := false
			for _, b := range got {
				if b.ID == dependent.ID {
					ready = true
				}
			}
			if ready != tc.want {
				t.Fatalf("dependent ready = %v, want %v (metadata %v)", ready, tc.want, tc.metadata)
			}
		})
	}
}
