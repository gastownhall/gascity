package convergence

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeTriggerScript writes an executable script that exits with the given
// code and returns its absolute path.
func writeTriggerScript(t *testing.T, exitCode int) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, "trigger.sh")
	body := fmt.Sprintf("#!/bin/sh\nexit %d\n", exitCode)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatalf("writing trigger script: %v", err)
	}
	return script
}

func TestParseTriggerConfig(t *testing.T) {
	tests := []struct {
		name      string
		meta      map[string]string
		wantMode  string
		wantEnab  bool
		wantError string
	}{
		{
			name:     "no trigger (default)",
			meta:     map[string]string{},
			wantMode: TriggerNone,
			wantEnab: false,
		},
		{
			name:     "event with condition",
			meta:     map[string]string{FieldTrigger: TriggerEvent, FieldTriggerCondition: "/path/to/check"},
			wantMode: TriggerEvent,
			wantEnab: true,
		},
		{
			name:      "event without condition",
			meta:      map[string]string{FieldTrigger: TriggerEvent},
			wantError: "requires a trigger condition",
		},
		{
			name:      "invalid mode",
			meta:      map[string]string{FieldTrigger: "cron"},
			wantError: "invalid trigger mode",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tc, err := ParseTriggerConfig(tt.meta)
			if tt.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantError) {
					t.Fatalf("error = %v, want to contain %q", err, tt.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.Mode != tt.wantMode {
				t.Errorf("Mode = %q, want %q", tc.Mode, tt.wantMode)
			}
			if tc.Enabled() != tt.wantEnab {
				t.Errorf("Enabled() = %v, want %v", tc.Enabled(), tt.wantEnab)
			}
		})
	}
}

// setupTriggerHandler builds a handler with a single waiting_trigger root bead.
func setupTriggerHandler(t *testing.T, condition string, extraMeta map[string]string) (*Handler, *fakeStore, *fakeEmitter) {
	t.Helper()
	store := newFakeStore()
	emitter := &fakeEmitter{}
	rootMeta := map[string]string{
		FieldState:            StateWaitingTrigger,
		FieldIteration:        "0",
		FieldMaxIterations:    "5",
		FieldFormula:          "test-formula",
		FieldTarget:           "test-agent",
		FieldGateMode:         GateModeCondition,
		FieldGateCondition:    "/gate/ignored-in-trigger-tests",
		FieldGateTimeout:      "60s",
		FieldTrigger:          TriggerEvent,
		FieldTriggerCondition: condition,
		FieldCityPath:         t.TempDir(),
	}
	for k, v := range extraMeta {
		rootMeta[k] = v
	}
	store.addBead("root-1", "in_progress", "", "", rootMeta)
	handler := &Handler{Store: store, Emitter: emitter, Clock: time.Now}
	return handler, store, emitter
}

