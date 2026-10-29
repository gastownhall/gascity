package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
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
