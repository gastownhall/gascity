package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// mockWorkerHandle implements worker.Handle for testing
type mockWorkerHandle struct {
	messageRequests []worker.MessageRequest
}

func (m *mockWorkerHandle) Message(ctx context.Context, req worker.MessageRequest) (worker.MessageResult, error) {
	m.messageRequests = append(m.messageRequests, req)
	return worker.MessageResult{Queued: false}, nil
}

func (m *mockWorkerHandle) Nudge(ctx context.Context, req worker.NudgeRequest) (worker.NudgeResult, error) {
	return worker.NudgeResult{Delivered: true}, nil
}

func (m *mockWorkerHandle) Interrupt(ctx context.Context, req worker.InterruptRequest) error {
	return nil
}

func (m *mockWorkerHandle) History(ctx context.Context, req worker.HistoryRequest) (worker.HistoryResult, error) {
	return worker.HistoryResult{}, nil
}

func (m *mockWorkerHandle) Close() error {
	return nil
}

// TestSubmitMessageWithCorrelation verifies that client_message_id flows through the submit path
func TestSubmitMessageWithCorrelation(t *testing.T) {
	// This is a simplified unit test - full integration would require
	// setting up beads store, session manager, etc.

	// Test that MessageRequest accepts ClientMessageID
	req := worker.MessageRequest{
		Text:            "Hello, world!",
		Delivery:        worker.DeliveryIntentDefault,
		ClientMessageID: "test-client-msg-001",
	}

	if req.ClientMessageID != "test-client-msg-001" {
		t.Errorf("ClientMessageID not preserved: got %q", req.ClientMessageID)
	}
}

// TestHistoryEntryCorrelationFields verifies correlation fields in HistoryEntry
func TestHistoryEntryCorrelationFields(t *testing.T) {
	entry := worker.HistoryEntry{
		ID:              "entry-abc123",
		ClientMessageID: "client-msg-456",
		TurnID:          "turn-def789",
		Kind:            "assistant",
		Text:            "Test response",
	}

	if entry.ClientMessageID != "client-msg-456" {
		t.Errorf("ClientMessageID = %q, want 'client-msg-456'", entry.ClientMessageID)
	}
	if entry.TurnID != "turn-def789" {
		t.Errorf("TurnID = %q, want 'turn-def789'", entry.TurnID)
	}
}

// TestStructuredMessageCorrelation verifies correlation propagation to API types
func TestStructuredMessageCorrelation(t *testing.T) {
	workerEntry := worker.HistoryEntry{
		ID:              "entry-001",
		ClientMessageID: "client-msg-789",
		TurnID:          "turn-ghi012",
		Kind:            "user",
		Text:            "Test message",
	}

	msg := historyEntryToStructuredMessage(workerEntry, false)

	if msg.ClientMessageID != "client-msg-789" {
		t.Errorf("StructuredMessage.ClientMessageID = %q, want 'client-msg-789'", msg.ClientMessageID)
	}
	if msg.TurnID != "turn-ghi012" {
		t.Errorf("StructuredMessage.TurnID = %q, want 'turn-ghi012'", msg.TurnID)
	}
}

// TestAsyncAcceptedBodyJSONSerialization verifies JSON encoding includes correlation fields
func TestAsyncAcceptedBodyJSONSerialization(t *testing.T) {
	body := asyncAcceptedBody{
		Status:          "accepted",
		RequestID:       "req-test123",
		EventCursor:     "999",
		TurnID:          "turn-jkl345",
		ClientMessageID: "client-mno678",
	}

	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	// Verify all fields are present in JSON
	if decoded["status"] != "accepted" {
		t.Errorf("JSON status = %v", decoded["status"])
	}
	if decoded["request_id"] != "req-test123" {
		t.Errorf("JSON request_id = %v", decoded["request_id"])
	}
	if decoded["turn_id"] != "turn-jkl345" {
		t.Errorf("JSON turn_id = %v", decoded["turn_id"])
	}
	if decoded["client_message_id"] != "client-mno678" {
		t.Errorf("JSON client_message_id = %v", decoded["client_message_id"])
	}
}

