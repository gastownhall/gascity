package dispatch

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
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
