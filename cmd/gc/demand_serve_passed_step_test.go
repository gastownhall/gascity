package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestBeadHasUnmetPlainBlocksDepDefersToAPassedStep keeps the drain-ack and
// divergence classifiers in agreement with Ready(): a blocker closed with
// gc.work_outcome=blocked still blocks, unless it is a formula step whose
// control-plane step passed.
func TestBeadHasUnmetPlainBlocksDepDefersToAPassedStep(t *testing.T) {
	for _, tc := range []struct {
		name     string
		metadata map[string]string
		want     bool
	}{
		{"blocked work outcome is unmet", map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked}, true},
		{
			"passed step is met despite a blocked work outcome",
			map[string]string{beadmeta.StepRefMetadataKey: "review", beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked},
			false,
		},
		{
			"passed work bead without a step ref is unmet",
			map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass, beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked},
			true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			blocker, err := store.Create(beads.Bead{Title: "plan review", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			dependent, err := store.Create(beads.Bead{Title: "decompose", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.DepAdd(dependent.ID, blocker.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			if err := store.SetMetadataBatch(blocker.ID, tc.metadata); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(blocker.ID); err != nil {
				t.Fatal(err)
			}
			got, err := beadHasUnmetPlainBlocksDep(store, dependent.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("beadHasUnmetPlainBlocksDep = %v, want %v (metadata %v)", got, tc.want, tc.metadata)
			}
		})
	}
}
