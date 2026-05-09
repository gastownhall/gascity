package convergence

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// --- Fake ConvergenceStore ---

type fakeBeadRecord struct {
	info     BeadInfo
	metadata map[string]string
	children []string // child bead IDs
}

type fakeStore struct {
	mu    sync.Mutex
	beads map[string]*fakeBeadRecord

	// PourWispFunc can be set to simulate sling failures.
	PourWispFunc             func(parentID, formula, idempotencyKey string, vars map[string]string, evaluatePrompt string) (string, error)
	PourSpeculativeWispFunc  func(parentID, formula, idempotencyKey string, vars map[string]string, evaluatePrompt string) (string, error)
	FindByIdempotencyKeyFunc func(key string) (string, bool, error)
	ActivateWispFunc         func(id string) error
	GetBeadFunc              func(id string) (BeadInfo, error)

	pourCounter int // auto-increment for wisp IDs

	// WriteLog records the key of every SetMetadata call in order,
	// enabling tests to verify write ordering contracts.
	WriteLog []string

	ActivatedWispIDs []string
}

func newFakeStore() *fakeStore {
	return &fakeStore{beads: make(map[string]*fakeBeadRecord)}
}

func (s *fakeStore) addBead(id, status, parentID, idempotencyKey string, meta map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if meta == nil {
		meta = make(map[string]string)
	}
	s.beads[id] = &fakeBeadRecord{
		info: BeadInfo{
			ID:             id,
			Status:         status,
			ParentID:       parentID,
			IdempotencyKey: idempotencyKey,
			CreatedAt:      time.Now().Add(-10 * time.Minute),
			ClosedAt:       time.Now(),
		},
		metadata: meta,
	}
	// Register as child of parent.
	if parentID != "" {
		if parent, ok := s.beads[parentID]; ok {
			parent.children = append(parent.children, id)
		}
	}
}

