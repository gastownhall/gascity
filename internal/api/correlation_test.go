package api

import (
	"regexp"
	"strings"
	"testing"
)

func TestNewTurnIDGeneratesUniqueIDs(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 1000; i++ {
		id, err := newTurnID()
		if err != nil {
			t.Fatalf("newTurnID() returned error: %v", err)
		}
		if seen[id] {
			t.Errorf("newTurnID() generated duplicate ID: %s", id)
		}
		seen[id] = true
	}
}

func TestNewTurnIDFormat(t *testing.T) {
	id, err := newTurnID()
	if err != nil {
		t.Fatalf("newTurnID() returned error: %v", err)
	}

	// Should start with "turn-" prefix
	if !strings.HasPrefix(id, "turn-") {
		t.Errorf("newTurnID() should start with 'turn-', got: %s", id)
	}

	// Should be turn- + 32 hex chars = 37 chars total
	if len(id) != 37 {
		t.Errorf("newTurnID() should be 37 chars (turn- + 32 hex), got %d: %s", len(id), id)
	}

	// Hex part should be valid hexadecimal
	hexPart := strings.TrimPrefix(id, "turn-")
	matched, _ := regexp.MatchString("^[0-9a-f]{32}$", hexPart)
	if !matched {
		t.Errorf("newTurnID() hex part should be 32 lowercase hex chars, got: %s", hexPart)
	}
}

func TestNewRequestIDFormat(t *testing.T) {
	id, err := newRequestID()
	if err != nil {
		t.Fatalf("newRequestID() returned error: %v", err)
	}

	// Should start with "req-" prefix
	if !strings.HasPrefix(id, "req-") {
		t.Errorf("newRequestID() should start with 'req-', got: %s", id)
	}

	// Should be req- + 24 hex chars = 28 chars total
	if len(id) != 28 {
		t.Errorf("newRequestID() should be 28 chars (req- + 24 hex), got %d: %s", len(id), id)
	}

	// Hex part should be valid hexadecimal
	hexPart := strings.TrimPrefix(id, "req-")
	matched, _ := regexp.MatchString("^[0-9a-f]{24}$", hexPart)
	if !matched {
		t.Errorf("newRequestID() hex part should be 24 lowercase hex chars, got: %s", hexPart)
	}
}

func TestClientMessageIDValidation(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		valid    bool
	}{
		{"valid alphanumeric", "msg-abc123", true},
		{"valid with underscores", "user_message_001", true},
		{"valid with hyphens", "client-msg-456", true},
		{"empty is valid (optional)", "", true},
		{"too long (>128 chars)", strings.Repeat("a", 129), false},
		{"exactly 128 chars", strings.Repeat("a", 128), true},
		{"invalid characters (spaces)", "msg with spaces", false},
		{"invalid characters (special)", "msg@#$%", false},
		{"unicode not allowed", "消息-001", false},
	}

	pattern := regexp.MustCompile(`^[a-zA-Z0-9_-]{1,128}$`)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			matches := pattern.MatchString(tt.input)
			if matches != tt.valid {
				t.Errorf("pattern match for %q: got %v, want %v", tt.input, matches, tt.valid)
			}
		})
	}
}

func TestAsyncAcceptedBodyIncludesCorrelationFields(t *testing.T) {
	body := asyncAcceptedBody{
		Status:          "accepted",
		RequestID:       "req-abc123",
		EventCursor:     "12345",
		TurnID:          "turn-def456",
		ClientMessageID: "client-msg-789",
	}

	// Verify all fields are present
	if body.Status != "accepted" {
		t.Errorf("Status = %q, want 'accepted'", body.Status)
	}
	if body.RequestID != "req-abc123" {
		t.Errorf("RequestID = %q, want 'req-abc123'", body.RequestID)
	}
	if body.EventCursor != "12345" {
		t.Errorf("EventCursor = %q, want '12345'", body.EventCursor)
	}
	if body.TurnID != "turn-def456" {
		t.Errorf("TurnID = %q, want 'turn-def456'", body.TurnID)
	}
	if body.ClientMessageID != "client-msg-789" {
		t.Errorf("ClientMessageID = %q, want 'client-msg-789'", body.ClientMessageID)
	}
}

