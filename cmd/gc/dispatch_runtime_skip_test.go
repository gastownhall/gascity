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

// TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps pins the third impact of
// #6024: a --follow drain that only ever skips a stale non-open row used to
// report processedAny on every sweep, pinning the idle backoff at its 1s floor
// forever. Skip-only sweeps are idle sweeps: the waits must see idleSweeps
// 0, 1, 2, 3 with sleeps 1s, 2s, 4s and 5s (capped at workflowServeMaxIdleSleep).
func TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps(t *testing.T) {
	eventsDir := t.TempDir()
	ep := newTestProvider(t, eventsDir)

	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)
	stale := createRoutedControl(t, store, beadmeta.KindScopeCheck)
	if err := store.Close(stale.ID); err != nil {
		t.Fatalf("close control bead: %v", err)
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
			t.Fatalf("serve scanned the queue %d times, want one scan per sweep", scans)
		}
		return []hookBead{{ID: stale.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}}}, nil
	}

	type waitCall struct {
		idleSweeps int
		sleepDur   time.Duration
	}
	var waitCalls []waitCall
	stopErr := fmt.Errorf("stop after sequence")
	workflowServeWaitForWake = func(_ <-chan workflowWatchResult, sleepDur time.Duration, idleSweeps int) (bool, error) {
		waitCalls = append(waitCalls, waitCall{idleSweeps: idleSweeps, sleepDur: sleepDur})
		if len(waitCalls) == 4 {
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
	if !slices.Equal(waitCalls, want) {
		t.Fatalf("wait calls = %#v, want %#v: skip-only sweeps must engage the idle backoff", waitCalls, want)
	}
	if scans != len(want) {
		t.Errorf("serve scanned the queue %d times across %d sweeps, want one scan per sweep", scans, len(want))
	}
}

// TestDrainWorkflowServeWorkProcessesLiveControlBesideStaleRow pins that the
// skip-aware exit keeps the freshness guarantee a processed pass buys: after
// real work beside a stale row, the drain still takes the same bounded
// skip-only re-polls the empty-queue path takes (workflowServeIdlePollAttempts),
// so a minted successor is picked up without waiting for the --follow sleep.
// The stale bead is dispatched exactly 2 + workflowServeIdlePollAttempts
// times: once in the processed pass, once per re-poll, once in the exit scan.
func TestDrainWorkflowServeWorkProcessesLiveControlBesideStaleRow(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)
	ensureDispatchWakeDir(t, cityDir)

	// The live control is the leak test's orphaned scope-check: its root is
	// missing, so dispatching it closes it as orphaned-workflow — a real
	// processed write beside the stale row. scope-check needs no city-config
	// resolution.
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
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 3
	t.Cleanup(func() {
		workflowServeList = prevList
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	scans := 0
	workflowServeList = func(_, _ string, _ map[string]string) ([]hookBead, error) {
		scans++
		if scans > 2+workflowServeIdlePollAttempts+1 {
			t.Fatalf("drain scanned the queue %d times, want %d", scans, 2+workflowServeIdlePollAttempts+1)
		}
		queue := []hookBead{{ID: stale.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}}}
		liveNow, err := store.Get(live.ID)
		if err != nil {
			return nil, err
		}
		if liveNow.Status == "open" {
			queue = append([]hookBead{{ID: live.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}}}, queue...)
		}
		return queue, nil
	}

	result, err := drainWorkflowServeWork(freshnessControlAgent(), cityDir, cityDir, workflowServeControlReadyQuery(freshnessControlAgent()), nil, io.Discard)
	if err != nil {
		t.Fatalf("drainWorkflowServeWork: %v", err)
	}
	if !result.processedAny {
		t.Fatal("processedAny = false, want the live control processed")
	}
	wantScans := 2 + workflowServeIdlePollAttempts
	if scans != wantScans {
		t.Errorf("drain scanned the queue %d times, want %d (one processed pass, %d skip-only re-polls, one exit scan), so the stale bead is dispatched exactly %d times",
			scans, wantScans, workflowServeIdlePollAttempts, wantScans)
	}
	if _, statErr := os.Stat(dispatchWakeFile(cityDir)); statErr != nil {
		t.Errorf("dispatch wake file missing after a pass that processed the live control: %v", statErr)
	}
	liveNow, err := store.Get(live.ID)
	if err != nil {
		t.Fatalf("reload live control: %v", err)
	}
	if liveNow.Status != "closed" {
		t.Errorf("live control status = %s, want closed (dispatched as orphaned-workflow)", liveNow.Status)
	}
}

// TestRunControlDispatcherNotOpenSkipContract pins both dispatch entry points
// for a control bead whose status is not open. The one-shot `gc convoy control
// <id>` path must succeed silently, exactly as it did before the skip crossed
// the seam. The serve seam must report the no-op (errControlNotProcessed) so
// the drain can count it as a skip, and must close the scope store it opened
// exactly once on the way out.
func TestRunControlDispatcherNotOpenSkipContract(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	fake := newCloseCountingStore(t, false)
	stale, err := fake.Create(beads.Bead{
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck},
	})
	if err != nil {
		t.Fatalf("create control bead: %v", err)
	}
	if err := fake.Close(stale.ID); err != nil {
		t.Fatalf("close control bead: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := runControlDispatcherWithStoreAndConfig(cityDir, cityDir, fake, stale.ID, nil, &stdout, &stderr); err != nil {
		t.Fatalf("one-shot dispatch of a not-open control: %v (stderr=%q)", err, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("one-shot dispatch wrote %q, want no output for a no-op", stdout.String())
	}

	prev := openControlStoreForDispatch
	openControlStoreForDispatch = func(_, _ string, _ *config.City) (beads.Store, error) {
		return fake, nil
	}
	t.Cleanup(func() { openControlStoreForDispatch = prev })

	seamErr := runControlDispatcherInStore(cityDir, cityDir, stale.ID, io.Discard, io.Discard, nil)
	if !errors.Is(seamErr, errControlNotProcessed) {
		t.Fatalf("serve-seam dispatch of a not-open control = %v, want errControlNotProcessed so the drain can count it as a skip", seamErr)
	}
	if got := fake.closes(); got != 1 {
		t.Fatalf("scope store closed %d times, want 1: the no-op path must not leak the opened store", got)
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