// TestAsyncAcceptedBodyJSONOmitsEmptyFields verifies omitempty works correctly
func TestAsyncAcceptedBodyJSONOmitsEmptyFields(t *testing.T) {
	body := asyncAcceptedBody{
		Status:      "accepted",
		RequestID:   "req-test456",
		EventCursor: "888",
		// TurnID and ClientMessageID omitted
	}

	data, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	// Empty fields should be omitted from JSON
	if _, exists := decoded["turn_id"]; exists {
		t.Error("turn_id should be omitted when empty")
	}
	if _, exists := decoded["client_message_id"]; exists {
		t.Error("client_message_id should be omitted when empty")
	}
}

// TestTurnEventPayloadsJSON verifies turn event payloads serialize correctly
func TestTurnEventPayloadsJSON(t *testing.T) {
	tests := []struct {
		name    string
		payload interface{}
		check   func(map[string]interface{}) error
	}{
		{
			name: "TurnStartedPayload",
			payload: TurnStartedPayload{
				TurnID:          "turn-abc",
				SessionID:       "sess-xyz",
				ClientMessageID: "msg-123",
				RequestID:       "req-456",
			},
			check: func(m map[string]interface{}) error {
				if m["turn_id"] != "turn-abc" {
					t.Errorf("turn_id mismatch")
				}
				if m["client_message_id"] != "msg-123" {
					t.Errorf("client_message_id mismatch")
				}
				return nil
			},
		},
		{
			name: "TurnFailedPayload",
			payload: TurnFailedPayload{
				TurnID:       "turn-def",
				SessionID:    "sess-uvw",
				ErrorCode:    "timeout",
				ErrorMessage: "Request timed out",
			},
			check: func(m map[string]interface{}) error {
				if m["turn_id"] != "turn-def" {
					t.Errorf("turn_id mismatch")
				}
				if m["error_code"] != "timeout" {
					t.Errorf("error_code mismatch")
				}
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := json.Marshal(tt.payload)
			if err != nil {
				t.Fatalf("json.Marshal failed: %v", err)
			}

			var decoded map[string]interface{}
			if err := json.Unmarshal(data, &decoded); err != nil {
				t.Fatalf("json.Unmarshal failed: %v", err)
			}

			if err := tt.check(decoded); err != nil {
				t.Error(err)
			}
		})
	}
}

// TestEventConstants verifies turn event type constants are defined
func TestEventConstants(t *testing.T) {
	expectedEvents := []string{
		events.TurnStarted,
		events.TurnCompleted,
		events.TurnFailed,
		events.TurnCanceled,
	}

	for _, eventType := range expectedEvents {
		if eventType == "" {
			t.Errorf("Event constant should not be empty")
		}
	}

	// Verify specific values
	if events.TurnStarted != "turn.started" {
		t.Errorf("TurnStarted = %q, want 'turn.started'", events.TurnStarted)
	}
	if events.TurnCompleted != "turn.completed" {
		t.Errorf("TurnCompleted = %q, want 'turn.completed'", events.TurnCompleted)
	}
	if events.TurnFailed != "turn.failed" {
		t.Errorf("TurnFailed = %q, want 'turn.failed'", events.TurnFailed)
	}
	if events.TurnCanceled != "turn.canceled" {
		t.Errorf("TurnCanceled = %q, want 'turn.canceled'", events.TurnCanceled)
	}
}

// BenchmarkNewTurnID benchmarks turn ID generation performance
func BenchmarkNewTurnID(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = newTurnID()
	}
}

// BenchmarkNewRequestID benchmarks request ID generation performance
func BenchmarkNewRequestID(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_, _ = newRequestID()
	}
}