func (s *fakeStore) GetBead(id string) (BeadInfo, error) {
	if s.GetBeadFunc != nil {
		return s.GetBeadFunc(id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.beads[id]
	if !ok {
		return BeadInfo{}, fmt.Errorf("bead %q: %w", id, beads.ErrNotFound)
	}
	return rec.info, nil
}

func (s *fakeStore) GetMetadata(id string) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.beads[id]
	if !ok {
		return nil, fmt.Errorf("bead %q: %w", id, beads.ErrNotFound)
	}
	// Return a copy.
	cp := make(map[string]string, len(rec.metadata))
	for k, v := range rec.metadata {
		cp[k] = v
	}
	return cp, nil
}

func (s *fakeStore) SetMetadata(id, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.beads[id]
	if !ok {
		return fmt.Errorf("bead %q: %w", id, beads.ErrNotFound)
	}
	rec.metadata[key] = value
	s.WriteLog = append(s.WriteLog, key)
	return nil
}

func (s *fakeStore) CloseBead(id, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.beads[id]
	if !ok {
		return fmt.Errorf("bead %q: %w", id, beads.ErrNotFound)
	}
	rec.info.Status = "closed"
	rec.info.ClosedAt = time.Now()
	if reason != "" {
		if rec.metadata == nil {
			rec.metadata = map[string]string{}
		}
		rec.metadata["close_reason"] = reason
	}
	return nil
}

func (s *fakeStore) DeleteBead(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.beads[id]
	if !ok {
		return fmt.Errorf("bead %q: %w", id, beads.ErrNotFound)
	}
	if rec.info.ParentID != "" {
		if parent, ok := s.beads[rec.info.ParentID]; ok {
			filtered := parent.children[:0]
			for _, childID := range parent.children {
				if childID != id {
					filtered = append(filtered, childID)
				}
			}
			parent.children = filtered
		}
	}
	delete(s.beads, id)
	return nil
}

func (s *fakeStore) Children(parentID string) ([]BeadInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.beads[parentID]
	if !ok {
		return nil, nil
	}
	var result []BeadInfo
	for _, childID := range rec.children {
		if child, ok := s.beads[childID]; ok {
			result = append(result, child.info)
		}
	}
	return result, nil
}

func (s *fakeStore) PourWisp(parentID, formula, idempotencyKey string, vars map[string]string, evaluatePrompt string) (string, error) {
	if s.PourWispFunc != nil {
		return s.PourWispFunc(parentID, formula, idempotencyKey, vars, evaluatePrompt)
	}
	return s.pourWisp(parentID, idempotencyKey)
}

func (s *fakeStore) PourSpeculativeWisp(parentID, formula, idempotencyKey string, vars map[string]string, evaluatePrompt string) (string, error) {
	if s.PourSpeculativeWispFunc != nil {
		return s.PourSpeculativeWispFunc(parentID, formula, idempotencyKey, vars, evaluatePrompt)
	}
	return s.pourWisp(parentID, idempotencyKey)
}

func (s *fakeStore) pourWisp(parentID, idempotencyKey string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Check for existing wisp with this key (idempotent).
	for _, rec := range s.beads {
		if rec.info.IdempotencyKey == idempotencyKey {
			return rec.info.ID, nil
		}
	}

	s.pourCounter++
	wispID := fmt.Sprintf("wisp-%d", s.pourCounter)
	s.beads[wispID] = &fakeBeadRecord{
		info: BeadInfo{
			ID:             wispID,
			Status:         "in_progress",
			ParentID:       parentID,
			IdempotencyKey: idempotencyKey,
			CreatedAt:      time.Now(),
		},
		metadata: make(map[string]string),
	}
	// Add as child.
	if parent, ok := s.beads[parentID]; ok {
		parent.children = append(parent.children, wispID)
	}
	return wispID, nil
}

func (s *fakeStore) FindByIdempotencyKey(key string) (string, bool, error) {
	if s.FindByIdempotencyKeyFunc != nil {
		return s.FindByIdempotencyKeyFunc(key)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, rec := range s.beads {
		if rec.info.IdempotencyKey == key {
			return rec.info.ID, true, nil
		}
	}
	return "", false, nil
}

func (s *fakeStore) ActivateWisp(id string) error {
	if s.ActivateWispFunc != nil {
		return s.ActivateWispFunc(id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.beads[id]; !ok {
		return fmt.Errorf("bead %q: %w", id, beads.ErrNotFound)
	}
	s.ActivatedWispIDs = append(s.ActivatedWispIDs, id)
	return nil
}

func (s *fakeStore) CountActiveConvergenceLoops(targetAgent string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, rec := range s.beads {
		if rec.metadata[FieldTarget] == targetAgent &&
			rec.metadata[FieldState] == StateActive {
			count++
		}
	}
	return count, nil
}

func (s *fakeStore) CreateConvergenceBead(_ string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pourCounter++
	id := fmt.Sprintf("conv-%d", s.pourCounter)
	s.beads[id] = &fakeBeadRecord{
		info: BeadInfo{
			ID:        id,
			Status:    "in_progress",
			CreatedAt: time.Now(),
		},
		metadata: map[string]string{},
	}
	return id, nil
}

// --- Fake EventEmitter ---

type emittedEvent struct {
	Type     string
	EventID  string
	BeadID   string
	Payload  json.RawMessage
	Recovery bool
}

type fakeEmitter struct {
	mu     sync.Mutex
	events []emittedEvent
}

func (e *fakeEmitter) Emit(eventType, eventID, beadID string, payload json.RawMessage, recovery bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.events = append(e.events, emittedEvent{
		Type:     eventType,
		EventID:  eventID,
		BeadID:   beadID,
		Payload:  payload,
		Recovery: recovery,
	})
}

func (e *fakeEmitter) findEvent(eventType string) (emittedEvent, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, ev := range e.events {
		if ev.Type == eventType {
			return ev, true
		}
	}
	return emittedEvent{}, false
}

// --- Test Helpers ---

// setupBasicHandler creates a handler with a fake store and emitter,
// and a root bead with the given metadata plus a closed wisp for iteration 1.
func setupBasicHandler(t *testing.T, meta map[string]string) (*Handler, *fakeStore, *fakeEmitter) {
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
	}
	for k, v := range meta {
		rootMeta[k] = v
	}

	store.addBead("root-1", "in_progress", "", "", rootMeta)
	store.addBead("wisp-iter-1", "closed", "root-1",
		IdempotencyKey("root-1", 1), nil)

	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}

	return handler, store, emitter
}

// --- Tests ---

func TestParseIterationFromKey(t *testing.T) {
	tests := []struct {
		key      string
		wantIter int
		wantOK   bool
	}{
		{"converge:root-1:iter:1", 1, true},
		{"converge:root-1:iter:5", 5, true},
		{"converge:root-1:iter:0", 0, true},
		{"converge:root-1:iter:abc", 0, false},
		{"converge:root-1:iter:", 0, false},
		{"no-iter-marker", 0, false},
		{"", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			got, ok := ParseIterationFromKey(tt.key)
			if ok != tt.wantOK {
				t.Errorf("ParseIterationFromKey(%q) ok = %v, want %v", tt.key, ok, tt.wantOK)
			}
			if got != tt.wantIter {
				t.Errorf("ParseIterationFromKey(%q) = %d, want %d", tt.key, got, tt.wantIter)
			}
		})
	}
}

func TestIdempotencyKey(t *testing.T) {
	key := IdempotencyKey("bead-42", 3)
	want := "converge:bead-42:iter:3"
	if key != want {
		t.Errorf("IdempotencyKey = %q, want %q", key, want)
	}

	iter, ok := ParseIterationFromKey(key)
	if !ok || iter != 3 {
		t.Errorf("round-trip: got iter=%d, ok=%v", iter, ok)
	}
}

func TestHandleWispClosed_GuardCheck_Terminated(t *testing.T) {
	handler, _, _ := setupBasicHandler(t, map[string]string{
		FieldState: StateTerminated,
	})

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionSkipped {
		t.Errorf("Action = %q, want %q", result.Action, ActionSkipped)
	}
}

func TestHandleWispClosed_DedupCheck_AlreadyProcessed(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldLastProcessedWisp: "wisp-iter-1",
	})

	// The last processed wisp is wisp-iter-1 (iteration 1).
	// Processing wisp-iter-1 again should be skipped.
	_ = store // already set up

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionSkipped {
		t.Errorf("Action = %q, want %q", result.Action, ActionSkipped)
	}
}

func TestHandleWispClosed_CorruptedLastProcessedWisp_GracefulDegradation(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldLastProcessedWisp: "deleted-wisp",
		FieldGateMode:          GateModeManual,
	})

	// The last processed wisp reference points to a bead that doesn't
	// exist. The handler should degrade gracefully (treat as iteration 0)
	// instead of permanently blocking the loop.
	_ = store

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("expected graceful degradation, got error: %v", err)
	}
	// Should process normally (not skip), since the corrupted reference
	// is treated as "no previous iteration".
	if result.Action == ActionSkipped {
		t.Error("should not skip when last_processed_wisp is corrupted")
	}
}

