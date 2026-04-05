package convergence

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- Test helpers ---

// setupReconciler creates a Reconciler with a fresh fakeStore and fakeEmitter.
// The returned store starts empty — callers add beads as needed.
func setupReconciler(t *testing.T) (*Reconciler, *fakeStore, *fakeEmitter) {
	t.Helper()
	store := newFakeStore()
	emitter := &fakeEmitter{}
	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}
	return &Reconciler{Handler: handler}, store, emitter
}

// --- Path 1: Missing state ---

func TestReconcile_MissingState_NoWisps_PoursFirst(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	// Root bead with no convergence.state set.
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldFormula:       "test-formula",
		FieldMaxIterations: "5",
		FieldTarget:        "test-agent",
	})

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", report.Scanned)
	}
	if report.Recovered != 1 {
		t.Errorf("Recovered = %d, want 1", report.Recovered)
	}
	if report.Errors != 0 {
		t.Errorf("Errors = %d, want 0", report.Errors)
	}

	d := report.Details[0]
	if d.Action != "poured_wisp" {
		t.Errorf("Action = %q, want %q", d.Action, "poured_wisp")
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	// Verify state was set to active.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateActive {
		t.Errorf("state = %q, want %q", meta[FieldState], StateActive)
	}
	if meta[FieldActiveWisp] == "" {
		t.Error("active_wisp should be set after pouring")
	}
}

func TestReconcile_MissingState_WispExists_Adopts(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldFormula:       "test-formula",
		FieldMaxIterations: "5",
		FieldTarget:        "test-agent",
	})

	// Pre-existing wisp for iteration 1.
	key1 := IdempotencyKey("root-1", 1)
	store.addBead("existing-wisp", "in_progress", "root-1", key1, nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "adopted_wisp" {
		t.Errorf("Action = %q, want %q", d.Action, "adopted_wisp")
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateActive {
		t.Errorf("state = %q, want %q", meta[FieldState], StateActive)
	}
	if meta[FieldActiveWisp] != "existing-wisp" {
		t.Errorf("active_wisp = %q, want %q", meta[FieldActiveWisp], "existing-wisp")
	}
}

// --- Path 1b: StateCreating (partial creation) ---

func TestReconcile_StateCreating_TerminatesPartialCreation(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	// Bead stuck in "creating" state — creation was interrupted.
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState: StateCreating,
	})

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Recovered != 1 {
		t.Errorf("Recovered = %d, want 1", report.Recovered)
	}
	if report.Errors != 0 {
		t.Errorf("Errors = %d, want 0", report.Errors)
	}

	d := report.Details[0]
	if d.Action != "completed_terminal" {
		t.Errorf("Action = %q, want %q", d.Action, "completed_terminal")
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	// Verify the bead is now terminated and closed.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
	if meta[FieldTerminalReason] != TerminalPartialCreation {
		t.Errorf("terminal_reason = %q, want %q", meta[FieldTerminalReason], TerminalPartialCreation)
	}
	if meta[FieldTerminalActor] != "recovery" {
		t.Errorf("terminal_actor = %q, want %q", meta[FieldTerminalActor], "recovery")
	}
	beadInfo, _ := store.GetBead("root-1")
	if beadInfo.Status != "closed" {
		t.Errorf("bead status = %q, want %q", beadInfo.Status, "closed")
	}
}

// --- Path 2: Terminated but not closed ---

func TestReconcile_TerminatedNotClosed_CompletesClosure(t *testing.T) {
	rec, store, emitter := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalApproved,
		FieldTerminalActor:  "controller",
		FieldFormula:        "test-formula",
		FieldMaxIterations:  "5",
	})

	// Add a closed wisp child.
	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "completed_terminal" {
		t.Errorf("Action = %q, want %q", d.Action, "completed_terminal")
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	// Bead should now be closed.
	beadInfo, _ := store.GetBead("root-1")
	if beadInfo.Status != "closed" {
		t.Errorf("bead status = %q, want %q", beadInfo.Status, "closed")
	}

	// ConvergenceTerminated event should have been emitted with recovery=true.
	ev, ok := emitter.findEvent(EventTerminated)
	if !ok {
		t.Error("expected ConvergenceTerminated event")
	}
	if ev.BeadID != "root-1" {
		t.Errorf("event bead_id = %q, want %q", ev.BeadID, "root-1")
	}
}

