package main

import (
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

// serveSkipListCap bounds how many times a test's workflowServeList stub
// answers before failing, so a drain that re-queries a skipped bead with no
// sleep (gastownhall/gascity#6024) fails fast instead of hanging the shard.
const serveSkipListCap = 20

// serveSkipCity is a city whose serve drain runs the production
// controlDispatcherServe against an in-memory scope store.
type serveSkipCity struct {
	dir   string
	store *beads.MemStore
	// dispatches counts production controlDispatcherServe calls per bead.
	dispatches map[string]int
}

// newServeSkipCity builds the shared fixture: a city.toml, a .gc directory so
// writeDispatchWakeFile can actually create the wake file (a "no wake file"
// assertion is vacuous without it), and openControlStoreForDispatch stubbed to
// one MemStore. controlDispatcherServe stays the production seam; it is only
// wrapped to count dispatches.
func newServeSkipCity(t *testing.T) *serveSkipCity {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("create .gc: %v", err)
	}
	city := &serveSkipCity{dir: dir, store: beads.NewMemStore(), dispatches: map[string]int{}}

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
		return city.store, nil
	}
	controlDispatcherServe = func(cityPath, storePath, beadID string, stdout, stderr io.Writer, emits *executionEmitDeferral) error {
		city.dispatches[beadID]++
		return runControlDispatcherInStore(cityPath, storePath, beadID, stdout, stderr, emits)
	}
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 3
	return city
}

// createStaleControl creates a scope-check control (no city-config
// resolution) and moves it to status, so ProcessControl takes its not-open
// skip.
func (c *serveSkipCity) createStaleControl(t *testing.T, status string) beads.Bead {
	t.Helper()
	control, err := c.store.Create(beads.Bead{
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck},
	})
	if err != nil {
		t.Fatalf("create control bead: %v", err)
	}
	if err := c.store.Update(control.ID, beads.UpdateOpts{Status: &status}); err != nil {
		t.Fatalf("set control bead %s status %q: %v", control.ID, status, err)
	}
	return control
}

// stubQueue makes workflowServeList answer rows() on every call, failing the
// test past serveSkipListCap, and returns the call counter.
func stubQueue(t *testing.T, rows func() []hookBead) *int {
	t.Helper()
	calls := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		calls++
		if calls > serveSkipListCap {
			t.Fatalf("serve queue queried %d times: the drain re-queried a skipped control bead without bound", calls)
		}
		return rows(), nil
	}
	return &calls
}

func hookRow(b beads.Bead) hookBead {
	return hookBead{ID: b.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: b.Metadata[beadmeta.KindMetadataKey]}}
}

func wakeFileExists(t *testing.T, cityDir string) bool {
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

// TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning is #6024: a
// queue row whose control bead is no longer open is a ProcessControl no-op,
// and the drain must not count it as progress — no immediate re-query, no
// dispatch wake file, no processedAny to pin the --follow backoff.
func TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning(t *testing.T) {
	for _, status := range []string{"closed", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			city := newServeSkipCity(t)
			stale := city.createStaleControl(t, status)
			calls := stubQueue(t, func() []hookBead { return []hookBead{hookRow(stale)} })

			agent := config.Agent{Name: "control-dispatcher"}
			result, err := drainWorkflowServeWork(agent, city.dir, city.dir, "bd ready", nil, io.Discard)
			if err != nil {
				t.Fatalf("drainWorkflowServeWork: %v", err)
			}
			if *calls != 1 {
				t.Errorf("serve queue queried %d times, want 1", *calls)
			}
			if got := city.dispatches[stale.ID]; got != 1 {
				t.Errorf("%s control dispatched %d times, want 1", status, got)
			}
			if result.processedAny || result.pendingAny {
				t.Errorf("drain result = %+v, want neither processed nor pending for a skip-only drain", result)
			}
			if wakeFileExists(t, city.dir) {
				t.Errorf("dispatch wake file written for a drain that only skipped a %s control bead", status)
			}
		})
	}
}

// TestDrainWorkflowServeWorkPendingWinsOverSkip pins the exit order of a pass
// that neither processed anything: a pending row still returns through the
// pending-queue path with pendingAny set, whatever skips sit beside it.
func TestDrainWorkflowServeWorkPendingWinsOverSkip(t *testing.T) {
	prevList := workflowServeList
	prevServe := controlDispatcherServe
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevServe
	})
	calls := stubQueue(t, func() []hookBead {
		return []hookBead{
			{ID: "gc-stale", Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindWorkflowFinalize}},
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
	if *calls != 1 {
		t.Errorf("serve queue queried %d times, want 1", *calls)
	}
	if result.processedAny || !result.pendingAny {
		t.Errorf("drain result = %+v, want pendingAny only", result)
	}
}

// TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps pins the third #6024
// impact: drains that see only a skipped control bead are idle sweeps, so the
// --follow sleep doubles up to workflowServeMaxIdleSleep.
func TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps(t *testing.T) {
	city := newServeSkipCity(t)
	stale := city.createStaleControl(t, "closed")
	stubQueue(t, func() []hookBead { return []hookBead{hookRow(stale)} })

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

	agent := config.Agent{Name: "control-dispatcher"}
	if err := runWorkflowServeFollow(agent, city.dir, city.dir, "bd ready", nil, io.Discard); !errors.Is(err, stopErr) {
		t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, stopErr)
	}
	want := []waitCall{
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 1, sleepDur: 2 * time.Second},
		{idleSweeps: 2, sleepDur: 4 * time.Second},
		{idleSweeps: 3, sleepDur: 5 * time.Second},
	}
	if !slices.Equal(waits, want) {
		t.Fatalf("wait calls = %+v, want %+v", waits, want)
	}
}
