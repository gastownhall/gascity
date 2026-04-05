package convergence

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// setupActiveHandler creates a handler with a root bead in active state,
// one closed child wisp (iteration 1), and an active wisp (iteration 2).
func setupActiveHandler(t *testing.T, activeWispStatus string, extraMeta map[string]string) (*Handler, *fakeStore, *fakeEmitter) {
	t.Helper()

	store := newFakeStore()
	emitter := &fakeEmitter{}

	rootMeta := map[string]string{
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
	}
	for k, v := range extraMeta {
		rootMeta[k] = v
	}

	store.addBead("root-1", "in_progress", "", "", rootMeta)
	store.addBead("wisp-iter-1", "closed", "root-1",
		IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-iter-2", activeWispStatus, "root-1",
		IdempotencyKey("root-1", 2), nil)

	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}

	return handler, store, emitter
}

func TestStopHandler_DrainCompletedIteration(t *testing.T) {
	// Active wisp is already closed and gate passes -> HandleWispClosed
	// should terminate the loop via gate pass, making stop a no-op.
	handler, store, _ := setupActiveHandler(t, "closed", map[string]string{
		FieldGateOutcomeWisp: "wisp-iter-2",
		FieldGateOutcome:     GatePass,
	})
	_ = store

	result, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// HandleWispClosed terminated the loop (gate passed), so stop is a no-op.
	if result.Action != ActionStopped {
		t.Errorf("Action = %q, want %q", result.Action, ActionStopped)
	}

	// Verify the loop was terminated by the drain (approved).
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
	if meta[FieldTerminalReason] != TerminalApproved {
		t.Errorf("terminal_reason = %q, want %q (drain should have approved)", meta[FieldTerminalReason], TerminalApproved)
	}
}

func TestStopHandler_DrainThenStop(t *testing.T) {
	// Active wisp is closed but gate fails -> HandleWispClosed iterates,
	// then stop continues with the new state.
	handler, store, emitter := setupActiveHandler(t, "closed", map[string]string{
		FieldGateOutcomeWisp: "wisp-iter-2",
		FieldGateOutcome:     GateFail,
	})

	result, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionStopped {
		t.Errorf("Action = %q, want %q", result.Action, ActionStopped)
	}

	// Verify terminal state with stopped reason (not approved).
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
	if meta[FieldTerminalReason] != TerminalStopped {
		t.Errorf("terminal_reason = %q, want %q", meta[FieldTerminalReason], TerminalStopped)
	}

	// Verify terminated event was emitted.
	if _, ok := emitter.findEvent(EventTerminated); !ok {
		t.Error("expected ConvergenceTerminated event")
	}
	_ = store
}

func TestStopHandler_ForceClose(t *testing.T) {
	// Active wisp is still open -> force-close it.
	handler, store, _ := setupActiveHandler(t, "in_progress", nil)

	result, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionStopped {
		t.Errorf("Action = %q, want %q", result.Action, ActionStopped)
	}

	// Verify wisp was force-closed.
	wispInfo, _ := store.GetBead("wisp-iter-2")
	if wispInfo.Status != "closed" {
		t.Errorf("wisp status = %q, want %q (should be force-closed)", wispInfo.Status, "closed")
	}

	// Verify root bead is terminated.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
	if meta[FieldTerminalReason] != TerminalStopped {
		t.Errorf("terminal_reason = %q, want %q", meta[FieldTerminalReason], TerminalStopped)
	}
	// last_processed_wisp should be updated to the force-closed wisp
	// (the highest closed wisp after force-close).
	if meta[FieldLastProcessedWisp] != "wisp-iter-2" {
		t.Errorf("last_processed_wisp = %q, want %q (should be force-closed wisp)",
			meta[FieldLastProcessedWisp], "wisp-iter-2")
	}
}

func TestStopHandler_ForceClose_SyntheticEvent(t *testing.T) {
	// Active wisp is open -> force-close emits synthetic iteration event.
	handler, _, emitter := setupActiveHandler(t, "in_progress", nil)

	_, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Find the synthetic iteration event.
	ev, ok := emitter.findEvent(EventIteration)
	if !ok {
		t.Fatal("expected synthetic ConvergenceIteration event for force-closed wisp")
	}

	var payload IterationPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal iteration payload: %v", err)
	}
	if payload.Action != string(ActionStopped) {
		t.Errorf("action = %q, want %q", payload.Action, string(ActionStopped))
	}
	if payload.WispID != "wisp-iter-2" {
		t.Errorf("wisp_id = %q, want %q", payload.WispID, "wisp-iter-2")
	}
	// gate_outcome and next_wisp_id should be null.
	if payload.GateOutcome != nil {
		t.Errorf("gate_outcome = %v, want nil", payload.GateOutcome)
	}
	if payload.GateResult != nil {
		t.Errorf("gate_result = %v, want nil", payload.GateResult)
	}
	if payload.NextWispID != nil {
		t.Errorf("next_wisp_id = %v, want nil", payload.NextWispID)
	}
}