func TestHandleWispClosed_ManualGate_WaitingManual(t *testing.T) {
	handler, store, emitter := setupBasicHandler(t, map[string]string{
		FieldGateMode: GateModeManual,
	})
	_ = store

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionWaitingManual {
		t.Errorf("Action = %q, want %q", result.Action, ActionWaitingManual)
	}
	if result.WaitingReason != WaitManual {
		t.Errorf("WaitingReason = %q, want %q", result.WaitingReason, WaitManual)
	}

	// Verify state was set to waiting_manual.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateWaitingManual {
		t.Errorf("state = %q, want %q", meta[FieldState], StateWaitingManual)
	}
	if meta[FieldWaitingReason] != WaitManual {
		t.Errorf("waiting_reason = %q, want %q", meta[FieldWaitingReason], WaitManual)
	}

	// Verify events were emitted.
	if _, ok := emitter.findEvent(EventIteration); !ok {
		t.Error("expected ConvergenceIteration event")
	}
	if _, ok := emitter.findEvent(EventWaitingManual); !ok {
		t.Error("expected ConvergenceWaitingManual event")
	}
}

func TestHandleWispClosed_HybridNoCondition_WaitingManual(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateMode:      GateModeHybrid,
		FieldGateCondition: "", // no condition configured
	})
	_ = store

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionWaitingManual {
		t.Errorf("Action = %q, want %q", result.Action, ActionWaitingManual)
	}
	if result.WaitingReason != WaitHybridNoCondition {
		t.Errorf("WaitingReason = %q, want %q", result.WaitingReason, WaitHybridNoCondition)
	}
}

func TestHandleWispClosed_GateReplay_SkipsReEvaluation(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GateFail,
		FieldGateRetryCount:  "0",
	})
	_ = store

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Gate was fail, under max (1 < 5), should iterate.
	if result.Action != ActionIterate {
		t.Errorf("Action = %q, want %q", result.Action, ActionIterate)
	}
	if result.GateOutcome != GateFail {
		t.Errorf("GateOutcome = %q, want %q", result.GateOutcome, GateFail)
	}
}

func TestHandleWispClosed_GatePassApproved(t *testing.T) {
	handler, store, emitter := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GatePass,
	})
	_ = store

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionApproved {
		t.Errorf("Action = %q, want %q", result.Action, ActionApproved)
	}

	// Verify terminal state.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
	if meta[FieldTerminalReason] != TerminalApproved {
		t.Errorf("terminal_reason = %q, want %q", meta[FieldTerminalReason], TerminalApproved)
	}
	if meta[FieldTerminalActor] != "controller" {
		t.Errorf("terminal_actor = %q, want %q", meta[FieldTerminalActor], "controller")
	}

	// Verify bead is closed.
	beadInfo, _ := store.GetBead("root-1")
	if beadInfo.Status != "closed" {
		t.Errorf("bead status = %q, want %q", beadInfo.Status, "closed")
	}

	// Verify both events emitted.
	if _, ok := emitter.findEvent(EventIteration); !ok {
		t.Error("expected ConvergenceIteration event")
	}
	if _, ok := emitter.findEvent(EventTerminated); !ok {
		t.Error("expected ConvergenceTerminated event")
	}
}

func TestHandleWispClosed_GateFailIterate(t *testing.T) {
	handler, store, emitter := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GateFail,
	})
	_ = store

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionIterate {
		t.Errorf("Action = %q, want %q", result.Action, ActionIterate)
	}
	if result.NextWispID == "" {
		t.Error("expected NextWispID to be set")
	}

	// Verify active_wisp is set to the new wisp.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] != result.NextWispID {
		t.Errorf("active_wisp = %q, want %q", meta[FieldActiveWisp], result.NextWispID)
	}

	// Verify ConvergenceIteration event has next_wisp_id.
	ev, ok := emitter.findEvent(EventIteration)
	if !ok {
		t.Fatal("expected ConvergenceIteration event")
	}
	var payload IterationPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.NextWispID == nil || *payload.NextWispID != result.NextWispID {
		t.Errorf("event next_wisp_id = %v, want %q", payload.NextWispID, result.NextWispID)
	}
	if payload.Action != "iterate" {
		t.Errorf("event action = %q, want %q", payload.Action, "iterate")
	}
}

func TestHandleWispClosed_MaxIterationsReached_NoConvergence(t *testing.T) {
	handler, store, emitter := setupBasicHandler(t, map[string]string{
		FieldMaxIterations:   "1", // max is 1, and we're processing iteration 1
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GateFail,
	})
	_ = store

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionNoConvergence {
		t.Errorf("Action = %q, want %q", result.Action, ActionNoConvergence)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldTerminalReason] != TerminalNoConvergence {
		t.Errorf("terminal_reason = %q, want %q", meta[FieldTerminalReason], TerminalNoConvergence)
	}

	if _, ok := emitter.findEvent(EventTerminated); !ok {
		t.Error("expected ConvergenceTerminated event")
	}
}