func TestReconcile_TerminatedNotClosed_BackfillsActor(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	// terminal_actor is missing.
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalStopped,
		FieldFormula:        "test-formula",
	})

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "completed_terminal" {
		t.Errorf("Action = %q, want %q", d.Action, "completed_terminal")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldTerminalActor] != "recovery" {
		t.Errorf("terminal_actor = %q, want %q", meta[FieldTerminalActor], "recovery")
	}
}

func TestReconcile_TerminatedAlreadyClosed_NoAction(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "closed", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalApproved,
		FieldTerminalActor:  "controller",
	})

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "no_action" {
		t.Errorf("Action = %q, want %q", d.Action, "no_action")
	}
}

// --- Path 3: Waiting manual ---

func TestReconcile_WaitingManual_TerminalReasonSet_CompletesTerminal(t *testing.T) {
	rec, store, emitter := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateWaitingManual,
		FieldWaitingReason:  WaitManual,
		FieldTerminalReason: TerminalStopped,
		FieldTerminalActor:  "operator:alice",
		FieldFormula:        "test-formula",
	})

	// A child wisp for the iteration.
	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "completed_terminal" {
		t.Errorf("Action = %q, want %q", d.Action, "completed_terminal")
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	beadInfo, _ := store.GetBead("root-1")
	if beadInfo.Status != "closed" {
		t.Errorf("bead status = %q, want %q", beadInfo.Status, "closed")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}

	_, ok := emitter.findEvent(EventTerminated)
	if !ok {
		t.Error("expected ConvergenceTerminated event")
	}
}

func TestReconcile_WaitingManual_GenuineHold_NoStateChange(t *testing.T) {
	rec, store, emitter := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateWaitingManual,
		FieldWaitingReason:     WaitManual,
		FieldLastProcessedWisp: "wisp-1",
		FieldFormula:           "test-formula",
		FieldGateMode:          GateModeManual,
		FieldIteration:         "1",
	})

	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "no_action" {
		t.Errorf("Action = %q, want %q", d.Action, "no_action")
	}

	// State should remain waiting_manual.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateWaitingManual {
		t.Errorf("state = %q, want %q", meta[FieldState], StateWaitingManual)
	}

	// Recovery should re-emit ConvergenceWaitingManual event.
	ev, ok := emitter.findEvent(EventWaitingManual)
	if !ok {
		t.Fatal("expected ConvergenceWaitingManual recovery event")
	}
	if !ev.Recovery {
		t.Error("expected recovery flag to be true")
	}
}

func TestReconcile_WaitingManual_GenuineHold_RepairsLastProcessedWisp(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	// last_processed_wisp is stale (points to wisp-0, but wisp-1 is the
	// highest closed wisp).
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateWaitingManual,
		FieldWaitingReason:     WaitManual,
		FieldLastProcessedWisp: "wisp-0",
		FieldFormula:           "test-formula",
	})

	store.addBead("wisp-0", "closed", "root-1", IdempotencyKey("root-1", 0), nil)
	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "repaired_state" {
		t.Errorf("Action = %q, want %q", d.Action, "repaired_state")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldLastProcessedWisp] != "wisp-1" {
		t.Errorf("last_processed_wisp = %q, want %q", meta[FieldLastProcessedWisp], "wisp-1")
	}
}

// --- Path 4: Active ---

