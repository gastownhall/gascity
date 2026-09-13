package convergence

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// setupTerminatedHandler creates a handler with a terminated root bead
// suitable for retry tests.
func setupTerminatedHandler(t *testing.T, terminalReason string, extraMeta map[string]string) (*Handler, *fakeStore, *fakeEmitter) {
	t.Helper()

	store := newFakeStore()
	emitter := &fakeEmitter{}

	rootMeta := map[string]string{
		FieldState:             StateTerminated,
		FieldIteration:         "3",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldGateCondition:     "/path/to/gate.sh",
		FieldGateTimeout:       "30s",
		FieldGateTimeoutAction: TimeoutActionIterate,
		FieldTerminalReason:    terminalReason,
		FieldTerminalActor:     "controller",
		FieldCityPath:          "/home/test/city",
		FieldEvaluatePrompt:    "check the code",
		FieldLastProcessedWisp: "wisp-iter-3",
		VarPrefix + "doc_path": "/docs/readme.md",
		VarPrefix + "branch":   "feature-x",
	}
	for k, v := range extraMeta {
		rootMeta[k] = v
	}

	store.addBead("source-1", "closed", "", "", rootMeta)
	store.addBead("wisp-iter-3", "closed", "source-1",
		IdempotencyKey("source-1", 3), nil)

	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}

	return handler, store, emitter
}

func TestRetryHandler_CarriesRigForward(t *testing.T) {
	handler, store, _ := setupTerminatedHandler(t, TerminalStopped,
		map[string]string{FieldRig: "gascity-prod"})

	result, err := handler.RetryHandler(context.Background(), "source-1", "alice", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	meta, _ := store.GetMetadata(result.NewBeadID)
	if meta[FieldRig] != "gascity-prod" {
		t.Errorf("retry bead rig = %q, want %q (rig must carry forward)", meta[FieldRig], "gascity-prod")
	}
}

func TestRetryHandler_Success(t *testing.T) {
	handler, store, _ := setupTerminatedHandler(t, TerminalStopped, nil)

	result, err := handler.RetryHandler(context.Background(), "source-1", "alice", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.NewBeadID == "" {
		t.Fatal("expected NewBeadID to be set")
	}
	if result.FirstWispID == "" {
		t.Fatal("expected FirstWispID to be set")
	}
	if result.Iteration != 1 {
		t.Errorf("Iteration = %d, want 1", result.Iteration)
	}

	// Verify new bead metadata.
	meta, _ := store.GetMetadata(result.NewBeadID)
	if meta[FieldState] != StateActive {
		t.Errorf("state = %q, want %q", meta[FieldState], StateActive)
	}
	if meta[FieldFormula] != "test-formula" {
		t.Errorf("formula = %q, want %q", meta[FieldFormula], "test-formula")
	}
	if meta[FieldActiveWisp] != result.FirstWispID {
		t.Errorf("active_wisp = %q, want %q", meta[FieldActiveWisp], result.FirstWispID)
	}
	if meta[FieldMaxIterations] != "10" {
		t.Errorf("max_iterations = %q, want %q", meta[FieldMaxIterations], "10")
	}
	if meta[FieldIteration] != "1" {
		t.Errorf("iteration = %q, want %q", meta[FieldIteration], "1")
	}
}

func TestRetryHandler_PartialCreateCleanup(t *testing.T) {
	handler, store, _ := setupTerminatedHandler(t, TerminalStopped, nil)

	// Make PourWisp fail to simulate a partial-create scenario.
	store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		return "", fmt.Errorf("simulated PourWisp failure")
	}

	_, err := handler.RetryHandler(context.Background(), "source-1", "alice", 5)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "pouring first wisp") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "pouring first wisp")
	}

	// The orphan bead should have been closed/terminated.
	for _, rec := range store.beads {
		if rec.info.ID == "source-1" {
			continue // skip the source bead
		}
		if rec.info.Status == "closed" && rec.metadata[FieldState] == StateTerminated {
			return // cleanup happened
		}
	}
	t.Error("orphan bead was not terminated+closed after partial retry failure")
}

