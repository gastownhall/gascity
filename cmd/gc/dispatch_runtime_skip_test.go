package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// These tests pin issue #6024: a control bead ProcessControl no-ops on (status
// not open) is a skip, not drain progress. Counting it as progress re-queried
// the store with no sleep, rewrote the dispatch wake file on every pass, and
// held the --follow backoff at its 1s floor for as long as the row stayed in
// the ready queue.

// serveSkipListCap bounds every stubbed queue so a drain that spins fails fast
// instead of hanging the test binary.
const serveSkipListCap = 20

// setUpServeSkipCity builds a city whose control dispatches run through the
// production controlDispatcherServe against an in-memory scope store. The
// MemStore has no CloseStore, so the per-dispatch close is a no-op and the
// same store answers every dispatch of the drain. <city>/.gc exists up front:
// writeDispatchWakeFile gives up when it is missing, which would make a "no
// wake file" assertion vacuous.
func setUpServeSkipCity(t *testing.T) (string, *beads.MemStore) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("create .gc: %v", err)
	}
	store := beads.NewMemStore()

	prevOpen := openControlStoreForDispatch
	prevServe := controlDispatcherServe
	prevList := workflowServeList
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	t.Cleanup(func() {
		openControlStoreForDispatch = prevOpen
		controlDispatcherServe = prevServe
		workflowServeList = prevList
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) {
		return store, nil
	}
	workflowServeIdlePollInterval = 0
	return cityDir, store
}

// createNonOpenScopeCheck seeds a scope-check control (no city-config
// resolution needed) and moves it to status, the stale-row shape the ready
// queue keeps offering.
func createNonOpenScopeCheck(t *testing.T, store beads.Store, status string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck},
	})
	if err != nil {
		t.Fatalf("create scope-check control: %v", err)
	}
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("set control status %q: %v", status, err)
	}
	got, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if got.Status != status {
		t.Fatalf("control status = %q, want %q", got.Status, status)
	}
	return got
}

func serveQueueRow(b beads.Bead) hookBead {
	return hookBead{ID: b.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: b.Metadata[beadmeta.KindMetadataKey]}}
}

// stubServeQueue answers every drain query from rows and fails the test once
// the drain has queried more than serveSkipListCap times. It returns the query
// count.
func stubServeQueue(t *testing.T, rows func() []hookBead) *int {
	t.Helper()
	lists := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		lists++
		if lists > serveSkipListCap {
			t.Fatalf("drain queried the queue %d times: a no-op dispatch is re-queried with no bound", lists)
		}
		return rows(), nil
	}
	return &lists
}

// countServeDispatches wraps the production serve seam so a test can count
// dispatches per bead while every dispatch still runs the real path.
func countServeDispatches(t *testing.T) map[string]int {
	t.Helper()
	counts := map[string]int{}
	controlDispatcherServe = func(cityPath, storePath, beadID string, stdout, stderr io.Writer, emits *executionEmitDeferral) error {
		counts[beadID]++
		return runControlDispatcherInStore(cityPath, storePath, beadID, stdout, stderr, emits)
	}
	return counts
}

func drainServeSkipCity(t *testing.T, cityDir string) workflowServeDrainResult {
	t.Helper()
	var stderr bytes.Buffer
	result, err := drainWorkflowServeWork(freshnessControlAgent(), cityDir, cityDir, workflowServeControlReadyQuery(freshnessControlAgent()), nil, &stderr)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v (stderr=%q)", err, stderr.String())
	}
	return result
}

func dispatchWakeFileExists(t *testing.T, cityDir string) bool {
	t.Helper()
	_, err := os.Stat(dispatchWakeFile(cityDir))
	if err == nil {
		return true
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stat dispatch wake file: %v", err)
	}
	return false
}

// TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning is AC-1: a
// stale non-open control row is dispatched once and ends the drain as idle.
func TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning(t *testing.T) {
	for _, status := range []string{"closed", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			cityDir, store := setUpServeSkipCity(t)
			stale := createNonOpenScopeCheck(t, store, status)
			lists := stubServeQueue(t, func() []hookBead { return []hookBead{serveQueueRow(stale)} })
			dispatches := countServeDispatches(t)

			result := drainServeSkipCity(t, cityDir)

			if *lists != 1 {
				t.Errorf("queue queried %d times, want 1", *lists)
			}
			if got := dispatches[stale.ID]; got != 1 {
				t.Errorf("%s control dispatched %d times, want 1", status, got)
			}
			if result.processedAny || result.pendingAny {
				t.Errorf("drain result = %+v, want neither processed nor pending for a skip-only drain", result)
			}
			if dispatchWakeFileExists(t, cityDir) {
				t.Error("dispatch wake file written for a drain that only skipped a non-open control")
			}
		})
	}
}

// TestDrainWorkflowServeWorkPendingWinsOverSkip pins that a pass holding a
// skip and a pending control ends through the pending exit and still reports
// pendingAny.
func TestDrainWorkflowServeWorkPendingWinsOverSkip(t *testing.T) {
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevControl
	})
	stubServeQueue(t, func() []hookBead {
		return []hookBead{
			{ID: "gc-skip", Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}},
			{ID: "gc-pending", Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindRetryEval}},
		}
	})
	controlDispatcherServe = func(_, _, beadID string, _, _ io.Writer, _ *executionEmitDeferral) error {
		if beadID == "gc-pending" {
			return dispatch.ErrControlPending
		}
		return errControlNotProcessed
	}

	cityPath := t.TempDir()
	result, err := drainWorkflowServeWork(config.Agent{Name: "control-dispatcher"}, cityPath, cityPath, "bd ready", nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if result.processedAny || !result.pendingAny {
		t.Fatalf("drain result = %+v, want pendingAny only", result)
	}
}