func TestHandleWispClosed_TimeoutTerminate(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateTimeoutAction: TimeoutActionTerminate,
		FieldGateOutcomeWisp:   "wisp-iter-1",
		FieldGateOutcome:       GateTimeout,
	})
	_ = store

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionNoConvergence {
		t.Errorf("Action = %q, want %q", result.Action, ActionNoConvergence)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldTerminalReason] != TerminalNoConvergence {
		t.Errorf("terminal_reason = %q, want %q", meta[FieldTerminalReason], TerminalNoConvergence)
	}
}

func TestHandleWispClosed_TimeoutManual(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateTimeoutAction: TimeoutActionManual,
		FieldGateOutcomeWisp:   "wisp-iter-1",
		FieldGateOutcome:       GateTimeout,
	})
	_ = store

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionWaitingManual {
		t.Errorf("Action = %q, want %q", result.Action, ActionWaitingManual)
	}
	if result.WaitingReason != WaitTimeout {
		t.Errorf("WaitingReason = %q, want %q", result.WaitingReason, WaitTimeout)
	}
}

func TestHandleWispClosed_SlingFailure_WaitingManual(t *testing.T) {
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
		FieldGateOutcomeWisp:   "wisp-iter-1",
		FieldGateOutcome:       GateFail,
	}
	store.addBead("root-1", "in_progress", "", "", rootMeta)
	store.addBead("wisp-iter-1", "closed", "root-1",
		IdempotencyKey("root-1", 1), nil)

	// Simulate speculative PourWisp failure on a nonterminal gate outcome.
	store.PourSpeculativeWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		return "", fmt.Errorf("sling failure: connection refused")
	}

	handler := &Handler{Store: store, Emitter: emitter}

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionWaitingManual {
		t.Errorf("Action = %q, want %q", result.Action, ActionWaitingManual)
	}
	if result.WaitingReason != WaitSlingFailure {
		t.Errorf("WaitingReason = %q, want %q", result.WaitingReason, WaitSlingFailure)
	}

	// Verify waiting_reason was persisted.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldWaitingReason] != WaitSlingFailure {
		t.Errorf("waiting_reason = %q, want %q", meta[FieldWaitingReason], WaitSlingFailure)
	}
}

func TestHandleWispClosed_VerdictClearedOnIterate(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp:  "wisp-iter-1",
		FieldGateOutcome:      GateFail,
		FieldAgentVerdict:     "block",
		FieldAgentVerdictWisp: "wisp-iter-1",
	})

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionIterate {
		t.Fatalf("Action = %q, want %q", result.Action, ActionIterate)
	}

	// Verdict should be cleared for next iteration.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldAgentVerdict] != "" {
		t.Errorf("agent_verdict should be cleared, got %q", meta[FieldAgentVerdict])
	}
	if meta[FieldAgentVerdictWisp] != "" {
		t.Errorf("agent_verdict_wisp should be cleared, got %q", meta[FieldAgentVerdictWisp])
	}
}

func TestHandleWispClosed_VerdictPreservedForLaterWisp(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp:  "wisp-iter-1",
		FieldGateOutcome:      GateFail,
		FieldAgentVerdict:     "approve",
		FieldAgentVerdictWisp: "wisp-iter-2", // belongs to a LATER wisp
	})

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionIterate {
		t.Fatalf("Action = %q, want %q", result.Action, ActionIterate)
	}

	// Verdict should NOT be cleared (belongs to later wisp).
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldAgentVerdict] != "approve" {
		t.Errorf("agent_verdict should be preserved, got %q", meta[FieldAgentVerdict])
	}
}

func TestHandleWispClosed_WriteOrdering_TerminalReasonBeforeState(t *testing.T) {
	// Verify that terminal_reason and terminal_actor are written
	// before state=terminated (write ordering contract).
	store := newFakeStore()
	emitter := &fakeEmitter{}

	rootMeta := map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldGateMode:          GateModeCondition,
		FieldGateTimeout:       "60s",
		FieldGateTimeoutAction: TimeoutActionIterate,
		FieldGateOutcomeWisp:   "wisp-iter-1",
		FieldGateOutcome:       GatePass,
	}
	store.addBead("root-1", "in_progress", "", "", rootMeta)
	store.addBead("wisp-iter-1", "closed", "root-1",
		IdempotencyKey("root-1", 1), nil)

	handler := &Handler{Store: store, Emitter: emitter}
	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionApproved {
		t.Fatalf("Action = %q, want %q", result.Action, ActionApproved)
	}

	// All terminal fields should be set.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldTerminalReason] != TerminalApproved {
		t.Errorf("terminal_reason = %q, want %q", meta[FieldTerminalReason], TerminalApproved)
	}
	if meta[FieldTerminalActor] != "controller" {
		t.Errorf("terminal_actor = %q, want %q", meta[FieldTerminalActor], "controller")
	}
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}

	// Verify actual write ordering via the write log.
	// The commit sequence must be: terminal_reason, terminal_actor,
	// state=terminated, then last_processed_wisp LAST (dedup marker).
	commitKeys := extractCommitKeys(store.WriteLog)
	expectedOrder := []string{
		FieldTerminalReason,
		FieldTerminalActor,
		FieldState,
		FieldLastProcessedWisp,
	}
	if len(commitKeys) < len(expectedOrder) {
		t.Fatalf("WriteLog has %d commit keys, want at least %d: %v", len(commitKeys), len(expectedOrder), commitKeys)
	}
	// last_processed_wisp must be the very last write.
	lastKey := store.WriteLog[len(store.WriteLog)-1]
	if lastKey != FieldLastProcessedWisp {
		t.Errorf("last write = %q, want %q (dedup marker must be last)", lastKey, FieldLastProcessedWisp)
	}
	// terminal_reason and terminal_actor must appear before state.
	reasonIdx, actorIdx, stateIdx := -1, -1, -1
	for i, key := range commitKeys {
		switch key {
		case FieldTerminalReason:
			reasonIdx = i
		case FieldTerminalActor:
			actorIdx = i
		case FieldState:
			stateIdx = i
		}
	}
	if reasonIdx >= stateIdx {
		t.Errorf("terminal_reason (idx %d) must be written before state (idx %d)", reasonIdx, stateIdx)
	}
	if actorIdx >= stateIdx {
		t.Errorf("terminal_actor (idx %d) must be written before state (idx %d)", actorIdx, stateIdx)
	}
}