func TestRetryHandler_InvalidGateConfig(t *testing.T) {
	handler, store, _ := setupTerminatedHandler(t, TerminalStopped, map[string]string{
		FieldGateMode: "invalid-mode",
	})

	_, err := handler.RetryHandler(context.Background(), "source-1", "alice", 5)
	if err == nil {
		t.Fatal("expected error for invalid gate_mode from source bead, got nil")
	}
	if !strings.Contains(err.Error(), "invalid gate config") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "invalid gate config")
	}

	// No new bead should have been created — only the source bead should exist.
	for _, rec := range store.beads {
		if rec.info.ID != "source-1" && rec.info.ID != "wisp-iter-3" {
			t.Errorf("unexpected bead %q created despite invalid gate config", rec.info.ID)
		}
	}
}

func TestRetryHandler_SourceNotTerminated(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	store.addBead("active-1", "in_progress", "", "", map[string]string{
		FieldState:  StateActive,
		FieldTarget: "test-agent",
	})

	handler := &Handler{Store: store, Emitter: emitter}

	_, err := handler.RetryHandler(context.Background(), "active-1", "alice", 5)
	if err == nil {
		t.Fatal("expected error for non-terminated source")
	}
	if !contains(err.Error(), StateTerminated) {
		t.Errorf("error should mention %q, got: %v", StateTerminated, err)
	}
}

func TestRetryHandler_SourceApproved(t *testing.T) {
	handler, _, _ := setupTerminatedHandler(t, TerminalApproved, nil)

	_, err := handler.RetryHandler(context.Background(), "source-1", "alice", 5)
	if err == nil {
		t.Fatal("expected error for approved source")
	}
	if !contains(err.Error(), TerminalApproved) {
		t.Errorf("error should mention %q, got: %v", TerminalApproved, err)
	}
	if !contains(err.Error(), "cannot be retried") {
		t.Errorf("error should mention 'cannot be retried', got: %v", err)
	}
}

