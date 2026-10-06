package dispatch

import (
	"bytes"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// newTerminalRootRetryControl builds the minimal graph the retry dispatcher
// needs to spawn attempt 2: a workflow root, a retry control, and a closed
// attempt 1 that classifies transient (the INFRA_TIMEOUT shape from
// gastownhall/gascity#5389). extraControlMetadata is merged onto the control.
func newTerminalRootRetryControl(t *testing.T, store beads.Store, rootOutcome string, extraControlMetadata map[string]string) (control beads.Bead) {
	t.Helper()

	root := mustCreate(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "molecule",
		Metadata: map[string]string{"gc.kind": "workflow"},
	})
	if rootOutcome != "" {
		if _, err := store.CloseAll([]string{root.ID}, map[string]string{"gc.outcome": rootOutcome}); err != nil {
			t.Fatalf("close root %s: %v", rootOutcome, err)
		}
	}

	controlMetadata := map[string]string{
		"gc.kind":             "retry",
		"gc.root_bead_id":     root.ID,
		"gc.root_store_ref":   "rig:gascity",
		"gc.step_ref":         "mol-test.finalize",
		"gc.step_id":          "finalize",
		"gc.max_attempts":     "5",
		"gc.on_exhausted":     "hard_fail",
		"gc.source_step_spec": `{"id":"finalize","title":"Finalize","type":"task","retry":{"max_attempts":5}}`,
		"gc.control_epoch":    "1",
	}
	for key, value := range extraControlMetadata {
		controlMetadata[key] = value
	}
	control = mustCreate(t, store, beads.Bead{Title: "finalize", Metadata: controlMetadata})

	attempt := mustCreate(t, store, beads.Bead{
		Title: "finalize attempt 1",
		Metadata: map[string]string{
			"gc.root_bead_id":   root.ID,
			"gc.step_ref":       "mol-test.finalize.attempt.1",
			"gc.attempt":        "1",
			"gc.outcome":        "fail",
			"gc.failure_class":  "transient",
			"gc.failure_reason": "INFRA_TIMEOUT",
		},
	})
	mustClose(t, store, attempt.ID)
	mustDep(t, store, control.ID, attempt.ID, "blocks")
	return control
}

// TestProcessControlStopsControlWhenWorkflowRootTerminallyClosed pins the
// terminal-root gate reported as gastownhall/gascity#5389: once a workflow root
// has recorded a terminal gc.outcome and closed_at, control beads under it must
// stop spawning work.
//
// processWorkflowFinalize closes the root (runtime.go) BEFORE its bulk
// CloseSubtreeWithMetadataExcept sweep, deliberately, so a crash leaves the
// finalizer open to retry. That sweep is all-or-nothing: under store contention
// it fails wholesale and the finalizer returns early, leaving the root closed
// and every control under it still open. Without this gate those controls keep
// being dispatched and keep minting attempts — the reporter measured a
// downstream step still retrying 5 hours after its root recorded gc.outcome=fail.
// The gate is per-control and idempotent, so settlement converges even when the
// bulk sweep never succeeds.
func TestProcessControlStopsControlWhenWorkflowRootTerminallyClosed(t *testing.T) {
	t.Parallel()

	for _, rootOutcome := range []string{"fail", "pass", "skipped"} {
		t.Run(rootOutcome, func(t *testing.T) {
			t.Parallel()
			store := beads.NewMemStore()
			control := newTerminalRootRetryControl(t, store, rootOutcome, nil)

			var traceBuf bytes.Buffer
			opts := ProcessOptions{Tracef: func(format string, args ...any) {
				fmt.Fprintf(&traceBuf, format, args...)
				traceBuf.WriteByte('\n')
			}}

			result, err := ProcessControl(store, mustGet(t, store, control.ID), opts)
			if err != nil {
				t.Fatalf("ProcessControl: %v", err)
			}
			if !result.Processed || result.Action != "settled-workflow" {
				t.Fatalf("result = %+v, want processed settled-workflow", result)
			}
			if result.Created != 0 {
				t.Fatalf("created %d bead(s) under a terminally closed root, want 0", result.Created)
			}

			after := mustGet(t, store, control.ID)
			if after.Status != "closed" {
				t.Fatalf("control status = %q, want closed", after.Status)
			}
			// Residue closed by the gate must be indistinguishable from residue
			// closed by the finalizer's own sweep, which stamps skipped.
			if got := after.Metadata["gc.outcome"]; got != "skipped" {
				t.Fatalf("gc.outcome = %q, want skipped", got)
			}
			// Residue is not a failure: stamping fail here would pollute
			// outcome aggregation with beads that never ran.
			if got := after.Metadata["gc.failure_reason"]; got != "" {
				t.Fatalf("gc.failure_reason = %q, want empty", got)
			}
			if got := after.Metadata["gc.failure_class"]; got != "" {
				t.Fatalf("gc.failure_class = %q, want empty", got)
			}
			if !bytes.Contains(traceBuf.Bytes(), []byte("close reason=root_settled")) {
				t.Fatalf("trace missing root_settled close reason; got:\n%s", traceBuf.String())
			}
		})
	}
}