func TestHandleWispClosed_WriteOrdering_IterateLastProcessedBeforePendingCleanup(t *testing.T) {
	// Verify last_processed_wisp remains the final load-bearing write in the
	// iterate path; pending_next_wisp cleanup is best-effort after commit.
	store := newFakeStore()
	emitter := &fakeEmitter{}

	rootMeta := map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldGateMode:          GateModeCondition,
		FieldGateTimeout:       "60s",
		FieldGateTimeoutAction: TimeoutActionIterate,
		FieldGateOutcomeWisp:   "wisp-iter-1",
		FieldGateOutcome:       GateFail,
		FieldGateExitCode:      "1",
	}
	store.addBead("root-1", "in_progress", "", "", rootMeta)
	store.addBead("wisp-iter-1", "closed", "root-1",
		IdempotencyKey("root-1", 1), nil)

	handler := &Handler{Store: store, Emitter: emitter}
	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionIterate {
		t.Fatalf("Action = %q, want %q", result.Action, ActionIterate)
	}

	commitKeys := extractCommitKeys(store.WriteLog)
	if len(commitKeys) < 2 {
		t.Fatalf("commit log too short: %v", commitKeys)
	}
	if commitKeys[len(commitKeys)-2] != FieldLastProcessedWisp {
		t.Errorf("last_processed_wisp should precede pending cleanup, log=%v", commitKeys)
	}
	if commitKeys[len(commitKeys)-1] != FieldPendingNextWisp {
		t.Errorf("pending_next_wisp cleanup should be last, log=%v", commitKeys)
	}
}

func TestHandleWispClosed_WriteOrdering_WaitingManualLastProcessedWispLast(t *testing.T) {
	// Verify last_processed_wisp is the final write in the waiting_manual path.
	store := newFakeStore()
	emitter := &fakeEmitter{}

	rootMeta := map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldGateMode:          GateModeManual,
		FieldGateTimeout:       "60s",
		FieldGateTimeoutAction: TimeoutActionManual,
	}
	store.addBead("root-1", "in_progress", "", "", rootMeta)
	store.addBead("wisp-iter-1", "closed", "root-1",
		IdempotencyKey("root-1", 1), nil)

	handler := &Handler{Store: store, Emitter: emitter}
	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionWaitingManual {
		t.Fatalf("Action = %q, want %q", result.Action, ActionWaitingManual)
	}

	// last_processed_wisp must be the very last write.
	lastKey := store.WriteLog[len(store.WriteLog)-1]
	if lastKey != FieldLastProcessedWisp {
		t.Errorf("last write = %q, want %q (dedup marker must be last)", lastKey, FieldLastProcessedWisp)
	}
}

func TestHandleWispClosed_EventPayloads(t *testing.T) {
	handler, store, emitter := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp:  "wisp-iter-1",
		FieldGateOutcome:      GatePass,
		FieldAgentVerdict:     "approve",
		FieldAgentVerdictWisp: "wisp-iter-1",
	})
	_ = store

	_, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Check ConvergenceIteration event.
	iterEv, ok := emitter.findEvent(EventIteration)
	if !ok {
		t.Fatal("expected ConvergenceIteration event")
	}
	if iterEv.EventID != EventIDIteration("root-1", 1) {
		t.Errorf("event_id = %q, want %q", iterEv.EventID, EventIDIteration("root-1", 1))
	}

	var iterPayload IterationPayload
	if err := json.Unmarshal(iterEv.Payload, &iterPayload); err != nil {
		t.Fatalf("unmarshal iteration payload: %v", err)
	}
	if iterPayload.Iteration != 1 {
		t.Errorf("iteration = %d, want 1", iterPayload.Iteration)
	}
	if iterPayload.WispID != "wisp-iter-1" {
		t.Errorf("wisp_id = %q, want %q", iterPayload.WispID, "wisp-iter-1")
	}
	if iterPayload.Action != "approved" {
		t.Errorf("action = %q, want %q", iterPayload.Action, "approved")
	}
	if iterPayload.AgentVerdict != "approve" {
		t.Errorf("agent_verdict = %q, want %q", iterPayload.AgentVerdict, "approve")
	}

	// Check ConvergenceTerminated event.
	termEv, ok := emitter.findEvent(EventTerminated)
	if !ok {
		t.Fatal("expected ConvergenceTerminated event")
	}
	if termEv.EventID != EventIDTerminated("root-1") {
		t.Errorf("event_id = %q, want %q", termEv.EventID, EventIDTerminated("root-1"))
	}

	var termPayload TerminatedPayload
	if err := json.Unmarshal(termEv.Payload, &termPayload); err != nil {
		t.Fatalf("unmarshal terminated payload: %v", err)
	}
	if termPayload.TerminalReason != TerminalApproved {
		t.Errorf("terminal_reason = %q, want %q", termPayload.TerminalReason, TerminalApproved)
	}
	if termPayload.FinalStatus != "closed" {
		t.Errorf("final_status = %q, want %q", termPayload.FinalStatus, "closed")
	}
	if termPayload.Actor != "controller" {
		t.Errorf("actor = %q, want %q", termPayload.Actor, "controller")
	}
}