func TestHandleTrigger_EntryPoursFirstWispOnPass(t *testing.T) {
	handler, store, emitter := setupTriggerHandler(t, writeTriggerScript(t, 0), nil)

	result, err := handler.HandleTrigger(context.Background(), "root-1")
	if err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	if result.Action != ActionIterate {
		t.Errorf("Action = %q, want %q", result.Action, ActionIterate)
	}
	if result.Iteration != 1 {
		t.Errorf("Iteration = %d, want 1", result.Iteration)
	}
	if result.NextWispID == "" {
		t.Fatal("expected NextWispID to be set")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateActive {
		t.Errorf("state = %q, want %q", meta[FieldState], StateActive)
	}
	if meta[FieldActiveWisp] != result.NextWispID {
		t.Errorf("active_wisp = %q, want %q", meta[FieldActiveWisp], result.NextWispID)
	}
	if meta[FieldIteration] != "1" {
		t.Errorf("iteration = %q, want 1", meta[FieldIteration])
	}

	// The poured wisp must carry the iteration-1 idempotency key.
	wispInfo, err := store.GetBead(result.NextWispID)
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if wispInfo.IdempotencyKey != IdempotencyKey("root-1", 1) {
		t.Errorf("wisp key = %q, want %q", wispInfo.IdempotencyKey, IdempotencyKey("root-1", 1))
	}

	if _, ok := emitter.findEvent(EventIteration); !ok {
		t.Error("expected ConvergenceIteration event")
	}
}

func TestHandleTrigger_WaitsWhenConditionFails(t *testing.T) {
	handler, store, _ := setupTriggerHandler(t, writeTriggerScript(t, 1), nil)

	result, err := handler.HandleTrigger(context.Background(), "root-1")
	if err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	if result.Action != ActionSkipped {
		t.Errorf("Action = %q, want %q", result.Action, ActionSkipped)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateWaitingTrigger {
		t.Errorf("state = %q, want %q (should keep waiting)", meta[FieldState], StateWaitingTrigger)
	}
	if meta[FieldActiveWisp] != "" {
		t.Errorf("active_wisp = %q, want empty (no wisp poured)", meta[FieldActiveWisp])
	}
	// No wisp should have been poured.
	children, _ := store.Children("root-1")
	if len(children) != 0 {
		t.Errorf("children = %d, want 0 (no wisp poured while waiting)", len(children))
	}
}

func TestHandleTrigger_IterationGateAdvance(t *testing.T) {
	handler, store, _ := setupTriggerHandler(t, writeTriggerScript(t, 0), map[string]string{
		FieldIteration:         "1",
		FieldLastProcessedWisp: "wisp-iter-1",
	})
	// One closed wisp already exists (iteration 1).
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	result, err := handler.HandleTrigger(context.Background(), "root-1")
	if err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	if result.Action != ActionIterate {
		t.Errorf("Action = %q, want %q", result.Action, ActionIterate)
	}
	if result.Iteration != 2 {
		t.Errorf("Iteration = %d, want 2", result.Iteration)
	}
	wispInfo, err := store.GetBead(result.NextWispID)
	if err != nil {
		t.Fatalf("GetBead: %v", err)
	}
	if wispInfo.IdempotencyKey != IdempotencyKey("root-1", 2) {
		t.Errorf("wisp key = %q, want %q", wispInfo.IdempotencyKey, IdempotencyKey("root-1", 2))
	}
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateActive {
		t.Errorf("state = %q, want %q", meta[FieldState], StateActive)
	}
}

func TestHandleTrigger_SkipsWhenNotWaiting(t *testing.T) {
	handler, _, _ := setupTriggerHandler(t, writeTriggerScript(t, 0), map[string]string{
		FieldState: StateActive,
	})
	result, err := handler.HandleTrigger(context.Background(), "root-1")
	if err != nil {
		t.Fatalf("HandleTrigger: %v", err)
	}
	if result.Action != ActionSkipped {
		t.Errorf("Action = %q, want %q", result.Action, ActionSkipped)
	}
}

func TestHandleTrigger_RefusesToExceedMaxIterations(t *testing.T) {
	// Corrupt state: waiting_trigger with closed iterations already at max.
	handler, store, _ := setupTriggerHandler(t, writeTriggerScript(t, 0), map[string]string{
		FieldMaxIterations:     "1",
		FieldIteration:         "1",
		FieldLastProcessedWisp: "wisp-iter-1",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	_, err := handler.HandleTrigger(context.Background(), "root-1")
	if err == nil {
		t.Fatal("expected error when advancing past max_iterations")
	}
	if !strings.Contains(err.Error(), "exceeds max_iterations") {
		t.Errorf("error = %q, want to contain %q", err.Error(), "exceeds max_iterations")
	}
	// No over-limit wisp should have been poured.
	children, _ := store.Children("root-1")
	if len(children) != 1 {
		t.Errorf("children = %d, want 1 (no over-limit pour)", len(children))
	}
}

func TestHandleWispClosed_TriggerGatesIteration(t *testing.T) {
	// Gate outcome is fail (would iterate) and a trigger gates the loop, so
	// instead of pouring the next wisp the loop holds in waiting_trigger.
	handler, store, emitter := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp:  "wisp-iter-1",
		FieldGateOutcome:      GateFail,
		FieldTrigger:          TriggerEvent,
		FieldTriggerCondition: "/some/trigger/check",
	})

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("HandleWispClosed: %v", err)
	}
	if result.Action != ActionWaitingTrigger {
		t.Errorf("Action = %q, want %q", result.Action, ActionWaitingTrigger)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateWaitingTrigger {
		t.Errorf("state = %q, want %q", meta[FieldState], StateWaitingTrigger)
	}
	if meta[FieldActiveWisp] != "" {
		t.Errorf("active_wisp = %q, want empty (no next wisp poured)", meta[FieldActiveWisp])
	}
	if meta[FieldLastProcessedWisp] != "wisp-iter-1" {
		t.Errorf("last_processed_wisp = %q, want wisp-iter-1", meta[FieldLastProcessedWisp])
	}

	// No successor wisp should exist: only the original closed wisp.
	children, _ := store.Children("root-1")
	if len(children) != 1 {
		t.Errorf("children = %d, want 1 (no speculative/next wisp)", len(children))
	}

	ev, ok := emitter.findEvent(EventIteration)
	if !ok {
		t.Fatal("expected ConvergenceIteration event")
	}
	var payload IterationPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Action != string(ActionWaitingTrigger) {
		t.Errorf("event action = %q, want %q", payload.Action, ActionWaitingTrigger)
	}
}
