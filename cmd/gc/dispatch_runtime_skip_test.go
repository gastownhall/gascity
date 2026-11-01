package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
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
// queue keeps offering. It declares the graph.v2 contract, as production
// control beads do, so it classifies as graph class: the split-strict gate
// reads a control bead through the graph view.
func createNonOpenScopeCheck(t *testing.T, store beads.Store, status string) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Type: "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindScopeCheck,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
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

// createOrphanedScopeCheck seeds an open scope-check whose root the store does
// not hold: ProcessControl closes it as orphaned-workflow and reports it
// Processed.
func createOrphanedScopeCheck(t *testing.T, store beads.Store) beads.Bead {
	t.Helper()
	b, err := store.Create(beads.Bead{
		Type: "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindScopeCheck,
			beadmeta.RootBeadIDMetadataKey:   "ga-missing-root",
			beadmeta.RootStoreRefMetadataKey: "city:test-city",
		},
	})
	if err != nil {
		t.Fatalf("create orphaned scope-check control: %v", err)
	}
	return b
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

// TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps is AC-2: sweeps that only
// skip a stale row let the --follow idle backoff grow to its cap.
func TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps(t *testing.T) {
	cityDir, store := setUpServeSkipCity(t)
	stale := createNonOpenScopeCheck(t, store, "closed")
	stubServeQueue(t, func() []hookBead { return []hookBead{serveQueueRow(stale)} })
	countServeDispatches(t)

	ep := newTestProvider(t, t.TempDir())
	prevProvider := workflowServeOpenEventsProvider
	prevWait := workflowServeWaitForWake
	t.Cleanup(func() {
		workflowServeOpenEventsProvider = prevProvider
		workflowServeWaitForWake = prevWait
	})
	workflowServeOpenEventsProvider = func(io.Writer) (events.Provider, error) { return ep, nil }

	type waitCall struct {
		idleSweeps int
		sleepDur   time.Duration
	}
	var waits []waitCall
	stopErr := fmt.Errorf("stop after four sweeps")
	workflowServeWaitForWake = func(_ <-chan workflowWatchResult, sleepDur time.Duration, idleSweeps int) (bool, error) {
		waits = append(waits, waitCall{idleSweeps: idleSweeps, sleepDur: sleepDur})
		if len(waits) == 4 {
			return false, stopErr
		}
		return false, nil
	}

	agent := freshnessControlAgent()
	err := runWorkflowServeFollow(agent, cityDir, cityDir, workflowServeControlReadyQuery(agent), nil, io.Discard)
	if !errors.Is(err, stopErr) {
		t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, stopErr)
	}
	want := []waitCall{
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 1, sleepDur: 2 * time.Second},
		{idleSweeps: 2, sleepDur: 4 * time.Second},
		{idleSweeps: 3, sleepDur: 5 * time.Second},
	}
	if !slices.Equal(waits, want) {
		t.Fatalf("waits = %+v, want %+v", waits, want)
	}
}

// TestDrainWorkflowServeWorkProcessesLiveControlBesideStaleRow is AC-3: a
// processable control beside a persistent stale row is processed once, and
// the skip-only passes after it take the bounded idle re-polls, not a spin.
func TestDrainWorkflowServeWorkProcessesLiveControlBesideStaleRow(t *testing.T) {
	cityDir, store := setUpServeSkipCity(t)
	workflowServeIdlePollAttempts = 3
	stale := createNonOpenScopeCheck(t, store, "closed")
	live := createOrphanedScopeCheck(t, store)
	lists := stubServeQueue(t, func() []hookBead {
		rows := []hookBead{serveQueueRow(stale)}
		if b, err := store.Get(live.ID); err == nil && b.Status == "open" {
			rows = append(rows, serveQueueRow(b))
		}
		return rows
	})
	dispatches := countServeDispatches(t)

	result := drainServeSkipCity(t, cityDir)

	if !result.processedAny {
		t.Error("processedAny = false, want true after processing the live control")
	}
	if got, err := store.Get(live.ID); err != nil || got.Status != "closed" {
		t.Errorf("live control = (%+v, %v), want closed by its dispatch", got, err)
	}
	if !dispatchWakeFileExists(t, cityDir) {
		t.Error("dispatch wake file not written after processing the live control")
	}
	if got := dispatches[live.ID]; got != 1 {
		t.Errorf("live control dispatched %d times, want 1", got)
	}
	wantPasses := 2 + workflowServeIdlePollAttempts
	if *lists != wantPasses {
		t.Errorf("queue queried %d times, want %d (one processing pass, one skip-only pass, %d idle re-polls)", *lists, wantPasses, workflowServeIdlePollAttempts)
	}
	if got := dispatches[stale.ID]; got != wantPasses {
		t.Errorf("stale control dispatched %d times, want %d", got, wantPasses)
	}
}