func TestAsyncAcceptedBodyOmitsEmptyCorrelationFields(t *testing.T) {
	body := asyncAcceptedBody{
		Status:      "accepted",
		RequestID:   "req-abc123",
		EventCursor: "12345",
		// TurnID and ClientMessageID omitted (empty)
	}

	// Empty strings should serialize as omitted in JSON due to omitempty
	if body.TurnID != "" {
		t.Errorf("TurnID should be empty string, got %q", body.TurnID)
	}
	if body.ClientMessageID != "" {
		t.Errorf("ClientMessageID should be empty string, got %q", body.ClientMessageID)
	}
}

func TestTurnStartedPayload(t *testing.T) {
	payload := TurnStartedPayload{
		TurnID:          "turn-abc123",
		SessionID:       "sess-xyz789",
		ClientMessageID: "client-msg-456",
		RequestID:       "req-def000",
	}

	if payload.TurnID != "turn-abc123" {
		t.Errorf("TurnID = %q, want 'turn-abc123'", payload.TurnID)
	}
	if payload.SessionID != "sess-xyz789" {
		t.Errorf("SessionID = %q, want 'sess-xyz789'", payload.SessionID)
	}
	if payload.ClientMessageID != "client-msg-456" {
		t.Errorf("ClientMessageID = %q, want 'client-msg-456'", payload.ClientMessageID)
	}
	if payload.RequestID != "req-def000" {
		t.Errorf("RequestID = %q, want 'req-def000'", payload.RequestID)
	}

	// Verify it implements events.Payload interface
	var _ interface{ IsEventPayload() } = payload
}

func TestTurnFailedPayload(t *testing.T) {
	payload := TurnFailedPayload{
		TurnID:       "turn-abc123",
		SessionID:    "sess-xyz789",
		ErrorCode:    "provider_error",
		ErrorMessage: "API rate limit exceeded",
	}

	if payload.TurnID != "turn-abc123" {
		t.Errorf("TurnID = %q, want 'turn-abc123'", payload.TurnID)
	}
	if payload.SessionID != "sess-xyz789" {
		t.Errorf("SessionID = %q, want 'sess-xyz789'", payload.SessionID)
	}
	if payload.ErrorCode != "provider_error" {
		t.Errorf("ErrorCode = %q, want 'provider_error'", payload.ErrorCode)
	}
	if payload.ErrorMessage != "API rate limit exceeded" {
		t.Errorf("ErrorMessage = %q, want 'API rate limit exceeded'", payload.ErrorMessage)
	}

	// Verify it implements events.Payload interface
	var _ interface{ IsEventPayload() } = payload
}

func TestTurnCompletedPayload(t *testing.T) {
	payload := TurnCompletedPayload{
		TurnID:     "turn-abc123",
		SessionID:  "sess-xyz789",
		EntryCount: 5,
		DurationMs: 3500,
	}

	if payload.TurnID != "turn-abc123" {
		t.Errorf("TurnID = %q, want 'turn-abc123'", payload.TurnID)
	}
	if payload.SessionID != "sess-xyz789" {
		t.Errorf("SessionID = %q, want 'sess-xyz789'", payload.SessionID)
	}
	if payload.EntryCount != 5 {
		t.Errorf("EntryCount = %d, want 5", payload.EntryCount)
	}
	if payload.DurationMs != 3500 {
		t.Errorf("DurationMs = %d, want 3500", payload.DurationMs)
	}

	// Verify it implements events.Payload interface
	var _ interface{ IsEventPayload() } = payload
}

func TestTurnCanceledPayload(t *testing.T) {
	payload := TurnCanceledPayload{
		TurnID:    "turn-abc123",
		SessionID: "sess-xyz789",
		Reason:    "user_interrupt",
	}

	if payload.TurnID != "turn-abc123" {
		t.Errorf("TurnID = %q, want 'turn-abc123'", payload.TurnID)
	}
	if payload.SessionID != "sess-xyz789" {
		t.Errorf("SessionID = %q, want 'sess-xyz789'", payload.SessionID)
	}
	if payload.Reason != "user_interrupt" {
		t.Errorf("Reason = %q, want 'user_interrupt'", payload.Reason)
	}

	// Verify it implements events.Payload interface
	var _ interface{ IsEventPayload() } = payload
}

func TestTurnCanceledPayloadOptionalReason(t *testing.T) {
	payload := TurnCanceledPayload{
		TurnID:    "turn-abc123",
		SessionID: "sess-xyz789",
		// Reason omitted
	}

	if payload.Reason != "" {
		t.Errorf("Reason should be empty when omitted, got %q", payload.Reason)
	}
}