func TestReconcile_Active_ClosedUnprocessedWisp_Replays(t *testing.T) {
	rec, store, emitter := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldActiveWisp:        "wisp-iter-1",
		FieldGateMode:          GateModeCondition,
		FieldGateTimeout:       "60s",
		FieldGateTimeoutAction: TimeoutActionIterate,
		// Pre-persist the gate outcome so replay skips evaluation.
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GateFail,
	})

	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "repaired_state" {
		t.Errorf("Action = %q, want %q", d.Action, "repaired_state")
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	// After replaying wisp_closed with gate=fail and iteration < max,
	// the handler should have iterated: a new wisp should be poured
	// and active_wisp updated.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] == "" || meta[FieldActiveWisp] == "wisp-iter-1" {
		t.Errorf("active_wisp should be updated to new wisp, got %q", meta[FieldActiveWisp])
	}

	// Verify iteration event was emitted.
	if _, ok := emitter.findEvent(EventIteration); !ok {
		t.Error("expected ConvergenceIteration event from replay")
	}
}

func TestReconcile_Active_MissingActiveWisp_ReconstructsChain(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldGateTimeout:       "60s",
		FieldGateTimeoutAction: TimeoutActionIterate,
		FieldActiveWisp:        "wisp-iter-2",
		FieldLastProcessedWisp: "wisp-iter-1",
	})

	// The previous wisp exists and is closed, but the active wisp was
	// cleaned up after the crash. Startup recovery should rebuild the chain
	// from the remaining state instead of stalling on the missing bead.
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Error != nil {
		t.Fatalf("reconcile error: %v", d.Error)
	}
	if d.Action != "poured_wisp" && d.Action != "adopted_wisp" {
		t.Fatalf("Action = %q, want %q or %q", d.Action, "poured_wisp", "adopted_wisp")
	}

	meta, _ := store.GetMetadata("root-1")
	activeWisp := meta[FieldActiveWisp]
	if activeWisp == "" {
		t.Fatal("active_wisp should be restored after recovery")
	}
	if _, err := store.GetBead(activeWisp); err != nil {
		t.Fatalf("active_wisp %q should point to an existing bead: %v", activeWisp, err)
	}
}

func TestReconcile_Active_MissingActiveWisp_ReplaysRecoveredClosedReplacement(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldGateTimeout:       "60s",
		FieldGateTimeoutAction: TimeoutActionIterate,
		FieldActiveWisp:        "wisp-iter-2",
		FieldLastProcessedWisp: "wisp-iter-1",
		FieldGateOutcomeWisp:   "wisp-replacement",
		FieldGateOutcome:       GatePass,
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-replacement", "closed", "root-1", IdempotencyKey("root-1", 2), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Error != nil {
		t.Fatalf("reconcile error: %v", d.Error)
	}
	if d.Action != "repaired_state" {
		t.Fatalf("Action = %q, want %q", d.Action, "repaired_state")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Fatalf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
	if meta[FieldLastProcessedWisp] != "wisp-replacement" {
		t.Fatalf("last_processed_wisp = %q, want %q", meta[FieldLastProcessedWisp], "wisp-replacement")
	}
}

func TestReconcile_Active_MissingActiveWisp_RepairsOpenReplacementMetadata(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldGateTimeout:       "60s",
		FieldGateTimeoutAction: TimeoutActionIterate,
		FieldActiveWisp:        "wisp-iter-2",
		FieldLastProcessedWisp: "wisp-iter-1",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-replacement", "in_progress", "root-1", IdempotencyKey("root-1", 2), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Error != nil {
		t.Fatalf("reconcile error: %v", d.Error)
	}
	if d.Action != "repaired_state" {
		t.Fatalf("Action = %q, want %q", d.Action, "repaired_state")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] != "wisp-replacement" {
		t.Fatalf("active_wisp = %q, want %q", meta[FieldActiveWisp], "wisp-replacement")
	}
}

func TestReconcile_Active_StoreErrorReadingActiveWisp_ReportsError(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:      StateActive,
		FieldFormula:    "test-formula",
		FieldActiveWisp: "wisp-iter-1",
	})
	store.GetBeadFunc = func(id string) (BeadInfo, error) {
		return BeadInfo{}, fmt.Errorf("store unavailable for %s", id)
	}

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Error == nil {
		t.Fatal("expected reconcile error")
	}
	if got := d.Error.Error(); !strings.Contains(got, "store unavailable for wisp-iter-1") {
		t.Fatalf("reconcile error = %q, want store failure", got)
	}
	if d.Action != "no_action" {
		t.Fatalf("Action = %q, want %q", d.Action, "no_action")
	}
}

