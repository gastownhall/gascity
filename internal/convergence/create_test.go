package convergence

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCreateHandler_Basic(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}

	params := CreateParams{
		Formula:       "test-formula",
		Target:        "test-agent",
		MaxIterations: 5,
		GateMode:      GateModeManual,
		Title:         "Test convergence",
		Vars:          map[string]string{"doc_path": "/docs/readme.md"},
		CityPath:      "/home/test/city",
	}

	result, err := handler.CreateHandler(context.Background(), params)
	if err != nil {
		t.Fatalf("CreateHandler returned error: %v", err)
	}
	if result.BeadID == "" {
		t.Fatal("CreateHandler returned empty bead ID")
	}
	if result.FirstWispID == "" {
		t.Fatal("CreateHandler returned empty first wisp ID")
	}

	// Verify root bead metadata.
	meta, err := store.GetMetadata(result.BeadID)
	if err != nil {
		t.Fatalf("GetMetadata(%q): %v", result.BeadID, err)
	}
	if meta[FieldFormula] != "test-formula" {
		t.Errorf("formula = %q, want %q", meta[FieldFormula], "test-formula")
	}
	if meta[FieldTarget] != "test-agent" {
		t.Errorf("target = %q, want %q", meta[FieldTarget], "test-agent")
	}
	if meta[FieldState] != StateActive {
		t.Errorf("state = %q, want %q", meta[FieldState], StateActive)
	}
	if meta[FieldMaxIterations] != "5" {
		t.Errorf("max_iterations = %q, want %q", meta[FieldMaxIterations], "5")
	}
	if meta[FieldGateMode] != GateModeManual {
		t.Errorf("gate_mode = %q, want %q", meta[FieldGateMode], GateModeManual)
	}
	if meta[FieldActiveWisp] != result.FirstWispID {
		t.Errorf("active_wisp = %q, want %q", meta[FieldActiveWisp], result.FirstWispID)
	}
	if meta[FieldIteration] != "1" {
		t.Errorf("iteration = %q, want %q", meta[FieldIteration], "1")
	}
	if meta[VarPrefix+"doc_path"] != "/docs/readme.md" {
		t.Errorf("var.doc_path = %q, want %q", meta[VarPrefix+"doc_path"], "/docs/readme.md")
	}
	if meta[FieldCityPath] != "/home/test/city" {
		t.Errorf("city_path = %q, want %q", meta[FieldCityPath], "/home/test/city")
	}

	// Verify first wisp has correct idempotency key.
	wispInfo, err := store.GetBead(result.FirstWispID)
	if err != nil {
		t.Fatalf("GetBead(%q): %v", result.FirstWispID, err)
	}
	expectedKey := IdempotencyKey(result.BeadID, 1)
	if wispInfo.IdempotencyKey != expectedKey {
		t.Errorf("wisp idempotency key = %q, want %q", wispInfo.IdempotencyKey, expectedKey)
	}

	// Verify ConvergenceCreated event was emitted.
	if len(emitter.events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(emitter.events))
	}
	evt := emitter.events[0]
	if evt.Type != EventCreated {
		t.Errorf("event type = %q, want %q", evt.Type, EventCreated)
	}
	var payload CreatedPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatalf("unmarshaling payload: %v", err)
	}
	if payload.Formula != "test-formula" {
		t.Errorf("payload.Formula = %q, want %q", payload.Formula, "test-formula")
	}
	if payload.FirstWispID != result.FirstWispID {
		t.Errorf("payload.FirstWispID = %q, want %q", payload.FirstWispID, result.FirstWispID)
	}
}

func TestCreateHandler_Validation(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}

	tests := []struct {
		name   string
		params CreateParams
		errMsg string
	}{
		{
			name:   "missing formula",
			params: CreateParams{Target: "agent", MaxIterations: 5},
			errMsg: "formula is required",
		},
		{
			name:   "missing target",
			params: CreateParams{Formula: "f", MaxIterations: 5},
			errMsg: "target is required",
		},
		{
			name:   "zero max iterations",
			params: CreateParams{Formula: "f", Target: "a", MaxIterations: 0},
			errMsg: "max_iterations must be positive",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := handler.CreateHandler(context.Background(), tt.params)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.errMsg) {
				t.Errorf("error = %q, want to contain %q", err.Error(), tt.errMsg)
			}
		})
	}
}

