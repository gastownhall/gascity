package main

import (
	"io"
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Tests for the bead_not_open skip classification (issue #6024): a control
// dispatch whose ProcessControl call no-oped must reach the drain as a skip
// rather than as progress, so a persistent non-open queue row cannot re-query
// with no sleep, write .gc/dispatch-wake on every pass, or pin the --follow
// idle backoff at its 1s floor.

// TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning is the AC-1
// pin: a queue row for a control bead whose stored status is not open (closed,
// or stuck at in_progress) makes ProcessControl no-op, and that no-op must end
// the drain pass as a skip — one queue query, one dispatch, no wake-file write,
// processedAny=false — instead of counting as progress and re-querying with no
// sleep.
func TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning(t *testing.T) {
	for _, status := range []string{"closed", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			cityDir, store := setUpControlReadyFileStoreCity(t)
			noBDOnPathForTest(t)

			control := createRoutedControl(t, store, beadmeta.KindScopeCheck)
			if status == "closed" {
				if err := store.Close(control.ID); err != nil {
					t.Fatalf("close control: %v", err)
				}
			} else {
				if err := store.Update(control.ID, beads.UpdateOpts{Status: &status}); err != nil {
					t.Fatalf("set control status to %s: %v", status, err)
				}
			}

			prevList := workflowServeList
			prevControl := controlDispatcherServe
			prevInterval := workflowServeIdlePollInterval
			prevAttempts := workflowServeIdlePollAttempts
			workflowServeIdlePollInterval = 0
			workflowServeIdlePollAttempts = 0
			t.Cleanup(func() {
				workflowServeList = prevList
				controlDispatcherServe = prevControl
				workflowServeIdlePollInterval = prevInterval
				workflowServeIdlePollAttempts = prevAttempts
			})

			scans := 0
			workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
				scans++
				if scans > 20 {
					t.Fatalf("drain re-queried a skipped control %d times: a bead_not_open no-op counted as progress spins the drain with no sleep", scans)
				}
				return []hookBead{{ID: control.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}}}, nil
			}
			dispatches := 0
			controlDispatcherServe = func(cityPath, storePath, beadID string, stdout, stderr io.Writer, emits *executionEmitDeferral) error {
				dispatches++
				return prevControl(cityPath, storePath, beadID, stdout, stderr, emits)
			}

			result, err := drainWorkflowServeWork(freshnessControlAgent(), cityDir, cityDir, workflowServeControlReadyQuery(freshnessControlAgent()), nil, io.Discard)
			if err != nil {
				t.Fatalf("drainWorkflowServeWork: %v", err)
			}
			if scans != 1 || dispatches != 1 {
				t.Errorf("queue queries = %d, dispatches = %d, want 1 and 1: a skipped control ends the drain pass instead of re-querying", scans, dispatches)
			}
			if result.processedAny {
				t.Errorf("processedAny = true for a drain that only skipped a non-open control, want false: the --follow idle backoff can never engage")
			}
			if result.pendingAny {
				t.Errorf("pendingAny = true for a drain that only skipped a non-open control, want false")
			}
			if _, statErr := os.Stat(dispatchWakeFile(cityDir)); statErr == nil {
				t.Errorf("dispatch wake file written for a drain that only skipped a non-open control")
			}
		})
	}
}

// TestDrainWorkflowServeWorkKeepsIdleRePollsAfterProcessedPass pins the
// bounded idle exit the skip classification now shares with the empty queue:
// after a pass that advanced a control, the drain still re-polls the queue
// workflowServeIdlePollAttempts times (absorbing read-after-write lag in the
// ready source) before exiting, so classifying skips cannot cost the
// post-progress re-polls.
func TestDrainWorkflowServeWorkKeepsIdleRePollsAfterProcessedPass(t *testing.T) {
	clearGCEnv(t)

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
		if scans == 1 {
			return []hookBead{{ID: "gc-ctrl", Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}}}, nil
		}
		return nil, nil
	}
	dispatches := 0
	controlDispatcherServe = func(_, _, _ string, _, _ io.Writer, _ *executionEmitDeferral) error {
		dispatches++
		return nil
	}

	result, err := drainWorkflowServeWork(freshnessControlAgent(), t.TempDir(), t.TempDir(), "bd ready", nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if !result.processedAny {
		t.Fatal("drain reported no progress, want the dispatched control counted as processed")
	}
	if dispatches != 1 {
		t.Fatalf("dispatches = %d, want 1", dispatches)
	}
	wantScans := 2 + workflowServeIdlePollAttempts
	if scans != wantScans {
		t.Fatalf("queue queries = %d, want %d (one processed pass, %d idle re-polls, one final poll before idle-exit)", scans, wantScans, workflowServeIdlePollAttempts)
	}
}