func TestCheckNestedConvergence_Blocked(t *testing.T) {
	store := newFakeStore()
	store.addBead("loop-1", "in_progress", "", "", map[string]string{
		FieldTarget: "agent-a",
		FieldState:  StateActive,
	})

	err := CheckNestedConvergence(store, "agent-a", "agent-a")
	if err == nil {
		t.Fatal("expected error for self-targeting nested convergence")
	}
	if got := err.Error(); !contains(got, "deadlock") {
		t.Errorf("error should mention deadlock, got: %v", err)
	}
}

func TestCheckNestedConvergence_Allowed_DifferentAgent(t *testing.T) {
	store := newFakeStore()
	store.addBead("loop-1", "in_progress", "", "", map[string]string{
		FieldTarget: "agent-a",
		FieldState:  StateActive,
	})

	// Cross-agent convergence is always allowed (no self-deadlock risk).
	err := CheckNestedConvergence(store, "agent-b", "agent-a")
	if err != nil {
		t.Errorf("expected no error for different agent, got: %v", err)
	}
}

func TestCheckNestedConvergence_CrossAgent_TargetHasActiveLoops(t *testing.T) {
	store := newFakeStore()
	store.addBead("loop-1", "in_progress", "", "", map[string]string{
		FieldTarget: "agent-a",
		FieldState:  StateActive,
	})

	// Cross-agent: agent-b creating a loop targeting agent-a should
	// succeed even though agent-a has an active loop (no self-deadlock).
	err := CheckNestedConvergence(store, "agent-b", "agent-a")
	if err != nil {
		t.Errorf("cross-agent convergence should be allowed, got: %v", err)
	}
}

func TestCheckConcurrencyLimits_Exceeded(t *testing.T) {
	store := newFakeStore()
	store.addBead("loop-1", "in_progress", "", "", map[string]string{
		FieldTarget: "agent-a",
		FieldState:  StateActive,
	})
	store.addBead("loop-2", "in_progress", "", "", map[string]string{
		FieldTarget: "agent-a",
		FieldState:  StateActive,
	})

	err := CheckConcurrencyLimits(store, "agent-a", 2)
	if err == nil {
		t.Fatal("expected error for exceeded per-agent limit")
	}
	if got := err.Error(); !contains(got, "per-agent limit") {
		t.Errorf("error should mention per-agent limit, got: %v", err)
	}
}

func TestCheckConcurrencyLimits_OK(t *testing.T) {
	store := newFakeStore()
	store.addBead("loop-1", "in_progress", "", "", map[string]string{
		FieldTarget: "agent-a",
		FieldState:  StateActive,
	})

	err := CheckConcurrencyLimits(store, "agent-a", 2)
	if err != nil {
		t.Errorf("expected no error, got: %v", err)
	}
}

func TestEventIDFormulas(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"created", EventIDCreated("gc-conv-42"), "converge:gc-conv-42:created"},
		{"iteration", EventIDIteration("gc-conv-42", 3), "converge:gc-conv-42:iter:3:iteration"},
		{"waiting_manual", EventIDWaitingManual("gc-conv-42", 3), "converge:gc-conv-42:iter:3:waiting_manual"},
		{"terminated", EventIDTerminated("gc-conv-42"), "converge:gc-conv-42:terminated"},
		{"manual_approve", EventIDManualApprove("gc-conv-42"), "converge:gc-conv-42:manual_approve"},
		{"manual_iterate", EventIDManualIterate("gc-conv-42", 4), "converge:gc-conv-42:iter:4:manual_iterate"},
		{"manual_stop", EventIDManualStop("gc-conv-42"), "converge:gc-conv-42:manual_stop"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("got %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestNullableString(t *testing.T) {
	if got := NullableString(""); got != nil {
		t.Errorf("NullableString(\"\") = %v, want nil", got)
	}
	if got := NullableString("hello"); got == nil || *got != "hello" {
		t.Errorf("NullableString(\"hello\") = %v, want \"hello\"", got)
	}
}

func TestGateResultToPayload(t *testing.T) {
	// Empty outcome (manual mode) returns nil.
	result := GateResult{}
	if got := GateResultToPayload(result); got != nil {
		t.Errorf("expected nil for empty outcome, got %v", got)
	}

	// Non-empty outcome returns payload.
	code := 1
	result = GateResult{
		Outcome:   GateFail,
		ExitCode:  &code,
		Stdout:    "output",
		Stderr:    "error",
		Duration:  5 * time.Second,
		Truncated: true,
	}
	got := GateResultToPayload(result)
	if got == nil {
		t.Fatal("expected non-nil payload")
	}
	if got.ExitCode == nil || *got.ExitCode != 1 {
		t.Errorf("exit_code = %v, want 1", got.ExitCode)
	}
	if got.DurationMs != 5000 {
		t.Errorf("duration_ms = %d, want 5000", got.DurationMs)
	}
	if !got.Truncated {
		t.Error("truncated should be true")
	}
}

// extractCommitKeys filters a write log to only the Step 9 commit keys
// (state, terminal_reason, terminal_actor, last_processed_wisp, etc.).
func extractCommitKeys(log []string) []string {
	commitKeys := map[string]bool{
		FieldState:             true,
		FieldTerminalReason:    true,
		FieldTerminalActor:     true,
		FieldLastProcessedWisp: true,
		FieldActiveWisp:        true,
		FieldWaitingReason:     true,
		FieldIteration:         true,
		FieldRetrySource:       true,
		FieldPendingNextWisp:   true,
	}
	var result []string
	for _, key := range log {
		if commitKeys[key] {
			result = append(result, key)
		}
	}
	return result
}

// contains is a test helper for substring matching.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- Crash-safety (speculative pour) tests ---

func TestHandleWispClosed_SpeculativePour_WispExistsBeforeGateEval(t *testing.T) {
	// Verify that when HandleWispClosed processes a non-terminal gate outcome
	// (fail, below max), the next wisp is speculatively poured BEFORE gate
	// evaluation and adopted in iterate().
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GateFail,
	})

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionIterate {
		t.Fatalf("Action = %q, want %q", result.Action, ActionIterate)
	}
	if result.NextWispID == "" {
		t.Fatal("expected NextWispID to be set")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] != result.NextWispID {
		t.Errorf("active_wisp = %q, want %q", meta[FieldActiveWisp], result.NextWispID)
	}
	if meta[FieldPendingNextWisp] != "" {
		t.Errorf("pending_next_wisp should be cleared, got %q", meta[FieldPendingNextWisp])
	}
}

