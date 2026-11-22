package main

import (
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestActiveMoleculeTypesAreRegistered pins that every type the step
// reminder asks a store to list is one gc registers with every bd store it
// provisions. bd refuses a typed listing of a type the scope does not
// register (invalid issue type), and that refusal — not "no molecule found"
// — is what sent GET /work/active-step to 503 when "wisp" was on this list
// but not in RequiredCustomTypes.
func TestActiveMoleculeTypesAreRegistered(t *testing.T) {
	registered := map[string]bool{}
	for _, typ := range doctor.RequiredCustomTypes {
		registered[typ] = true
	}
	for _, typ := range activeMoleculeTypes {
		if !registered[typ] {
			t.Errorf("activeMoleculeTypes contains %q, which is not in doctor.RequiredCustomTypes; a store that validates types refuses a typed listing of it", typ)
		}
	}
}

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

// TestActiveStepOverABdServedWorkStore is the step reminder on a split city
// whose work store is served through bd (the default born-split shape): a
// typed listing of an unregistered type is refused outright, before the
// attached-formula bridge or the description fallback ever run. Because
// activeMoleculeTypes now holds only "molecule" — a type gc both writes and
// registers — the work store never sees a typed listing it would refuse.
func TestActiveStepOverABdServedWorkStore(t *testing.T) {
	assignees := []string{"alice"}

	t.Run("attached-formula bridge", func(t *testing.T) {
		work := &typeRefusingStore{MemStore: beads.NewMemStore(), refuses: "wisp"}
		graph := beads.NewMemStore()

		root := mustCreateInProgress(t, graph, beads.Bead{Title: "root", Type: "molecule"})
		step := mustCreateInProgress(t, graph, beads.Bead{
			Title: "Step 1", Description: "Do the step", Type: "step", ParentID: root.ID,
		})
		_ = mustCreateInProgress(t, work.MemStore, beads.Bead{
			Title:    "source",
			Type:     "task",
			Assignee: "alice",
			Metadata: map[string]string{"molecule_id": root.ID},
		})

		r := splitStepResolution(t, work, graph)
		got, err := r.activeStep(false, assignees)
		if err != nil {
			t.Fatalf("activeStep: %v", err)
		}
		if got == nil || got.ID != step.ID {
			t.Fatalf("activeStep = %v, want the bridged step %s", got, step.ID)
		}
	})

	t.Run("description fallback", func(t *testing.T) {
		work := &typeRefusingStore{MemStore: beads.NewMemStore(), refuses: "wisp"}
		graph := beads.NewMemStore()

		want := mustCreateInProgress(t, work.MemStore, beads.Bead{
			Title: "t-desc", Type: "task", Assignee: "alice", Description: "do it",
		})

		r := splitStepResolution(t, work, graph)
		got, err := r.activeStep(false, assignees)
		if err != nil {
			t.Fatalf("activeStep: %v", err)
		}
		if got == nil || got.ID != want.ID {
			t.Fatalf("activeStep = %v, want the described claimed bead %s", got, want.ID)
		}
	})

	t.Run("an assigned molecule root still wins", func(t *testing.T) {
		work := &typeRefusingStore{MemStore: beads.NewMemStore(), refuses: "wisp"}
		graph := beads.NewMemStore()

		root := mustCreateInProgress(t, graph, beads.Bead{Title: "root", Type: "molecule", Assignee: "alice"})
		step := mustCreateInProgress(t, graph, beads.Bead{
			Title: "Step 1", Description: "Do the step", Type: "step", ParentID: root.ID,
		})
		_ = mustCreateInProgress(t, work.MemStore, beads.Bead{
			Title: "t-desc", Type: "task", Assignee: "alice", Description: "decoy",
		})

		r := splitStepResolution(t, work, graph)
		got, err := r.activeStep(false, assignees)
		if err != nil {
			t.Fatalf("activeStep: %v", err)
		}
		if got == nil || got.ID != step.ID {
			t.Fatalf("activeStep = %v, want the assigned molecule root's step %s", got, step.ID)
		}
	})

	t.Run("a failing graph leg still errors", func(t *testing.T) {
		work := &typeRefusingStore{MemStore: beads.NewMemStore(), refuses: "wisp"}
		r := splitStepResolution(t, work, failingListStore{beads.NewMemStore()})
		if _, err := r.activeStep(false, assignees); err == nil {
			t.Fatal("activeStep over a store whose listing failed returned no error")
		}
	})
}

// splitStepResolution is a step resolution over distinct work and graph
// stores, as a split city's: the graph store leads the molecule readers and
// the work store follows.
func splitStepResolution(t *testing.T, work, graph activeStepReader) stepResolution {
	t.Helper()
	return stepResolution{
		work: work, graph: graph, logf: t.Logf,
		molecules: []stepMoleculeLeg{{list: graph, strict: graph}, {list: work, strict: work}},
	}
}
