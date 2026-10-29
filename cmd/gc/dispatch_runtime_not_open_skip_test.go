package main

import (
	"io"
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// TestDrainWorkflowServeWorkNonOpenSkipIsNotProgress reproduces issue #6024:
// ProcessControl no-ops on a control bead that is not open and returns nil,
// and the drain counts that nil as progress. A stale queue row for a closed
// control bead then re-queries with no sleep, rewrites the dispatch wake file
// on every pass, and reports processedAny so the --follow backoff never grows.
func TestDrainWorkflowServeWorkNonOpenSkipIsNotProgress(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)

	closed := createRoutedControl(t, store, beadmeta.KindWorkflowFinalize)
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("close control: %v", err)
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

	const maxScans = 50
	scans := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		scans++
		if scans > maxScans {
			return nil, nil
		}
		// A stale row: the control bead is closed in the store but still offered.
		return []hookBead{{ID: closed.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize}}}, nil
	}

	result, err := drainWorkflowServeWork(freshnessControlAgent(), cityDir, cityDir, workflowServeControlReadyQuery(freshnessControlAgent()), nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if scans > 1 {
		t.Errorf("drain scanned the queue %d times for one closed control bead, want 1: a bead_not_open no-op re-queried with no sleep", scans)
	}
	if result.processedAny {
		t.Errorf("processedAny = true for a drain that only skipped a closed control bead, want false: the --follow idle backoff can never engage")
	}
	if _, statErr := os.Stat(dispatchWakeFile(cityDir)); statErr == nil {
		t.Errorf("dispatch wake file written for a drain that only skipped a closed control bead")
	}
}