func TestHandleWispClosed_SpeculativePourFailureStillAllowsTerminalGate(t *testing.T) {
	dir := t.TempDir()
	gatePath := filepath.Join(dir, "pass.sh")
	if err := os.WriteFile(gatePath, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing gate script: %v", err)
	}

	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateCondition: gatePath,
	})
	store.PourSpeculativeWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		return "", fmt.Errorf("transient speculative pour failure")
	}

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionApproved {
		t.Fatalf("Action = %q, want %q", result.Action, ActionApproved)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Fatalf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
	if meta[FieldWaitingReason] == WaitSlingFailure {
		t.Fatalf("waiting_reason = %q, should not enter sling failure when gate passes", meta[FieldWaitingReason])
	}
}

func TestHandleWispClosed_InvalidConditionDoesNotBurnUnvalidatedPendingWisp(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldPendingNextWisp: "other-wisp",
	})
	store.addBead("other-root", "in_progress", "", "", nil)
	store.addBead("other-wisp", "in_progress", "other-root",
		IdempotencyKey("other-root", 2), nil)

	_, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err == nil {
		t.Fatal("expected missing condition error")
	}
	if _, err := store.GetBead("other-wisp"); err != nil {
		t.Fatalf("stale pending wisp from another parent was deleted: %v", err)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldPendingNextWisp] != "" {
		t.Fatalf("pending_next_wisp = %q, want cleared", meta[FieldPendingNextWisp])
	}
}

func TestHandleWispClosed_SpeculativePourDeletedOnTerminal(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GatePass,
	})

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionApproved {
		t.Fatalf("Action = %q, want %q", result.Action, ActionApproved)
	}

	iter2Key := IdempotencyKey("root-1", 2)
	_, found, _ := store.FindByIdempotencyKey(iter2Key)
	if found {
		t.Fatal("speculative wisp for iteration 2 should be deleted")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldPendingNextWisp] != "" {
		t.Errorf("pending_next_wisp should be cleared, got %q", meta[FieldPendingNextWisp])
	}
}

func TestHandleWispClosed_IterateActivatesSpeculativeWispBeforeCommit(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GateFail,
	})

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionIterate {
		t.Fatalf("Action = %q, want %q", result.Action, ActionIterate)
	}
	if len(store.ActivatedWispIDs) != 1 || store.ActivatedWispIDs[0] != result.NextWispID {
		t.Fatalf("activated wisps = %v, want [%s]", store.ActivatedWispIDs, result.NextWispID)
	}

	commitLog := extractCommitKeys(store.WriteLog)
	if len(commitLog) < 2 {
		t.Fatalf("commit log too short: %v", commitLog)
	}
	if commitLog[len(commitLog)-2] != FieldLastProcessedWisp {
		t.Fatalf("last_processed_wisp should be the final load-bearing commit write, log=%v", commitLog)
	}
	if commitLog[len(commitLog)-1] != FieldPendingNextWisp {
		t.Fatalf("pending_next_wisp should clear only after last_processed_wisp, log=%v", commitLog)
	}
}

func TestHandleWispClosed_NoSpeculativePourOnWaitingManual(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateMode: GateModeManual,
	})

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionWaitingManual {
		t.Fatalf("Action = %q, want %q", result.Action, ActionWaitingManual)
	}

	iter2Key := IdempotencyKey("root-1", 2)
	_, found, _ := store.FindByIdempotencyKey(iter2Key)
	if found {
		t.Fatal("manual gate should not pour a speculative wisp")
	}
}

