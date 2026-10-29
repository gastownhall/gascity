package main

import (
	"io"
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestDrainWorkflowServeWorkProcessesLiveControlBesideStaleRow is the AC-3 pin
// for issue #6024: a live control bead must still be dispatched beside a
// persistent non-open row — processed once, wake file written — while the
// stale row's skip-only passes end the drain through the bounded idle path
// after workflowServeIdlePollAttempts re-polls instead of spinning forever.
func TestDrainWorkflowServeWorkProcessesLiveControlBesideStaleRow(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)

	// The live bead is the orphaned scope-check from the scope-store leak
	// fixture: gc.root_bead_id names a root the store does not hold, so
	// ProcessControl closes the control and reports Processed.
	live, err := store.Create(beads.Bead{
		Type: "task",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:     freshnessControlTarget,
			beadmeta.KindMetadataKey:         beadmeta.KindScopeCheck,
			beadmeta.RootBeadIDMetadataKey:   "ga-missing-root",
			beadmeta.RootStoreRefMetadataKey: "city:test-city",
		},
	})
	if err != nil {
		t.Fatalf("create live control: %v", err)
	}
	stale := createRoutedControl(t, store, beadmeta.KindScopeCheck)
	if err := store.Close(stale.ID); err != nil {
		t.Fatalf("close stale control: %v", err)
	}

	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 3
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	scans := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		scans++
		if scans > 12 {
			t.Fatalf("drain re-queried the queue %d times: a skip-only pass must end through the bounded idle path", scans)
		}
		queue := []hookBead{{ID: stale.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}}}
		if b, getErr := store.Get(live.ID); getErr == nil && b.Status == "open" {
			queue = append(queue, hookBead{ID: live.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}})
		}
		return queue, nil
	}
	dispatches := map[string]int{}
	controlDispatcherServe = func(cityPath, storePath, beadID string, stdout, stderr io.Writer, emits *executionEmitDeferral) error {
		dispatches[beadID]++
		return prevControl(cityPath, storePath, beadID, stdout, stderr, emits)
	}

	result, err := drainWorkflowServeWork(freshnessControlAgent(), cityDir, cityDir, workflowServeControlReadyQuery(freshnessControlAgent()), nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if !result.processedAny {
		t.Fatal("processedAny = false, want the live control processed beside the stale row")
	}
	if got := dispatches[live.ID]; got != 1 {
		t.Fatalf("live control dispatched %d times, want 1", got)
	}
	wantStale := 2 + workflowServeIdlePollAttempts
	if got := dispatches[stale.ID]; got != wantStale {
		t.Fatalf("stale control dispatched %d times, want %d (once beside the processed pass, once per skip-only re-poll, once on the final exit poll)", got, wantStale)
	}
	if scans != wantStale {
		t.Fatalf("queue queries = %d, want %d (one processed pass, %d skip-only re-polls, one final exit poll)", scans, wantStale, workflowServeIdlePollAttempts)
	}
	if b, getErr := store.Get(live.ID); getErr != nil || b.Status != "closed" {
		t.Fatalf("live control status = %q (err %v), want closed", b.Status, getErr)
	}
	if _, statErr := os.Stat(dispatchWakeFile(cityDir)); statErr != nil {
		t.Errorf("dispatch wake file missing after a pass that processed a control: %v", statErr)
	}
}
