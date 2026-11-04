package dispatch

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/readywork"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

func TestProcessControlNativeInputAdmission(t *testing.T) {
	for _, phase := range []string{"ordinary", "teardown", "terminal", "canceled"} {
		t.Run(phase, func(t *testing.T) {
			graph, work := beads.NewMemStore(), beads.NewMemStoreFrom(100, nil, nil)
			control := newTerminalRootRetryControl(t, graph, "", nil)
			rootID := control.Metadata["gc.root_bead_id"]
			conv := mustCreate(t, work, beads.Bead{Type: "convoy", Title: "input convoy", Labels: []string{"hold:external"}})
			if err := graph.SetMetadata(rootID, "gc.input_convoy_id", conv.ID); err != nil {
				t.Fatal(err)
			}
			deps, err := graph.DepList(control.ID, "down")
			if err != nil {
				t.Fatal(err)
			}
			if err := graph.SetMetadata(deps[0].DependsOnID, "gc.outcome", "pass"); err != nil {
				t.Fatal(err)
			}
			inputs := sourceworkflow.InputStores{Graph: beadstest.Graph(graph), Work: []beads.WorkStore{beadstest.Work(work)}}
			opts := ProcessOptions{InputStores: &inputs}
			switch phase {
			case "teardown":
				control.Metadata["gc.scope_role"] = "teardown"
			case "terminal":
				mustClose(t, graph, rootID)
			case "canceled":
				if err := graph.SetMetadata(rootID, "gc.cancel_requested", "true"); err != nil {
					t.Fatal(err)
				}
			}
			result, err := ProcessControl(graph, control, opts)
			if phase == "ordinary" {
				if !errors.Is(err, ErrControlPending) || result.Processed {
					t.Fatalf("result=%+v err=%v; want pending", result, err)
				}
				if got := mustGet(t, graph, control.ID).Status; got != "open" {
					t.Fatal("parked control was mutated")
				}
				if err := work.Update(conv.ID, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
					t.Fatal(err)
				}
				result, err = ProcessControl(graph, control, opts)
			}
			if err != nil || !result.Processed {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

// processNativeControl composes the native fixture's named classes. Drain
// fixtures explicitly supply separate work stores through MemberStores; fixtures
// without that list keep both classes in their single in-memory store.
func processNativeControl(graph beads.Store, control beads.Bead, opts ProcessOptions) (ControlResult, error) {
	if opts.InputStores != nil {
		return ProcessControl(graph, control, opts)
	}
	work := opts.MemberStores
	if len(work) == 0 {
		work = []beads.Store{graph}
	}
	opts.InputStores = nativeFixtureInputs(graph, work...)
	return ProcessControl(graph, control, opts)
}

func TestNativeDrainMaterializesBeforeInputExecutionAdmission(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	work, graph, control, members := seedSplitClassDrainWorkflow(t, false)
	mustDepAdd(t, work, members[1], members[0], "blocks")
	inputs := sourceworkflow.InputStores{Graph: beadstest.Graph(graph), Work: []beads.WorkStore{beadstest.Work(work)}}
	opts := ProcessOptions{InputStores: &inputs, MemberStores: []beads.Store{work}, FormulaSearchPaths: []string{dir}}
	legs := []readywork.Leg{
		{Label: "work", Store: work, Work: beadstest.Work(work), Graph: beadstest.Graph(work)},
		{Label: "graph", Store: graph, Graph: beadstest.Graph(graph), ClassBinding: true},
	}
	ready := func(id string, want bool) {
		t.Helper()
		rows, err := readywork.Read(legs, readywork.Query{})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, row := range rows {
			if row.ID == id {
				found = true
			}
		}
		if found != want {
			t.Fatalf("ready contains %s = %v, want %v", id, found, want)
		}
	}
	// The shared discovery gate must expose expansion before B is ready.
	ready(control.ID, true)
	parent := mustGet(t, graph, control.Metadata["gc.root_bead_id"]).Metadata["gc.input_convoy_id"]
	if err := work.Update(parent, beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
		t.Fatal(err)
	}
	ready(control.ID, false)
	if _, err := ProcessControl(graph, control, opts); !errors.Is(err, ErrControlPending) {
		t.Fatalf("held expansion: %v", err)
	}
	if err := work.Update(parent, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ProcessControl(graph, control, opts); err != nil {
		t.Fatal(err)
	}
	manifest := mustDrainManifest(t, mustGet(t, graph, control.ID))
	steps := make(map[string]beads.Bead)
	roots := make(map[string]string)
	for _, row := range manifest.Rows {
		steps[row.MemberID] = mustFindDrainItemWorkStep(t, graph, row.ItemRootID)
		roots[row.MemberID] = row.ItemRootID
	}
	ready(steps[members[0]].ID, true)
	ready(steps[members[1]].ID, false)
	// Completing A satisfies both the projected graph edge and live work edge.
	mustClose(t, work, members[0])
	mustClose(t, graph, roots[members[0]])
	ready(steps[members[1]].ID, true)
	gate := mustCreate(t, work, beads.Bead{Title: "late prerequisite"})
	mustDepAdd(t, work, members[1], gate.ID, "blocks")
	ready(steps[members[1]].ID, false)
	if err := work.DepRemove(members[1], gate.ID); err != nil {
		t.Fatal(err)
	}
	if err := work.Update(members[1], beads.UpdateOpts{Labels: []string{"hold:external"}}); err != nil {
		t.Fatal(err)
	}
	ready(steps[members[1]].ID, false)
	// The execution predicate and demand view agree on the same unit input.
	admission, err := inputs.Admission(context.Background(), steps[members[1]])
	if err != nil || admission.Ready {
		t.Fatalf("late hold admission: %+v, %v", admission, err)
	}
	demand, err := readywork.NewLegGate(legs, beads.ReadyQuery{}).Gate(context.Background(), 1, []beads.Bead{steps[members[1]]})
	if err != nil || len(demand) != 0 {
		t.Fatalf("late hold demand: %v, %v", demand, err)
	}
}

func nativeFixtureInputs(graph beads.Store, work ...beads.Store) *sourceworkflow.InputStores {
	inputs := &sourceworkflow.InputStores{Graph: beadstest.Graph(graph)}
	for _, store := range work {
		inputs.Work = append(inputs.Work, beadstest.Work(store))
	}
	return inputs
}
