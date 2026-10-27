package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
)

const reproRoutedFormula = `formula = "routed-flow"
version = 1

[[steps]]
id = "work"
title = "Work"
assignee = "worker"
metadata = { "gc.routed_to" = "pool/worker", "gc.execution_routed_to" = "pool/worker" }
`

// TestReproConvergeStopLeavesWispStepsOpen: `gc converge stop` terminates the
// loop and closes its active wisp root, but leaves that wisp's step beads
// open, assigned and routed. From the second iteration on (condition gate),
// the step is an activated task and Ready() offers it to the worker.
func TestReproConvergeStopLeavesWispStepsOpen(t *testing.T) {
	for _, tc := range []struct {
		name       string
		params     map[string]string
		iterations int // wisps closed by the "agent" before the stop
	}{
		{"manual-gate/iteration-1", map[string]string{}, 0},
		{"condition-gate/iteration-2", map[string]string{"gate_mode": "condition"}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cr, store := setupConvergenceRuntime(t)
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "routed-flow.toml"), []byte(reproRoutedFormula), 0o644); err != nil {
				t.Fatal(err)
			}
			cond := filepath.Join(dir, "never-converged.sh")
			if err := os.WriteFile(cond, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			cr.convScopes[""] = cr.newConvergenceScope("", store, cr.cityPath, []string{dir}, false)

			params := map[string]string{"formula": "routed-flow", "target": "worker", "max_iterations": "5"}
			for k, v := range tc.params {
				params[k] = v
			}
			if params["gate_mode"] == "condition" {
				params["gate_condition"] = cond
			}
			reply := sendAndReceive(t, cr, convergenceRequest{Command: "create", Params: params})
			if reply.Error != "" {
				t.Fatalf("create: %s", reply.Error)
			}
			var created convergence.CreateResult
			if err := json.Unmarshal(reply.Result, &created); err != nil {
				t.Fatal(err)
			}
			if err := hqScope(t, cr).adapter.populateIndex(); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < tc.iterations; i++ {
				meta, _ := store.Get(created.BeadID)
				active := meta.Metadata[convergence.FieldActiveWisp]
				kids, _ := store.Children(active)
				for _, k := range kids {
					_ = store.Close(k.ID)
				}
				if err := store.Close(active); err != nil {
					t.Fatal(err)
				}
				cr.convergenceTick(context.Background())
			}
			root, _ := store.Get(created.BeadID)
			wispID := root.Metadata[convergence.FieldActiveWisp]
			t.Logf("before stop: state=%s iteration wisp=%s", root.Metadata[convergence.FieldState], wispID)
			steps, err := store.Children(wispID)
			if err != nil || len(steps) == 0 {
				t.Fatalf("wisp %s steps: %v (n=%d)", wispID, err, len(steps))
			}

			if r := sendAndReceive(t, cr, convergenceRequest{Command: "stop", BeadID: created.BeadID, User: "op"}); r.Error != "" {
				t.Fatalf("stop: %s", r.Error)
			}
			loop, _ := store.Get(created.BeadID)
			wisp, _ := store.Get(wispID)
			t.Logf("after stop: loop %s status=%s state=%s, wisp %s status=%s",
				loop.ID, loop.Status, loop.Metadata[convergence.FieldState], wisp.ID, wisp.Status)
			ready, err := store.Ready(beads.ReadyQuery{Assignee: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			inReady := map[string]bool{}
			for _, b := range ready {
				inReady[b.ID] = true
			}
			for _, s := range steps {
				got, _ := store.Get(s.ID)
				t.Logf("after stop: step %s %q type=%s status=%s assignee=%q gc.routed_to=%q in Ready(assignee=worker)=%v",
					got.ID, got.Title, got.Type, got.Status, got.Assignee, got.Metadata["gc.routed_to"], inReady[got.ID])
				if got.Status != "closed" {
					t.Errorf("step %s of the stopped loop's wisp is still %q", got.ID, got.Status)
				}
			}
		})
	}
}
