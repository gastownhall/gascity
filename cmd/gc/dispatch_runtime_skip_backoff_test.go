package main

import (
	"errors"
	"io"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/events"
)

// TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps is the AC-2 pin for
// issue #6024: a --follow loop whose every drain sees only a control bead the
// dispatcher skips (stored status not open) must let its idle backoff grow —
// waits of 1s, 2s, 4s, then the 5s cap — instead of every drain reporting
// processedAny and pinning the loop at followSleepDuration's 1s floor.
func TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps(t *testing.T) {
	eventsDir := t.TempDir()
	ep := newTestProvider(t, eventsDir)

	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)
	closed := createRoutedControl(t, store, beadmeta.KindScopeCheck)
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("close control: %v", err)
	}

	prevList := workflowServeList
	prevProvider := workflowServeOpenEventsProvider
	prevWait := workflowServeWaitForWake
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	workflowServeOpenEventsProvider = func(io.Writer) (events.Provider, error) { return ep, nil }
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		workflowServeList = prevList
		workflowServeOpenEventsProvider = prevProvider
		workflowServeWaitForWake = prevWait
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	scans := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		scans++
		if scans > 20 {
			t.Fatalf("drain re-queried a skipped control %d times: a bead_not_open no-op counted as progress spins the drain with no sleep", scans)
		}
		return []hookBead{{ID: closed.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}}}, nil
	}

	type waitCall struct {
		idleSweeps int
		sleepDur   time.Duration
	}
	var waitCalls []waitCall
	stopErr := errors.New("stop after sequence")
	workflowServeWaitForWake = func(_ <-chan workflowWatchResult, sleepDur time.Duration, idleSweeps int) (bool, error) {
		waitCalls = append(waitCalls, waitCall{idleSweeps: idleSweeps, sleepDur: sleepDur})
		if len(waitCalls) == 4 {
			return false, stopErr
		}
		return false, nil
	}

	// controlDispatcherServe stays production: the closed control reaches
	// ProcessControl, whose no-op crosses the serve seam as a skip report.

	err := runWorkflowServeFollow(freshnessControlAgent(), cityDir, cityDir, workflowServeControlReadyQuery(freshnessControlAgent()), nil, io.Discard)
	if !errors.Is(err, stopErr) {
		t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, stopErr)
	}
	want := []waitCall{
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 1, sleepDur: 2 * time.Second},
		{idleSweeps: 2, sleepDur: 4 * time.Second},
		{idleSweeps: 3, sleepDur: 5 * time.Second},
	}
	if !slices.Equal(waitCalls, want) {
		t.Fatalf("wait calls = %#v, want %#v: skip-only sweeps must grow the idle backoff to the %s cap", waitCalls, want, workflowServeMaxIdleSleep)
	}
	if scans != len(waitCalls) {
		t.Fatalf("queue queries = %d, want %d (one scan per sweep; a skip-only drain exits immediately)", scans, len(waitCalls))
	}
}