func TestHandleWispClosed_ManualThenIterateUsesNextSequentialIteration(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateMode: GateModeManual,
	})

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("HandleWispClosed: %v", err)
	}
	if result.Action != ActionWaitingManual {
		t.Fatalf("Action = %q, want %q", result.Action, ActionWaitingManual)
	}

	result, err = handler.IterateHandler(context.Background(), "root-1", "operator", "")
	if err != nil {
		t.Fatalf("IterateHandler: %v", err)
	}
	if result.Iteration != 2 {
		t.Fatalf("Iteration = %d, want 2", result.Iteration)
	}
	iter2Info, err := store.GetBead(result.NextWispID)
	if err != nil {
		t.Fatalf("reading next wisp: %v", err)
	}
	if iter2Info.IdempotencyKey != IdempotencyKey("root-1", 2) {
		t.Fatalf("next wisp key = %q, want %q", iter2Info.IdempotencyKey, IdempotencyKey("root-1", 2))
	}
}

func TestHandleWispClosed_NoSpeculativePourAtMaxIterations(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldMaxIterations:   "1",
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GateFail,
	})

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Action != ActionNoConvergence {
		t.Fatalf("Action = %q, want %q", result.Action, ActionNoConvergence)
	}

	iter2Key := IdempotencyKey("root-1", 2)
	_, found, _ := store.FindByIdempotencyKey(iter2Key)
	if found {
		t.Error("should not pour speculative wisp at max iterations")
	}
}

func TestCrashAfterSpeculativePour_ReconcilerRecoversChain(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}

	rootMeta := map[string]string{
		FieldState:           StateActive,
		FieldIteration:       "1",
		FieldMaxIterations:   "5",
		FieldFormula:         "test-formula",
		FieldTarget:          "test-agent",
		FieldGateMode:        GateModeManual,
		FieldActiveWisp:      "wisp-iter-1",
		FieldPendingNextWisp: "wisp-iter-2",
	}
	store.addBead("root-1", "in_progress", "", "", rootMeta)
	store.addBead("wisp-iter-1", "closed", "root-1",
		IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-iter-2", "in_progress", "root-1",
		IdempotencyKey("root-1", 2), nil)

	handler := &Handler{Store: store, Emitter: emitter}
	reconciler := &Reconciler{Handler: handler}

	report, err := reconciler.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("reconciliation error: %v", err)
	}
	if report.Errors > 0 {
		var errMsgs []string
		for _, d := range report.Details {
			if d.Error != nil {
				errMsgs = append(errMsgs, d.Error.Error())
			}
		}
		t.Fatalf("reconciliation had %d errors: %v", report.Errors, errMsgs)
	}

	meta, _ := store.GetMetadata("root-1")
	state := meta[FieldState]
	if state != StateWaitingManual {
		t.Errorf("state after reconciliation = %q, want %q", state, StateWaitingManual)
	}
}

func TestCrashAfterSpeculativePour_NoActiveWisp_ReconcilerAdoptsSpeculative(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}

	rootMeta := map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeManual,
		FieldActiveWisp:        "",
		FieldLastProcessedWisp: "wisp-iter-1",
		FieldPendingNextWisp:   "wisp-iter-2",
	}
	store.addBead("root-1", "in_progress", "", "", rootMeta)
	store.addBead("wisp-iter-1", "closed", "root-1",
		IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-iter-2", "in_progress", "root-1",
		IdempotencyKey("root-1", 2), nil)

	handler := &Handler{Store: store, Emitter: emitter}
	reconciler := &Reconciler{Handler: handler}

	report, err := reconciler.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("reconciliation error: %v", err)
	}
	if report.Errors > 0 {
		t.Fatalf("reconciliation had %d errors", report.Errors)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] != "wisp-iter-2" {
		t.Errorf("active_wisp = %q, want %q", meta[FieldActiveWisp], "wisp-iter-2")
	}
	if meta[FieldState] != StateActive {
		t.Errorf("state = %q, want %q", meta[FieldState], StateActive)
	}
}

func TestCrashAfterSpeculativePour_ReconcilerUsesPendingNextWispBeforeLookup(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}

	rootMeta := map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldActiveWisp:        "",
		FieldLastProcessedWisp: "wisp-iter-1",
		FieldPendingNextWisp:   "wisp-iter-2",
	}
	store.addBead("root-1", "in_progress", "", "", rootMeta)
	store.addBead("wisp-iter-1", "closed", "root-1",
		IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-iter-2", "in_progress", "root-1",
		IdempotencyKey("root-1", 2), nil)
	store.FindByIdempotencyKeyFunc = func(key string) (string, bool, error) {
		return "", false, fmt.Errorf("FindByIdempotencyKey should not be called for valid pending %q", key)
	}

	handler := &Handler{Store: store, Emitter: emitter}
	reconciler := &Reconciler{Handler: handler}

	report, err := reconciler.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("reconciliation error: %v", err)
	}
	if report.Errors > 0 {
		t.Fatalf("reconciliation had %d errors: %+v", report.Errors, report.Details)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] != "wisp-iter-2" {
		t.Fatalf("active_wisp = %q, want wisp-iter-2", meta[FieldActiveWisp])
	}
	if len(store.ActivatedWispIDs) != 1 || store.ActivatedWispIDs[0] != "wisp-iter-2" {
		t.Fatalf("activated wisps = %v, want [wisp-iter-2]", store.ActivatedWispIDs)
	}
}