func TestRetryHandler_CopiesConfig(t *testing.T) {
	handler, store, _ := setupTerminatedHandler(t, TerminalNoConvergence, map[string]string{
		VarPrefix + "extra_var": "extra_value",
	})

	result, err := handler.RetryHandler(context.Background(), "source-1", "alice", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	meta, _ := store.GetMetadata(result.NewBeadID)

	// Verify all config fields are copied.
	if meta[FieldFormula] != "test-formula" {
		t.Errorf("formula = %q, want %q", meta[FieldFormula], "test-formula")
	}
	if meta[FieldTarget] != "test-agent" {
		t.Errorf("target = %q, want %q", meta[FieldTarget], "test-agent")
	}
	if meta[FieldGateMode] != GateModeCondition {
		t.Errorf("gate_mode = %q, want %q", meta[FieldGateMode], GateModeCondition)
	}
	if meta[FieldGateCondition] != "/path/to/gate.sh" {
		t.Errorf("gate_condition = %q, want %q", meta[FieldGateCondition], "/path/to/gate.sh")
	}
	if meta[FieldGateTimeout] != "30s" {
		t.Errorf("gate_timeout = %q, want %q", meta[FieldGateTimeout], "30s")
	}
	if meta[FieldGateTimeoutAction] != TimeoutActionIterate {
		t.Errorf("gate_timeout_action = %q, want %q", meta[FieldGateTimeoutAction], TimeoutActionIterate)
	}
	if meta[FieldCityPath] != "/home/test/city" {
		t.Errorf("city_path = %q, want %q", meta[FieldCityPath], "/home/test/city")
	}
	if meta[FieldEvaluatePrompt] != "check the code" {
		t.Errorf("evaluate_prompt = %q, want %q", meta[FieldEvaluatePrompt], "check the code")
	}
	if meta[FieldMaxIterations] != "10" {
		t.Errorf("max_iterations = %q, want %q", meta[FieldMaxIterations], "10")
	}

	// Verify template variables are copied.
	if meta[VarPrefix+"doc_path"] != "/docs/readme.md" {
		t.Errorf("var.doc_path = %q, want %q", meta[VarPrefix+"doc_path"], "/docs/readme.md")
	}
	if meta[VarPrefix+"branch"] != "feature-x" {
		t.Errorf("var.branch = %q, want %q", meta[VarPrefix+"branch"], "feature-x")
	}
	if meta[VarPrefix+"extra_var"] != "extra_value" {
		t.Errorf("var.extra_var = %q, want %q", meta[VarPrefix+"extra_var"], "extra_value")
	}
}

// TestRetryHandler_CarriesTriggerForward is the regression guard for the
// live trigger-config-loss drift: before RetryHandler delegated to
// CreateHandler it silently dropped the trigger/trigger_condition fields, so
// retrying a trigger-gated loop produced a non-trigger-gated loop that poured
// its first wisp immediately. Delegation must carry the trigger config forward
// AND honor CreateHandler's trigger entry gate (waiting_trigger, no first wisp).
func TestRetryHandler_CarriesTriggerForward(t *testing.T) {
	handler, store, _ := setupTerminatedHandler(t, TerminalStopped, map[string]string{
		FieldTrigger:          TriggerEvent,
		FieldTriggerCondition: "/path/to/trigger.sh",
	})

	result, err := handler.RetryHandler(context.Background(), "source-1", "alice", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	meta, _ := store.GetMetadata(result.NewBeadID)
	if meta[FieldTrigger] != TriggerEvent {
		t.Errorf("trigger = %q, want %q (trigger config must carry forward on retry)", meta[FieldTrigger], TriggerEvent)
	}
	if meta[FieldTriggerCondition] != "/path/to/trigger.sh" {
		t.Errorf("trigger_condition = %q, want %q", meta[FieldTriggerCondition], "/path/to/trigger.sh")
	}
	// The trigger entry gate must defer the first pour: state waiting_trigger,
	// iteration 0, no first wisp — exactly what CreateHandler does for a fresh
	// trigger-gated loop.
	if meta[FieldState] != StateWaitingTrigger {
		t.Errorf("state = %q, want %q (trigger entry gate must be honored on retry)", meta[FieldState], StateWaitingTrigger)
	}
	if meta[FieldIteration] != "0" {
		t.Errorf("iteration = %q, want %q", meta[FieldIteration], "0")
	}
	if result.FirstWispID != "" {
		t.Errorf("FirstWispID = %q, want empty (trigger-gated loop defers first pour)", result.FirstWispID)
	}
}

func TestRetryHandler_SetsRetrySource(t *testing.T) {
	handler, store, _ := setupTerminatedHandler(t, TerminalStopped, nil)

	result, err := handler.RetryHandler(context.Background(), "source-1", "alice", 5)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	meta, _ := store.GetMetadata(result.NewBeadID)
	if meta[FieldRetrySource] != "source-1" {
		t.Errorf("retry_source = %q, want %q", meta[FieldRetrySource], "source-1")
	}
}

func TestRetryHandler_EmitsCreatedEvent(t *testing.T) {
	handler, _, emitter := setupTerminatedHandler(t, TerminalStopped, nil)

	result, err := handler.RetryHandler(context.Background(), "source-1", "alice", 10)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	ev, ok := emitter.findEvent(EventCreated)
	if !ok {
		t.Fatal("expected ConvergenceCreated event")
	}
	if ev.BeadID != result.NewBeadID {
		t.Errorf("event bead_id = %q, want %q", ev.BeadID, result.NewBeadID)
	}
	if ev.EventID != EventIDCreated(result.NewBeadID) {
		t.Errorf("event_id = %q, want %q", ev.EventID, EventIDCreated(result.NewBeadID))
	}

	var payload CreatedPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal created payload: %v", err)
	}
	if payload.Formula != "test-formula" {
		t.Errorf("formula = %q, want %q", payload.Formula, "test-formula")
	}
	if payload.Target != "test-agent" {
		t.Errorf("target = %q, want %q", payload.Target, "test-agent")
	}
	if payload.GateMode != GateModeCondition {
		t.Errorf("gate_mode = %q, want %q", payload.GateMode, GateModeCondition)
	}
	if payload.MaxIterations != 10 {
		t.Errorf("max_iterations = %d, want 10", payload.MaxIterations)
	}
	if payload.FirstWispID != result.FirstWispID {
		t.Errorf("first_wisp_id = %q, want %q", payload.FirstWispID, result.FirstWispID)
	}
	if payload.RetrySource == nil || *payload.RetrySource != "source-1" {
		t.Errorf("retry_source = %v, want %q", payload.RetrySource, "source-1")
	}
}
