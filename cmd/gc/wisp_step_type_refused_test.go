package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// typeRefusingStore is a store whose listing refuses a pushed type its scope
// does not register, in bd's words. Everything else it answers.
type typeRefusingStore struct {
	*beads.MemStore
	refuses string
}

func (s *typeRefusingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Type == s.refuses {
		return nil, fmt.Errorf("bd list both tiers: bd list: bd list: exit status 1: Error: invalid issue type %q (valid: bug, feature, task, molecule, step)", query.Type)
	}
	return s.MemStore.List(query)
}

// TestActiveStepOverAStoreThatRefusesARootType is the step reminder on a
// split city whose work store is served through bd: bd refuses a typed listing
// of a type the scope does not register, and "wisp" is one of the two root
// types the reminder asks for but not one gc registers. The refusal says the
// type cannot be pushed, not that the store is unavailable. Read as a failure
// it turns every prompt's GET /work/active-step into a 503, and each hook then
// opens the stores itself.
func TestActiveStepOverAStoreThatRefusesARootType(t *testing.T) {
	assignees := []string{"alice"}

	t.Run("no molecule is no error", func(t *testing.T) {
		work := &typeRefusingStore{MemStore: beads.NewMemStore(), refuses: "wisp"}
		r := stepResolution{work: work, graph: beads.NewMemStore(), logf: t.Logf}
		step, err := r.activeStep(false, assignees)
		if err != nil {
			t.Fatalf("activeStep over a store that refuses type wisp: %v; a refused type is not a failed read", err)
		}
		if step != nil {
			t.Fatalf("activeStep = %s, want none", step.ID)
		}
	})

	t.Run("a root of the refused type is still found", func(t *testing.T) {
		work := &typeRefusingStore{MemStore: beads.NewMemStore(), refuses: "wisp"}
		root := mustCreateInProgress(t, work.MemStore, beads.Bead{Title: "patrol", Type: "wisp", Assignee: "alice"})
		// Newer in-progress work of other types must not crowd the root out
		// of the sample: the type is applied before the cut.
		for i := range activeMoleculeSample + 2 {
			mustCreateInProgress(t, work.MemStore, beads.Bead{Title: fmt.Sprintf("task %d", i), Type: "task", Assignee: "alice"})
		}
		want := mustCreateInProgress(t, work.MemStore, beads.Bead{
			Title: "Step 1", Description: "Do the step", Type: "step", Assignee: "alice", ParentID: root.ID,
		})
		r := stepResolution{work: work, graph: beads.NewMemStore(), logf: t.Logf}
		step, err := r.activeStep(false, assignees)
		if err != nil {
			t.Fatalf("activeStep: %v", err)
		}
		if step == nil || step.ID != want.ID {
			t.Fatalf("activeStep = %v, want the in-progress step %s of the wisp root: a store that cannot push the type is listed without it", step, want.ID)
		}
	})

	t.Run("any other failure is still a failure", func(t *testing.T) {
		work := &typeRefusingStore{MemStore: beads.NewMemStore(), refuses: "wisp"}
		r := stepResolution{work: work, graph: failingListStore{beads.NewMemStore()}, logf: t.Logf}
		if _, err := r.activeStep(false, assignees); err == nil {
			t.Fatal("activeStep over a store whose listing failed returned no error")
		}
	})
}
