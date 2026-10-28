package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// This file pins the serve drain's handling of a control dispatch that made no
// change (gastownhall/gascity#6024): ProcessControl no-ops on a control bead
// whose status is not open and returns a nil error, which the drain used to
// count as progress — re-querying with no sleep, rewriting the dispatch wake
// file on every pass, and reporting processedAny so the --follow backoff
// never engaged. A no-op must count as neither processed nor pending, and a
// skip-only pass must end through the same bounded idle path an empty queue
// uses.

// ensureDispatchWakeDir creates the <city>/.gc directory the dispatch wake
// file lands in, so a test asserting the file was NOT written is not vacuous
// (writeDispatchWakeFile traces and returns when .gc is missing).
func ensureDispatchWakeDir(t *testing.T, cityDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("create .gc dir: %v", err)
	}
}

// TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning is the #6024
// regression: a serve queue that keeps offering one control bead whose stored
// status is not open must not spin the drain. The drain queries the queue
// once, dispatches the bead once, and reports a pass that is neither processed
// nor pending, with no dispatch wake file.
func TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning(t *testing.T) {
	for _, status := range []string{"closed", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			cityDir, store := setUpControlReadyFileStoreCity(t)
			noBDOnPathForTest(t)
			ensureDispatchWakeDir(t, cityDir)

			stale := createRoutedControl(t, store, beadmeta.KindScopeCheck)
			if status == "closed" {
				if err := store.Close(stale.ID); err != nil {
					t.Fatalf("close control bead: %v", err)
				}
			} else {
				if err := store.Update(stale.ID, beads.UpdateOpts{Status: &status}); err != nil {
					t.Fatalf("set control status %s: %v", status, err)
				}
			}

			prevList := workflowServeList
			prevInterval := workflowServeIdlePollInterval
			prevAttempts := workflowServeIdlePollAttempts
			workflowServeIdlePollInterval = 0
			workflowServeIdlePollAttempts = 0
			t.Cleanup(func() {
				workflowServeList = prevList
				workflowServeIdlePollInterval = prevInterval
				workflowServeIdlePollAttempts = prevAttempts
			})

			scans := 0
			workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
				scans++
				if scans > 20 {
					t.Fatalf("drain scanned the queue %d times for one %s control bead, want 1: a bead_not_open no-op must not re-query with no sleep", scans, status)
				}
				return []hookBead{{ID: stale.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}}}, nil
			}

			result, err := drainWorkflowServeWork(freshnessControlAgent(), cityDir, cityDir, workflowServeControlReadyQuery(freshnessControlAgent()), nil, io.Discard)
			if err != nil {
				t.Fatalf("drainWorkflowServeWork: %v", err)
			}
			if scans != 1 {
				t.Errorf("drain scanned the queue %d times for one %s control bead, want 1", scans, status)
			}
			if result.processedAny {
				t.Errorf("processedAny = true for a skip-only drain, want false: a no-op must not reset the --follow idle backoff")
			}
			if result.pendingAny {
				t.Errorf("pendingAny = true for a skip-only drain, want false")
			}
			if _, statErr := os.Stat(dispatchWakeFile(cityDir)); statErr == nil {
				t.Errorf("dispatch wake file written for a skip-only drain, want none: a skip must not tell every worker to skip its poll sleep")
			}
		})
	}
}

// TestDrainWorkflowServeWorkSkipBesidePendingStillReportsPending is the mixed
// edge: a pass with one skip and one pending row is a pending pass — pending
// wins over skips, the drain ends through serve pending-queue, and pendingAny
// stays set so the --follow loop still wakes. A skip must not turn a pass that
// holds real waiting work into an idle sweep.
func TestDrainWorkflowServeWorkSkipBesidePendingStillReportsPending(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)
	stale := createRoutedControl(t, store, beadmeta.KindScopeCheck)
	if err := store.Close(stale.ID); err != nil {
		t.Fatalf("close control bead: %v", err)
	}

	prevList := workflowServeList
	prevServe := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevServe
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	scans := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		scans++
		if scans > 20 {
			t.Fatalf("drain scanned the queue %d times, want 1: pending wins over skips and ends the pass", scans)
		}
		return []hookBead{
			{ID: stale.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}},
			{ID: "gc-pending", Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindRetry}},
		}, nil
	}
	// The stale row goes through the production seam so the skip is real; the
	// pending row is a stubbed ErrControlPending.
	controlDispatcherServe = func(cityPath, storePath, beadID string, stdout, stderr io.Writer, emits *executionEmitDeferral) error {
		if beadID == "gc-pending" {
			return dispatch.ErrControlPending
		}
		return runControlDispatcherInStore(cityPath, storePath, beadID, stdout, stderr, emits)
	}

	result, err := drainWorkflowServeWork(freshnessControlAgent(), cityDir, cityDir, workflowServeControlReadyQuery(freshnessControlAgent()), nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if scans != 1 {
		t.Errorf("drain scanned the queue %d times, want 1", scans)
	}
	if !result.pendingAny {
		t.Errorf("pendingAny = false, want true: the pending row must still wake the --follow loop")
	}
	if result.processedAny {
		t.Errorf("processedAny = true, want false: the skip wrote nothing")
	}
}

// TestDrainWorkflowServeWorkKeepsIdleRepollsAfterProcessedPass pins that the
// skip fix does not touch the re-poll budget a processed pass buys: an empty
// queue following real progress still gets its workflowServeIdlePollAttempts
// re-polls (read-after-write lag absorption for the ready source) before the
// drain exits. Passes before and after the skip fix.
func TestDrainWorkflowServeWorkKeepsIdleRepollsAfterProcessedPass(t *testing.T) {
	prevList := workflowServeList
	prevServe := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 3
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevServe
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	scans := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		scans++
		if scans > 1+workflowServeIdlePollAttempts+1 {
			t.Fatalf("drain scanned the queue %d times, want %d", scans, 1+workflowServeIdlePollAttempts+1)
		}
		if scans == 1 {
			return []hookBead{{ID: "gc-live", Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}}}, nil
		}
		return nil, nil
	}
	controlDispatcherServe = func(_, _, _ string, _, _ io.Writer, _ *executionEmitDeferral) error {
		return nil // nil = the dispatch advanced the bead
	}

	result, err := drainWorkflowServeWork(freshnessControlAgent(), t.TempDir(), t.TempDir(), "bd ready", nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if !result.processedAny {
		t.Fatal("processedAny = false, want true")
	}
	wantScans := 1 + workflowServeIdlePollAttempts + 1
	if scans != wantScans {
		t.Errorf("drain scanned the queue %d times, want %d (processed pass + %d idle re-polls + exit scan)", scans, wantScans, workflowServeIdlePollAttempts)
	}
}
