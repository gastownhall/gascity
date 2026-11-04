package dispatch

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
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
			result, err := ProcessControl(beadstest.Graph(graph), control, opts)
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
				result, err = ProcessControl(beadstest.Graph(graph), control, opts)
			}
			if err != nil || !result.Processed {
				t.Fatalf("result=%+v err=%v", result, err)
			}
		})
	}
}

// processNativeControl composes named class handles for a native fixture.
// A zero Work handle is the graph store itself, which is the single-store
// case and leaves the member tail off. A caller that names a separate work
// store sets Work and MemberTail. Missing InputStores are filled from that
// work store so a root carrying gc.input_convoy_id is not refused for lack
// of handles.
func processNativeControl(graph beads.Store, control beads.Bead, opts ProcessOptions) (ControlResult, error) {
	if opts.Work == (beads.WorkStore{}) {
		opts.Work = beadstest.Work(graph)
	}
	if opts.InputStores == nil {
		opts.InputStores = nativeFixtureInputs(graph, opts.Work.Store)
	}
	return ProcessControl(beadstest.Graph(graph), control, opts)
}

func TestNativeDrainMaterializesBeforeInputExecutionAdmission(t *testing.T) {
	dir := t.TempDir()
	writeDrainItemFormula(t, dir)
	work, graph, control, members := seedSplitClassDrainWorkflow(t, false)
	mustDepAdd(t, work, members[1], members[0], "blocks")
	inputs := sourceworkflow.InputStores{Graph: beadstest.Graph(graph), Work: []beads.WorkStore{beadstest.Work(work)}}
	opts := ProcessOptions{InputStores: &inputs, Work: beadstest.Work(work), MemberTail: true, FormulaSearchPaths: []string{dir}}
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
	if _, err := ProcessControl(beadstest.Graph(graph), control, opts); !errors.Is(err, ErrControlPending) {
		t.Fatalf("held expansion: %v", err)
	}
	if err := work.Update(parent, beads.UpdateOpts{RemoveLabels: []string{"hold:external"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := ProcessControl(beadstest.Graph(graph), control, opts); err != nil {
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

// Recorded drain outcomes and spawned fanouts only settle graph state. Neither
// discovery nor execution may reopen unavailable input scopes to finish them.
func TestNativeControlSettlementWithoutInputs(t *testing.T) {
	for _, tc := range []struct{ kind, stateKey, state, outcome string }{
		{"drain", "gc.drain_state", "succeeded", "pass"},
		{"drain", "gc.drain_state", "failed", "fail"},
		{"fanout", "gc.fanout_state", "spawned", "pass"},
	} {
		t.Run(tc.kind+"/"+tc.state, func(t *testing.T) {
			graph := beads.NewMemStoreFrom(0, []beads.Bead{
				{ID: "root", Type: "molecule", Status: "open", Metadata: map[string]string{"gc.input_convoy_id": "unavailable"}},
				{ID: "control", Status: "open", Metadata: map[string]string{"gc.kind": tc.kind, "gc.root_bead_id": "root", tc.stateKey: tc.state, "gc.outcome": tc.outcome}},
				{ID: "child", Status: "closed", Metadata: map[string]string{"gc.root_bead_id": "root", "gc.outcome": tc.outcome}},
			}, []beads.Dep{{IssueID: "control", DependsOnID: "child", Type: "blocks"}})
			control := mustGet(t, graph, "control")
			inputs := sourceworkflow.InputStores{Graph: beadstest.Graph(graph), OpenWork: func(context.Context) ([]beads.WorkStore, error) {
				t.Error("settlement opened work scopes")
				return nil, errors.New("input scope unavailable")
			}}
			legs := []readywork.Leg{{Label: "graph", Store: graph, Graph: inputs.Graph}}
			rows, err := readywork.Read(legs, readywork.Query{})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(rows, func(b readywork.ReadyBead) bool { return b.ID == control.ID }) {
				t.Fatal("settlement missing from ready")
			}
			demand, err := readywork.NewLegGate(legs, beads.ReadyQuery{}).Gate(context.Background(), 0, []beads.Bead{control})
			if err != nil || len(demand) != 1 {
				t.Fatalf("settlement demand: %v, %v", demand, err)
			}
			before, err := graph.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if err != nil {
				t.Fatal(err)
			}
			result, err := ProcessControl(beadstest.Graph(graph), control, ProcessOptions{Work: beadstest.Work(graph), InputStores: &inputs})
			if err != nil || !result.Processed {
				t.Fatalf("settle: %+v, %v", result, err)
			}
			after := mustGet(t, graph, control.ID)
			if after.Status != "closed" || after.Metadata["gc.outcome"] != tc.outcome {
				t.Fatalf("settlement = %+v", after)
			}
			all, err := graph.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
			if err != nil || len(all) != len(before) {
				t.Fatalf("settlement created beads: before=%d after=%d err=%v", len(before), len(all), err)
			}
		})
	}
}

func TestNativeExpandedSharedDrainStillRequiresInputs(t *testing.T) {
	graph := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "root", Status: "open", Metadata: map[string]string{"gc.input_convoy_id": "convoy"}},
		{ID: "control", Status: "open", Metadata: map[string]string{"gc.kind": "drain", "gc.root_bead_id": "root", "gc.drain_state": "expanded", "gc.drain_context": "shared"}},
	}, nil)
	opens := 0
	unavailable := errors.New("work scope unavailable")
	inputs := sourceworkflow.InputStores{Graph: beadstest.Graph(graph), OpenWork: func(context.Context) ([]beads.WorkStore, error) { opens++; return nil, unavailable }}
	result, err := ProcessControl(beadstest.Graph(graph), mustGet(t, graph, "control"), ProcessOptions{InputStores: &inputs})
	if !errors.Is(err, unavailable) || result.Processed || opens != 1 {
		t.Fatalf("result=%+v err=%v opens=%d", result, err, opens)
	}
	rows, err := graph.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil || len(rows) != 2 || mustGet(t, graph, "control").Status != "open" {
		t.Fatalf("refused drain mutated graph: %v, %v", rows, err)
	}
}
