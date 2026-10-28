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

// serveQueueCallCap bounds the stubbed serve queue so a drain that spins on a
// no-op dispatch fails fast instead of hanging the test.
const serveQueueCallCap = 20

// newSkipDispatchCity writes a minimal city with its .gc directory and routes
// the production dispatch's scope-store opens to store. The .gc directory has
// to exist up front: writeDispatchWakeFile gives up when it is missing, which
// would make every "no wake file" assertion vacuous.
func newSkipDispatchCity(t *testing.T, store beads.Store) string {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("create .gc: %v", err)
	}
	prev := openControlStoreForDispatch
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) {
		return store, nil
	}
	t.Cleanup(func() { openControlStoreForDispatch = prev })
	return cityDir
}

// createNonOpenScopeCheck seeds a scope-check control bead (no city-config
// resolution) and moves it to status, the shape of a stale ready-queue row.
func createNonOpenScopeCheck(t *testing.T, store beads.Store, status string) beads.Bead {
	t.Helper()
	control, err := store.Create(beads.Bead{
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck},
	})
	if err != nil {
		t.Fatalf("seed control bead: %v", err)
	}
	if status == "closed" {
		err = store.Close(control.ID)
	} else {
		err = store.Update(control.ID, beads.UpdateOpts{Status: &status})
	}
	if err != nil {
		t.Fatalf("set control bead %s to %s: %v", control.ID, status, err)
	}
	got, err := store.Get(control.ID)
	if err != nil {
		t.Fatalf("read control bead %s: %v", control.ID, err)
	}
	if got.Status != status {
		t.Fatalf("premise: control bead %s status = %q, want %q", control.ID, got.Status, status)
	}
	return got
}

// stubServeQueue installs a workflowServeList that answers queue() on every
// call, failing the test once the drain queries more than serveQueueCallCap
// times. It returns the call counter.
func stubServeQueue(t *testing.T, queue func() []hookBead) *int {
	t.Helper()
	prev := workflowServeList
	t.Cleanup(func() { workflowServeList = prev })
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		if calls > serveQueueCallCap {
			t.Fatalf("serve queue queried %d times: the drain re-queried with no bound", calls)
		}
		return queue(), nil
	}
	return &calls
}

// countServeDispatches wraps the production controlDispatcherServe and counts
// dispatches per bead ID.
func countServeDispatches(t *testing.T) map[string]int {
	t.Helper()
	prev := controlDispatcherServe
	t.Cleanup(func() { controlDispatcherServe = prev })
	counts := map[string]int{}
	controlDispatcherServe = func(cityPath, storePath, beadID string, stdout, stderr io.Writer, emits *executionEmitDeferral) error {
		counts[beadID]++
		return prev(cityPath, storePath, beadID, stdout, stderr, emits)
	}
	return counts
}

func controlQueueRow(b beads.Bead) hookBead {
	return hookBead{ID: b.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: b.Metadata[beadmeta.KindMetadataKey]}}
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

// TestRunControlDispatcherNotOpenSkipContract pins both entry points on a
// control bead that is not open: the one-shot `gc convoy control <id>` path
// keeps returning nil with no output, while the serve seam reports the no-op as
// errControlNotProcessed so the drain can tell it from progress, and still
// closes the scope store it opened.
func TestRunControlDispatcherNotOpenSkipContract(t *testing.T) {
	store := newCloseCountingStore(t, false)
	cityDir := newSkipDispatchCity(t, store)
	stale := createNonOpenScopeCheck(t, store, "closed")

	var oneShotOut, oneShotErr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityDir, cityDir, store, stale.ID, nil, &oneShotOut, &oneShotErr); err != nil {
		t.Fatalf("one-shot dispatch of a closed control bead: err = %v, want nil (stderr=%q)", err, oneShotErr.String())
	}
	if oneShotOut.Len() != 0 {
		t.Fatalf("one-shot dispatch of a closed control bead wrote stdout %q, want nothing", oneShotOut.String())
	}

	var seamOut, seamErr bytes.Buffer
	err := runControlDispatcherInStore(cityDir, cityDir, stale.ID, &seamOut, &seamErr, nil)
	if !errors.Is(err, errControlNotProcessed) {
		t.Fatalf("serve seam dispatch of a closed control bead: err = %v, want errControlNotProcessed (stderr=%q)", err, seamErr.String())
	}
	if seamOut.Len() != 0 {
		t.Fatalf("serve seam dispatch of a closed control bead wrote stdout %q, want nothing", seamOut.String())
	}
	if got := store.closes(); got != 1 {
		t.Fatalf("scope store closed %d times, want 1: the skip path must close the store the seam opened", got)
	}
}

// TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning is issue #6024:
// a control bead that is no longer open but still offered by the ready queue
// must not count as drain progress. Counted as progress, it re-queried the
// store with no sleep and no bound, rewrote the dispatch wake file on every
// pass, and reported processedAny so the --follow backoff never engaged.
func TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning(t *testing.T) {
	for _, status := range []string{"closed", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			store := beads.NewMemStore()
			cityDir := newSkipDispatchCity(t, store)
			stale := createNonOpenScopeCheck(t, store, status)
			listCalls := stubServeQueue(t, func() []hookBead { return []hookBead{controlQueueRow(stale)} })
			dispatches := countServeDispatches(t)

			result, err := drainWorkflowServeWork(config.Agent{Name: "control-dispatcher"}, cityDir, cityDir, "bd ready", nil, io.Discard)
			if err != nil {
				t.Fatalf("drainWorkflowServeWork: %v", err)
			}
			if *listCalls != 1 {
				t.Errorf("serve queue queried %d times, want 1", *listCalls)
			}
			if got := dispatches[stale.ID]; got != 1 {
				t.Errorf("%s control bead dispatched %d times, want 1", status, got)
			}
			if result.processedAny || result.pendingAny {
				t.Errorf("drain result = %+v, want neither processed nor pending for a skip-only drain", result)
			}
			if dispatchWakeFileExists(t, cityDir) {
				t.Errorf("dispatch wake file written for a drain that only skipped a %s control bead", status)
			}
			got, err := store.Get(stale.ID)
			if err != nil {
				t.Fatalf("read control bead: %v", err)
			}
			if got.Status != status {
				t.Errorf("control bead status = %q after the skip, want it left %q", got.Status, status)
			}
		})
	}
}

// TestDrainWorkflowServeWorkSkipBesidePendingReportsPending: a pass holding a
// skipped control bead and a pending one still ends through the pending-queue
// exit and reports pendingAny; the skip only stops counting as progress.
func TestDrainWorkflowServeWorkSkipBesidePendingReportsPending(t *testing.T) {
	clearGCEnv(t)
	listCalls := stubServeQueue(t, func() []hookBead {
		return []hookBead{
			{ID: "gc-stale", Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}},
			{ID: "gc-pending", Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: "retry-eval"}},
		}
	})
	prevControl := controlDispatcherServe
	t.Cleanup(func() { controlDispatcherServe = prevControl })
	controlDispatcherServe = func(_, _, beadID string, _, _ io.Writer, _ *executionEmitDeferral) error {
		if beadID == "gc-pending" {
			return dispatch.ErrControlPending
		}
		return errControlNotProcessed
	}

	cityDir := t.TempDir()
	result, err := drainWorkflowServeWork(config.Agent{Name: "control-dispatcher"}, cityDir, cityDir, "bd ready", nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if *listCalls != 1 {
		t.Errorf("serve queue queried %d times, want 1", *listCalls)
	}
	if result.processedAny || !result.pendingAny {
		t.Errorf("drain result = %+v, want pending and not processed", result)
	}
}

// TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps: a --follow loop whose
// every drain sees only a stale non-open control bead is idle, so its sleep
// doubles from the 1s floor up to workflowServeMaxIdleSleep.
func TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps(t *testing.T) {
	store := beads.NewMemStore()
	cityDir := newSkipDispatchCity(t, store)
	stale := createNonOpenScopeCheck(t, store, "closed")
	stubServeQueue(t, func() []hookBead { return []hookBead{controlQueueRow(stale)} })
	dispatches := countServeDispatches(t)

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
	var waitCalls []waitCall
	stopErr := fmt.Errorf("stop after four sweeps")
	workflowServeWaitForWake = func(_ <-chan workflowWatchResult, sleepDur time.Duration, idleSweeps int) (bool, error) {
		waitCalls = append(waitCalls, waitCall{idleSweeps: idleSweeps, sleepDur: sleepDur})
		if len(waitCalls) == 4 {
			return false, stopErr
		}
		return false, nil
	}

	err := runWorkflowServeFollow(config.Agent{Name: "control-dispatcher"}, cityDir, cityDir, "bd ready", nil, io.Discard)
	if !errors.Is(err, stopErr) {
		t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, stopErr)
	}
	want := []waitCall{
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 1, sleepDur: 2 * time.Second},
		{idleSweeps: 2, sleepDur: 4 * time.Second},
		{idleSweeps: 3, sleepDur: workflowServeMaxIdleSleep},
	}
	if !slices.Equal(waitCalls, want) {
		t.Fatalf("wait calls = %+v, want %+v", waitCalls, want)
	}
	if got := dispatches[stale.ID]; got != 4 {
		t.Fatalf("closed control bead dispatched %d times, want once per sweep (4)", got)
	}
}