// TestDrainWorkflowServeWorkKeepsIdleRepollsAfterProgress pins that an empty
// queue after a processed pass still takes every idle re-poll, the
// read-after-write allowance the skip-only exit shares.
func TestDrainWorkflowServeWorkKeepsIdleRepollsAfterProgress(t *testing.T) {
	cityDir, store := setUpServeSkipCity(t)
	workflowServeIdlePollAttempts = 3
	live := createOrphanedScopeCheck(t, store)
	lists := stubServeQueue(t, func() []hookBead {
		if b, err := store.Get(live.ID); err == nil && b.Status == "open" {
			return []hookBead{serveQueueRow(b)}
		}
		return nil
	})
	countServeDispatches(t)

	result := drainServeSkipCity(t, cityDir)

	if !result.processedAny {
		t.Error("processedAny = false, want true")
	}
	if want := 2 + workflowServeIdlePollAttempts; *lists != want {
		t.Errorf("queue queried %d times, want %d", *lists, want)
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

// TestRunControlDispatcherNotOpenSkipContract is AC-4: the one-shot CLI path
// keeps returning nil silently for a non-open control, while the serve seam
// reports the no-op as errControlNotProcessed and still closes its store.
func TestRunControlDispatcherNotOpenSkipContract(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	oneShot := newCloseCountingStore(t, false)
	closedOneShot := createNonOpenScopeCheck(t, oneShot, "closed")
	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityDir, cityDir, oneShot, closedOneShot.ID, nil, &stdout, &stderr); err != nil {
		t.Fatalf("one-shot dispatch of a closed control: err = %v, want nil (stderr=%q)", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("one-shot stdout = %q, want empty for a skipped control", stdout.String())
	}

	seam := newCloseCountingStore(t, false)
	closedSeam := createNonOpenScopeCheck(t, seam, "closed")
	prev := openControlStoreForDispatch
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) { return seam, nil }
	t.Cleanup(func() { openControlStoreForDispatch = prev })
	stdout.Reset()
	stderr.Reset()
	err := runControlDispatcherInStore(cityDir, cityDir, closedSeam.ID, &stdout, &stderr, nil)
	if !errors.Is(err, errControlNotProcessed) {
		t.Fatalf("serve seam dispatch of a closed control: err = %v, want errControlNotProcessed (stderr=%q)", err, stderr.String())
	}
	if got := seam.closes(); got != 1 {
		t.Fatalf("scope store closed %d times, want 1", got)
	}
}

// TestDrainWorkflowServeWorkCountsQuarantineAsProgress is AC-5: a control the
// dispatcher quarantines returns nil without Processed, and that close is a
// write the drain must keep counting as progress.
func TestDrainWorkflowServeWorkCountsQuarantineAsProgress(t *testing.T) {
	cityDir, store := setUpServeSkipCity(t)
	workflowServeIdlePollAttempts = 0
	bad, err := store.Create(beads.Bead{
		Type: "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            "not-a-control-kind",
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
	})
	if err != nil {
		t.Fatalf("create unsupported control: %v", err)
	}
	stubServeQueue(t, func() []hookBead {
		if b, err := store.Get(bad.ID); err == nil && b.Status == "open" {
			return []hookBead{serveQueueRow(b)}
		}
		return nil
	})
	dispatches := countServeDispatches(t)

	result := drainServeSkipCity(t, cityDir)

	got, err := store.Get(bad.ID)
	if err != nil {
		t.Fatalf("get control: %v", err)
	}
	if got.Status != "closed" || got.Metadata[beadmeta.ControlQuarantinedMetadataKey] != "true" {
		t.Fatalf("control = status %q, metadata %v; want closed and quarantined", got.Status, got.Metadata)
	}
	if dispatches[bad.ID] != 1 {
		t.Errorf("control dispatched %d times, want 1", dispatches[bad.ID])
	}
	if !result.processedAny {
		t.Error("processedAny = false, want true: a quarantine close is progress")
	}
	if !dispatchWakeFileExists(t, cityDir) {
		t.Error("dispatch wake file not written after a quarantine")
	}
}