// TestProcessControlKeepsTeardownTailRunningUnderCanceledRoot mirrors
// TestProcessControlKeepsTeardownTailRunningUnderTerminalRoot for the
// canceled-root arm: cancellation is a terminal state like any other, so the
// teardown tail's contract (it runs AFTER the root reaches a terminal state,
// its pass condition may branch on ROOT_OUTCOME) applies just the same. Before
// this exemption, cancelRun's blanket subtree close force-closed the teardown
// tail as canceled and it never executed.
func TestProcessControlKeepsTeardownTailRunningUnderCanceledRoot(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	control := newTerminalRootRetryControl(t, store, "canceled", map[string]string{
		"gc.scope_ref":  "teardown",
		"gc.scope_role": "teardown",
	})

	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if result.Action != "retry" || result.Created != 1 {
		t.Fatalf("result = %+v, want the teardown tail to keep running under a canceled root (retry, created 1)", result)
	}
	if after := mustGet(t, store, control.ID); after.Status != "open" {
		t.Fatalf("teardown control status = %q, want open", after.Status)
	}
}

// TestProcessControlKeepsTeardownTailRunningUnderTerminalRoot guards the
// contract #5271 established: a teardown step's control runs AFTER the root
// settles by design — its pass condition may branch on ROOT_OUTCOME, which only
// finalize produces. molecule.TeardownTailExclusion keeps that tail out of the
// finalizer's own sweep for the same reason, so the terminal-root gate must
// exempt it too or the adopt-pr settlement deadlock comes straight back.
//
// expandRetry mints a retry control via cloneStep, so the control inherits the
// host step's gc.scope_role; only the first attempt strips it.
func TestProcessControlKeepsTeardownTailRunningUnderTerminalRoot(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	control := newTerminalRootRetryControl(t, store, "fail", map[string]string{
		"gc.scope_ref":  "teardown",
		"gc.scope_role": "teardown",
	})

	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if result.Action != "retry" || result.Created != 1 {
		t.Fatalf("result = %+v, want the teardown tail to keep running (retry, created 1)", result)
	}
	if after := mustGet(t, store, control.ID); after.Status != "open" {
		t.Fatalf("teardown control status = %q, want open", after.Status)
	}
}

// TestProcessControlKeepsRunningUnderOpenRoot pins the hot path: an open root is
// not a stop signal, so the gate must not change ordinary dispatch.
func TestProcessControlKeepsRunningUnderOpenRoot(t *testing.T) {
	t.Parallel()

	store := beads.NewMemStore()
	control := newTerminalRootRetryControl(t, store, "", nil)

	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{})
	if err != nil {
		t.Fatalf("ProcessControl: %v", err)
	}
	if result.Action != "retry" || result.Created != 1 {
		t.Fatalf("result = %+v, want normal retry dispatch under an open root", result)
	}
}
