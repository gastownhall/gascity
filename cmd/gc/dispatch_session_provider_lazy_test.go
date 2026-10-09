package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestControlDispatchBuildsNoSessionProviderUnlessARetryRecycles pins
// ga-vnycm2.18 (g1): building the session provider reads the session snapshot
// (two bd calls), and only a pooled transient retry that recycles its
// subject's session uses it. A workflow whose retries all pass dispatches
// every control without building one. The recycle path itself, which must
// still build it, is pinned by the retry-eval recycle test in
// cmd_convoy_dispatch_test.go.
func TestControlDispatchBuildsNoSessionProviderUnlessARetryRecycles(t *testing.T) {
	store, convoyID, workflowID := startMemScopedWorkflow(t)
	cfg := buildMemGraphWorkflowConfig(t)
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	built := 0
	prevProvider := dispatchControlSessionProvider
	dispatchControlSessionProvider = func() (runtime.Provider, error) {
		built++
		return runtime.NewFake(), nil
	}
	t.Cleanup(func() { dispatchControlSessionProvider = prevProvider })

	dispatched := map[string]int{}
	for step := 0; step < 200; step++ {
		if mustGetMemBead(t, store, workflowID).Status == "closed" {
			break
		}
		progressed := false
		for _, bead := range memGraphReady(t, store) {
			kind := bead.Metadata["gc.kind"]
			if !graphroute.IsControlDispatcherKind(kind) {
				continue
			}
			if err := runControlDispatcherWithStoreAndConfig(cityPath, cityPath, store, bead.ID, cfg, io.Discard, io.Discard); err != nil {
				t.Fatalf("dispatch %s (%s): %v", bead.ID, kind, err)
			}
			dispatched[kind]++
			progressed = true
		}
		for {
			ready := memGraphReady(t, store)
			if i, ok := firstClaimableGraphWorkerBead(ready, "worker"); ok {
				worker := "worker"
				if err := store.Update(ready[i].ID, beads.UpdateOpts{Assignee: &worker}); err != nil {
					t.Fatalf("claim %s: %v", ready[i].ID, err)
				}
				ready[i] = mustGetMemBead(t, store, ready[i].ID)
			}
			bead, ok, err := selectExecutableGraphWorkerBead(ready, "worker")
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			executeMemGraphWorkerBead(t, store, bead, convoyID, cityPath, "success")
			progressed = true
		}
		if !progressed {
			t.Fatalf("workflow %s made no progress", workflowID)
		}
	}
	if got := mustGetMemBead(t, store, workflowID).Status; got != "closed" {
		t.Fatalf("workflow status = %q, want closed", got)
	}
	if dispatched["retry"] == 0 {
		t.Fatalf("premise: no retry control was dispatched (dispatched=%v)", dispatched)
	}
	if built != 0 {
		t.Fatalf("session provider built %d times for %v, want 0: no retry recycled a session", built, dispatched)
	}
}