func TestReconcile_Active_OpenWisp_NoAction(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:      StateActive,
		FieldActiveWisp: "wisp-iter-1",
		FieldFormula:    "test-formula",
	})

	// Wisp is still open (in_progress).
	store.addBead("wisp-iter-1", "in_progress", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "no_action" {
		t.Errorf("Action = %q, want %q", d.Action, "no_action")
	}
}

func TestReconcile_Active_TerminalReasonSet_CompletesStop(t *testing.T) {
	rec, store, emitter := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateActive,
		FieldTerminalReason: TerminalStopped,
		FieldTerminalActor:  "operator:bob",
		FieldFormula:        "test-formula",
	})

	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "completed_terminal" {
		t.Errorf("Action = %q, want %q", d.Action, "completed_terminal")
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	beadInfo, _ := store.GetBead("root-1")
	if beadInfo.Status != "closed" {
		t.Errorf("bead status = %q, want %q", beadInfo.Status, "closed")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}

	_, ok := emitter.findEvent(EventTerminated)
	if !ok {
		t.Error("expected ConvergenceTerminated event")
	}
}

func TestReconcile_Active_EmptyActiveWisp_PoursNext(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:      StateActive,
		FieldActiveWisp: "",
		FieldFormula:    "test-formula",
	})

	// One closed wisp from iteration 1.
	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "poured_wisp" {
		t.Errorf("Action = %q, want %q", d.Action, "poured_wisp")
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] == "" {
		t.Error("active_wisp should be set after pouring next wisp")
	}
}

func TestReconcile_Active_EmptyActiveWisp_AdoptsExisting(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:      StateActive,
		FieldActiveWisp: "",
		FieldFormula:    "test-formula",
	})

	// One closed wisp from iteration 1.
	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	// An existing wisp for iteration 2 (already poured before crash).
	store.addBead("wisp-2", "in_progress", "root-1", IdempotencyKey("root-1", 2), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "adopted_wisp" {
		t.Errorf("Action = %q, want %q", d.Action, "adopted_wisp")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] != "wisp-2" {
		t.Errorf("active_wisp = %q, want %q", meta[FieldActiveWisp], "wisp-2")
	}
}

// --- Already processed ---

func TestReconcile_Active_AlreadyProcessed_NoAction(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldActiveWisp:        "wisp-iter-1",
		FieldLastProcessedWisp: "wisp-iter-1", // already processed
		FieldFormula:           "test-formula",
	})

	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != "no_action" {
		t.Errorf("Action = %q, want %q", d.Action, "no_action")
	}
}

// --- Multiple beads ---

func TestReconcile_MultipleBeads_ContinuesOnError(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	// bead-1: valid, needs recovery.
	store.addBead("bead-1", "in_progress", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalApproved,
		FieldTerminalActor:  "controller",
		FieldFormula:        "test-formula",
	})

	// bead-2: does not exist — will cause an error.
	// (not added to the store)

	// bead-3: valid, no action needed.
	store.addBead("bead-3", "closed", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalApproved,
		FieldTerminalActor:  "controller",
	})

	report, err := rec.ReconcileBeads(context.Background(), []string{"bead-1", "bead-2", "bead-3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if report.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3", report.Scanned)
	}
	if report.Errors != 1 {
		t.Errorf("Errors = %d, want 1", report.Errors)
	}
	if report.Recovered != 1 {
		t.Errorf("Recovered = %d, want 1", report.Recovered)
	}
	if len(report.Details) != 3 {
		t.Fatalf("Details length = %d, want 3", len(report.Details))
	}

	// bead-1: completed_terminal
	if report.Details[0].Action != "completed_terminal" {
		t.Errorf("bead-1 Action = %q, want %q", report.Details[0].Action, "completed_terminal")
	}

	// bead-2: error
	if report.Details[1].Error == nil {
		t.Error("bead-2 should have an error")
	}

	// bead-3: no_action (already closed)
	if report.Details[2].Action != "no_action" {
		t.Errorf("bead-3 Action = %q, want %q", report.Details[2].Action, "no_action")
	}
}

