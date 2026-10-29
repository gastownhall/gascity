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
	"github.com/gastownhall/gascity/internal/events"
)

// stubWorkflowServeIdlePolling pins the drain's idle re-poll knobs for the
// duration of a test and restores them on cleanup: the re-poll interval drops
// to zero so the bounded re-polls stay instant, and attempts is the caller's
// choice.
func stubWorkflowServeIdlePolling(t *testing.T, attempts int) {
	t.Helper()
	prevInterval, prevAttempts := workflowServeIdlePollInterval, workflowServeIdlePollAttempts
	workflowServeIdlePollInterval, workflowServeIdlePollAttempts = 0, attempts
	t.Cleanup(func() {
		workflowServeIdlePollInterval, workflowServeIdlePollAttempts = prevInterval, prevAttempts
	})
}

// stubWorkflowServeList replaces the serve queue seam for the duration of a
// test.
func stubWorkflowServeList(t *testing.T, fn func(query, storePath string, env map[string]string) ([]hookBead, error)) {
	t.Helper()
	prev := workflowServeList
	workflowServeList = fn
	t.Cleanup(func() { workflowServeList = prev })
}

// ensureDispatchWakeDir creates <city>/.gc so writeDispatchWakeFile writes the
// real file instead of taking its missing-dir early return; without it a "no
// wake file" assertion is vacuous.
func ensureDispatchWakeDir(t *testing.T, cityDir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("create .gc: %v", err)
	}
}

// TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning is the AC-1 pin
// for gastownhall/gascity#6024: a queue row for a control bead whose status is
// not open makes ProcessControl no-op with a nil error, and the drain used to
// count that nil as progress — re-querying the queue with no sleep, writing
// the dispatch wake file on every pass, and reporting processedAny so the
// --follow idle backoff could never engage. A skip must leave the queue
// queried once and the bead dispatched once, counting as neither progress nor
// pending.
func TestDrainWorkflowServeWorkSkipsNonOpenControlWithoutSpinning(t *testing.T) {
	for _, status := range []string{"closed", "in_progress"} {
		t.Run(status, func(t *testing.T) {
			cityDir, store := setUpControlReadyFileStoreCity(t)
			noBDOnPathForTest(t)
			ensureDispatchWakeDir(t, cityDir)

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

			stubWorkflowServeIdlePolling(t, 0)
			const maxScans = 20
			scans := 0
			stubWorkflowServeList(t, func(_, _ string, _ map[string]string) ([]hookBead, error) {
				scans++
				if scans > maxScans {
					// Stop offering the row so a regressed drain (which
					// counts the skip as progress and re-queries with no
					// sleep) fails fast on the assertions below instead of
					// spinning.
					return nil, nil
				}
				return []hookBead{{ID: control.ID, Metadata: hookBeadMetadata{beadmeta.KindMetadataKey: beadmeta.KindScopeCheck}}}, nil
			})

			result, err := drainWorkflowServeWork(freshnessControlAgent(), cityDir, cityDir, workflowServeControlReadyQuery(freshnessControlAgent()), nil, io.Discard)
			if err != nil {
				t.Fatalf("drainWorkflowServeWork: %v", err)
			}
			if scans != 1 {
				t.Errorf("queue scanned %d times for one non-open (%s) control bead, want 1: a bead_not_open skip must not re-query with no sleep", scans, status)
			}
			if result.processedAny {
				t.Errorf("processedAny = true for a skip-only drain (%s), want false: the --follow idle backoff can never engage", status)
			}
			if result.pendingAny {
				t.Errorf("pendingAny = true for a skip-only drain (%s), want false", status)
			}
			if _, statErr := os.Stat(dispatchWakeFile(cityDir)); statErr == nil {
				t.Errorf("dispatch wake file written for a skip-only drain (%s), want none", status)
			}
		})
	}
}

// TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps is the AC-2 pin: a drain
// whose queue holds only a skipped control bead must report neither processed
// nor pending, so runWorkflowServeFollow grows its idle backoff (1s, 2s, 4s,
// then capped at workflowServeMaxIdleSleep) instead of sitting at the 1s
// floor (gastownhall/gascity#6024 impact 3).
func TestRunWorkflowServeFollowBacksOffOnSkipOnlySweeps(t *testing.T) {
	eventsDir := t.TempDir()
	ep := newTestProvider(t, eventsDir)

	prevProvider, prevWait := workflowServeOpenEventsProvider, workflowServeWaitForWake
	t.Cleanup(func() {
		workflowServeOpenEventsProvider = prevProvider
		workflowServeWaitForWake = prevWait
	})
	// A skip-only drain must exit on its first pass; the re-poll knobs only
	// matter to the processed path.
	stubWorkflowServeIdlePolling(t, 0)

	workflowServeOpenEventsProvider = func(io.Writer) (events.Provider, error) { return ep, nil }

	type waitCall struct {
		idleSweeps int
		sleepDur   time.Duration
	}
	var waitCalls []waitCall
	stopErr := fmt.Errorf("stop after sequence")
	workflowServeWaitForWake = func(_ <-chan workflowWatchResult, sleepDur time.Duration, idleSweeps int) (bool, error) {
		waitCalls = append(waitCalls, waitCall{idleSweeps: idleSweeps, sleepDur: sleepDur})
		if len(waitCalls) >= 4 {
			return false, stopErr
		}
		return false, nil
	}

	stubWorkflowServeList(t, func(_, _ string, _ map[string]string) ([]hookBead, error) {
		return []hookBead{{ID: "gc-stale", Metadata: hookBeadMetadata{"gc.kind": "workflow-finalize"}}}, nil
	})
	prevServe := controlDispatcherServe
	controlDispatcherServe = func(_, _ string, _ string, _, _ io.Writer, _ *executionEmitDeferral) error {
		return errControlNotProcessed
	}
	t.Cleanup(func() { controlDispatcherServe = prevServe })

	agent := config.Agent{Name: "control-dispatcher"}
	err := runWorkflowServeFollow(agent, t.TempDir(), t.TempDir(), agent.EffectiveWorkQuery(), nil, io.Discard)
	if !errors.Is(err, stopErr) {
		t.Fatalf("runWorkflowServeFollow error = %v, want %v", err, stopErr)
	}
	want := []waitCall{
		{idleSweeps: 0, sleepDur: 1 * time.Second},
		{idleSweeps: 1, sleepDur: 2 * time.Second},
		{idleSweeps: 2, sleepDur: 4 * time.Second},
		{idleSweeps: 3, sleepDur: 5 * time.Second}, // capped at workflowServeMaxIdleSleep
	}
	if !slices.Equal(waitCalls, want) {
		t.Fatalf("wait calls = %#v, want %#v: skip-only sweeps must grow the idle backoff", waitCalls, want)
	}
}