func TestStopHandler_ClearsStaleVerdict(t *testing.T) {
	handler, store, _ := setupActiveHandler(t, "in_progress", map[string]string{
		FieldAgentVerdict:     "approve",
		FieldAgentVerdictWisp: "wisp-iter-2",
	})

	_, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verdict metadata should be cleared.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldAgentVerdict] != "" {
		t.Errorf("agent_verdict should be cleared, got %q", meta[FieldAgentVerdict])
	}
	if meta[FieldAgentVerdictWisp] != "" {
		t.Errorf("agent_verdict_wisp should be cleared, got %q", meta[FieldAgentVerdictWisp])
	}
}

func TestStopHandler_FromWaitingManual_NoForceClose(t *testing.T) {
	// waiting_manual state has no active wisp to force-close.
	handler, store, emitter := setupWaitingManualHandler(t, nil)

	result, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionStopped {
		t.Errorf("Action = %q, want %q", result.Action, ActionStopped)
	}

	// Verify terminal state.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}

	// No synthetic iteration event should be emitted (no force-close happened).
	if _, ok := emitter.findEvent(EventIteration); ok {
		t.Error("should not emit synthetic ConvergenceIteration when no wisp was force-closed")
	}

	// ManualStop event should still be emitted.
	if _, ok := emitter.findEvent(EventManualStop); !ok {
		t.Error("expected ConvergenceManualStop event")
	}

	_ = store
}

func TestStopHandler_MissingActiveWisp_StopsGracefully(t *testing.T) {
	handler, store, emitter := setupActiveHandler(t, "in_progress", nil)

	store.mu.Lock()
	delete(store.beads, "wisp-iter-2")
	store.mu.Unlock()

	result, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionStopped {
		t.Fatalf("Action = %q, want %q", result.Action, ActionStopped)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Fatalf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
	if meta[FieldTerminalReason] != TerminalStopped {
		t.Fatalf("terminal_reason = %q, want %q", meta[FieldTerminalReason], TerminalStopped)
	}
	if _, ok := emitter.findEvent(EventIteration); ok {
		t.Fatal("should not emit synthetic iteration event when missing active wisp is skipped")
	}
	if _, ok := emitter.findEvent(EventManualStop); !ok {
		t.Fatal("expected ConvergenceManualStop event")
	}
}

func TestStopHandler_ActiveWispMissingBeforeForceClose_StopsGracefully(t *testing.T) {
	handler, store, _ := setupActiveHandler(t, "in_progress", nil)

	calls := 0
	store.GetBeadFunc = func(id string) (BeadInfo, error) {
		switch id {
		case "wisp-iter-1":
			return BeadInfo{
				ID:             id,
				Status:         "closed",
				ParentID:       "root-1",
				IdempotencyKey: IdempotencyKey("root-1", 1),
			}, nil
		case "wisp-iter-2":
			calls++
			if calls == 1 {
				return BeadInfo{
					ID:             id,
					Status:         "in_progress",
					ParentID:       "root-1",
					IdempotencyKey: IdempotencyKey("root-1", 2),
				}, nil
			}
			return BeadInfo{}, fmt.Errorf("bead %q: %w", id, beads.ErrNotFound)
		}
		return BeadInfo{}, fmt.Errorf("unexpected bead lookup %q", id)
	}

	result, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionStopped {
		t.Fatalf("Action = %q, want %q", result.Action, ActionStopped)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Fatalf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
}

func TestStopHandler_MissingActiveWisp_RecoversReplacementBeforeForceClose(t *testing.T) {
	handler, store, _ := setupActiveHandler(t, "in_progress", nil)

	store.mu.Lock()
	delete(store.beads, "wisp-iter-2")
	store.mu.Unlock()
	store.addBead("wisp-replacement", "in_progress", "root-1", IdempotencyKey("root-1", 2), nil)

	result, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionStopped {
		t.Fatalf("Action = %q, want %q", result.Action, ActionStopped)
	}

	replacement, err := store.GetBead("wisp-replacement")
	if err != nil {
		t.Fatalf("reading replacement wisp: %v", err)
	}
	if replacement.Status != "closed" {
		t.Fatalf("replacement status = %q, want %q", replacement.Status, "closed")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldLastProcessedWisp] != "wisp-replacement" {
		t.Fatalf("last_processed_wisp = %q, want %q", meta[FieldLastProcessedWisp], "wisp-replacement")
	}
}

func TestStopHandler_StoreErrorReadingActiveWisp_ReportsError(t *testing.T) {
	handler, store, _ := setupActiveHandler(t, "in_progress", nil)

	store.GetBeadFunc = func(id string) (BeadInfo, error) {
		return BeadInfo{}, fmt.Errorf("store unavailable for %s", id)
	}

	_, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := err.Error(); got != "reading active wisp \"wisp-iter-2\": store unavailable for wisp-iter-2" {
		t.Fatalf("StopHandler error = %q", got)
	}
}