// --- Recovery events ---

func TestReconcile_RecoveryEventsHaveRecoveryFlag(t *testing.T) {
	store := newFakeStore()

	// Use a custom emitter that captures the recovery flag.
	type recoveryEvent struct {
		eventType string
		recovery  bool
	}
	var captured []recoveryEvent
	emitter := &recoveryCapturingEmitter{
		capture: func(eventType string, recovery bool) {
			captured = append(captured, recoveryEvent{eventType, recovery})
		},
	}

	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}
	rec := &Reconciler{Handler: handler}

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalApproved,
		FieldTerminalActor:  "controller",
		FieldFormula:        "test-formula",
	})

	_, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured) == 0 {
		t.Fatal("expected at least one event to be captured")
	}
	for _, ev := range captured {
		if !ev.recovery {
			t.Errorf("event %q should have recovery=true", ev.eventType)
		}
	}
}

// --- Helper functions ---

func TestDeriveIterationFromChildren(t *testing.T) {
	children := []BeadInfo{
		{ID: "w1", Status: "closed", IdempotencyKey: IdempotencyKey("root-1", 1)},
		{ID: "w2", Status: "closed", IdempotencyKey: IdempotencyKey("root-1", 2)},
		{ID: "w3", Status: "in_progress", IdempotencyKey: IdempotencyKey("root-1", 3)},
		{ID: "other", Status: "closed", IdempotencyKey: "unrelated-key"},
	}

	got := deriveIterationFromChildren(children, "root-1")
	if got != 2 {
		t.Errorf("deriveIterationFromChildren = %d, want 2", got)
	}
}

func TestHighestClosedWisp(t *testing.T) {
	children := []BeadInfo{
		{ID: "w1", Status: "closed", IdempotencyKey: IdempotencyKey("root-1", 1)},
		{ID: "w3", Status: "closed", IdempotencyKey: IdempotencyKey("root-1", 3)},
		{ID: "w2", Status: "closed", IdempotencyKey: IdempotencyKey("root-1", 2)},
		{ID: "w4", Status: "in_progress", IdempotencyKey: IdempotencyKey("root-1", 4)},
	}

	best, iter, found := highestClosedWisp(children, "root-1")
	if !found {
		t.Fatal("expected to find a closed wisp")
	}
	if best.ID != "w3" {
		t.Errorf("best.ID = %q, want %q", best.ID, "w3")
	}
	if iter != 3 {
		t.Errorf("iter = %d, want 3", iter)
	}
}

func TestHighestClosedWisp_NoneFound(t *testing.T) {
	children := []BeadInfo{
		{ID: "w1", Status: "in_progress", IdempotencyKey: IdempotencyKey("root-1", 1)},
	}

	_, _, found := highestClosedWisp(children, "root-1")
	if found {
		t.Error("expected not to find a closed wisp")
	}
}

func TestReconcile_EmptyList_NoOp(t *testing.T) {
	rec, _, _ := setupReconciler(t)

	report, err := rec.ReconcileBeads(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0", report.Scanned)
	}
	if report.Recovered != 0 {
		t.Errorf("Recovered = %d, want 0", report.Recovered)
	}
	if len(report.Details) != 0 {
		t.Errorf("Details length = %d, want 0", len(report.Details))
	}
}

// --- recoveryCapturingEmitter ---

// recoveryCapturingEmitter is a test-only EventEmitter that captures the
// recovery flag passed to Emit.  It also satisfies the fakeEmitter
// contract for findEvent.
type recoveryCapturingEmitter struct {
	fakeEmitter
	capture func(eventType string, recovery bool)
}

func (e *recoveryCapturingEmitter) Emit(eventType, eventID, beadID string, payload json.RawMessage, recovery bool) {
	e.fakeEmitter.Emit(eventType, eventID, beadID, payload, recovery)
	if e.capture != nil {
		e.capture(eventType, recovery)
	}
}