func TestCreateHandler_PartialCreateCleanup(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}

	// Make PourWisp fail to simulate a partial-create scenario.
	store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		return "", fmt.Errorf("simulated PourWisp failure")
	}

	params := CreateParams{
		Formula:       "test-formula",
		Target:        "test-agent",
		MaxIterations: 5,
		GateMode:      GateModeManual,
	}

	_, err := handler.CreateHandler(context.Background(), params)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "pouring first wisp") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "pouring first wisp")
	}

	// The orphan bead should have been closed/terminated so the reconciler
	// does not try to resume it.
	for _, rec := range store.beads {
		if rec.info.Status == "closed" && rec.metadata[FieldState] == StateTerminated {
			return // cleanup happened
		}
	}
	t.Error("orphan bead was not terminated+closed after partial create failure")
}

func TestCreateHandler_InvalidGateConfig(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}

	params := CreateParams{
		Formula:       "test-formula",
		Target:        "test-agent",
		MaxIterations: 5,
		GateMode:      "invalid-mode",
	}

	_, err := handler.CreateHandler(context.Background(), params)
	if err == nil {
		t.Fatal("expected error for invalid gate_mode, got nil")
	}

	// No bead should have been created — validation happens before CreateConvergenceBead.
	if len(store.beads) != 0 {
		t.Errorf("expected 0 beads after invalid gate config, got %d", len(store.beads))
	}
}

func TestCreateHandler_StateCreatingBeforeActive(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}

	params := CreateParams{
		Formula:       "test-formula",
		Target:        "test-agent",
		MaxIterations: 3,
		GateMode:      GateModeManual,
	}

	_, err := handler.CreateHandler(context.Background(), params)
	if err != nil {
		t.Fatalf("CreateHandler returned error: %v", err)
	}

	// Verify write ordering: StateCreating must appear before StateActive.
	creatingIdx := -1
	activeIdx := -1
	for i, key := range store.WriteLog {
		if key == FieldState {
			if creatingIdx == -1 {
				creatingIdx = i
			} else if activeIdx == -1 {
				activeIdx = i
			}
		}
	}
	if creatingIdx == -1 {
		t.Fatal("expected at least one FieldState write (creating)")
	}
	if activeIdx == -1 {
		t.Fatal("expected a second FieldState write (active)")
	}
	if creatingIdx >= activeIdx {
		t.Errorf("StateCreating (index %d) must be written before StateActive (index %d)", creatingIdx, activeIdx)
	}
}

func TestCreateHandler_DefaultGateMode(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}

	params := CreateParams{
		Formula:       "test-formula",
		Target:        "test-agent",
		MaxIterations: 3,
		// GateMode left empty — should default to manual.
	}

	result, err := handler.CreateHandler(context.Background(), params)
	if err != nil {
		t.Fatalf("CreateHandler returned error: %v", err)
	}
	meta, _ := store.GetMetadata(result.BeadID)
	if meta[FieldGateMode] != GateModeManual {
		t.Errorf("gate_mode = %q, want %q", meta[FieldGateMode], GateModeManual)
	}
}

func TestCreateHandler_PersistsRig(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}

	result, err := handler.CreateHandler(context.Background(), CreateParams{
		Formula:       "test-formula",
		Target:        "test-agent",
		MaxIterations: 3,
		GateMode:      GateModeManual,
		Rig:           "gascity-prod",
	})
	if err != nil {
		t.Fatalf("CreateHandler returned error: %v", err)
	}
	meta, err := store.GetMetadata(result.BeadID)
	if err != nil {
		t.Fatalf("GetMetadata(%q): %v", result.BeadID, err)
	}
	if meta[FieldRig] != "gascity-prod" {
		t.Errorf("rig = %q, want %q", meta[FieldRig], "gascity-prod")
	}
	if len(emitter.events) != 1 {
		t.Fatalf("emitted events = %d, want 1", len(emitter.events))
	}
	var payload CreatedPayload
	if err := json.Unmarshal(emitter.events[0].Payload, &payload); err != nil {
		t.Fatalf("unmarshaling event payload: %v", err)
	}
	if payload.Rig != "gascity-prod" {
		t.Errorf("payload.Rig = %q, want %q", payload.Rig, "gascity-prod")
	}
}

func TestCreateHandler_TriggerDefersFirstWisp(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	handler := &Handler{Store: store, Emitter: emitter, Clock: time.Now}

	result, err := handler.CreateHandler(context.Background(), CreateParams{
		Formula:          "test-formula",
		Target:           "test-agent",
		MaxIterations:    5,
		GateMode:         GateModeManual,
		Trigger:          TriggerEvent,
		TriggerCondition: "/scripts/check-chunk-complete",
	})
	if err != nil {
		t.Fatalf("CreateHandler: %v", err)
	}
	if result.FirstWispID != "" {
		t.Errorf("FirstWispID = %q, want empty (first pour deferred to trigger)", result.FirstWispID)
	}

	meta, _ := store.GetMetadata(result.BeadID)
	if meta[FieldState] != StateWaitingTrigger {
		t.Errorf("state = %q, want %q", meta[FieldState], StateWaitingTrigger)
	}
	if meta[FieldIteration] != "0" {
		t.Errorf("iteration = %q, want 0", meta[FieldIteration])
	}
	if meta[FieldTrigger] != TriggerEvent {
		t.Errorf("trigger = %q, want %q", meta[FieldTrigger], TriggerEvent)
	}
	if meta[FieldTriggerCondition] != "/scripts/check-chunk-complete" {
		t.Errorf("trigger_condition = %q, want %q", meta[FieldTriggerCondition], "/scripts/check-chunk-complete")
	}
	if meta[FieldActiveWisp] != "" {
		t.Errorf("active_wisp = %q, want empty", meta[FieldActiveWisp])
	}

	// No wisp should have been poured at create time.
	children, _ := store.Children(result.BeadID)
	if len(children) != 0 {
		t.Errorf("children = %d, want 0 (no wisp until trigger passes)", len(children))
	}

	// Created event with empty first wisp.
	ev, ok := emitter.findEvent(EventCreated)
	if !ok {
		t.Fatal("expected ConvergenceCreated event")
	}
	var payload CreatedPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if payload.FirstWispID != "" {
		t.Errorf("payload.FirstWispID = %q, want empty", payload.FirstWispID)
	}
}

func TestCreateHandler_TriggerRequiresCondition(t *testing.T) {
	store := newFakeStore()
	handler := &Handler{Store: store, Emitter: &fakeEmitter{}, Clock: time.Now}

	_, err := handler.CreateHandler(context.Background(), CreateParams{
		Formula:       "test-formula",
		Target:        "test-agent",
		MaxIterations: 5,
		GateMode:      GateModeManual,
		Trigger:       TriggerEvent,
		// TriggerCondition omitted — must error before any bead is created.
	})
	if err == nil {
		t.Fatal("expected error for trigger without condition")
	}
	if !strings.Contains(err.Error(), "requires a trigger condition") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "requires a trigger condition")
	}
	if len(store.beads) != 0 {
		t.Errorf("beads = %d, want 0 (validation before create)", len(store.beads))
	}
}

func TestCreateHandler_EmptyRigForCityScope(t *testing.T) {
	store := newFakeStore()
	handler := &Handler{
		Store:   store,
		Emitter: &fakeEmitter{},
		Clock:   time.Now,
	}

	result, err := handler.CreateHandler(context.Background(), CreateParams{
		Formula:       "test-formula",
		Target:        "test-agent",
		MaxIterations: 3,
		GateMode:      GateModeManual,
		// Rig left empty — city/HQ scope.
	})
	if err != nil {
		t.Fatalf("CreateHandler returned error: %v", err)
	}
	meta, _ := store.GetMetadata(result.BeadID)
	if meta[FieldRig] != "" {
		t.Errorf("rig = %q, want empty for city scope", meta[FieldRig])
	}
}
