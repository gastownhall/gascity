package acp

import (
	"encoding/json"
	"testing"
)

func TestJSONRPCMessage_RequestRoundTrip(t *testing.T) {
	msg, id := newInitializeRequest()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded JSONRPCMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want %q", decoded.JSONRPC, "2.0")
	}
	if decoded.ID == nil || *decoded.ID != id {
		t.Errorf("id = %v, want %d", decoded.ID, id)
	}
	if decoded.Method != "initialize" {
		t.Errorf("method = %q, want %q", decoded.Method, "initialize")
	}

	var params InitializeParams
	if err := json.Unmarshal(decoded.Params, &params); err != nil {
		t.Fatalf("Unmarshal params: %v", err)
	}
	if params.ClientInfo.Name != "gc" {
		t.Errorf("clientInfo.name = %q, want %q", params.ClientInfo.Name, "gc")
	}
}

func TestJSONRPCMessage_NotificationOmitsID(t *testing.T) {
	msg := newInitializedNotification()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded JSONRPCMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.ID != nil {
		t.Errorf("notification should have nil ID, got %d", *decoded.ID)
	}
	if decoded.Method != "initialized" {
		t.Errorf("method = %q, want %q", decoded.Method, "initialized")
	}
}

func TestJSONRPCMessage_ResponseRoundTrip(t *testing.T) {
	id := int64(42)
	result, _ := json.Marshal(SessionNewResult{SessionID: "sess-1"})
	msg := JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Result:  result,
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded JSONRPCMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.ID == nil || *decoded.ID != 42 {
		t.Errorf("id = %v, want 42", decoded.ID)
	}
	if decoded.Method != "" {
		t.Errorf("response should have empty method, got %q", decoded.Method)
	}

	var sessResult SessionNewResult
	if err := json.Unmarshal(decoded.Result, &sessResult); err != nil {
		t.Fatalf("Unmarshal result: %v", err)
	}
	if sessResult.SessionID != "sess-1" {
		t.Errorf("sessionId = %q, want %q", sessResult.SessionID, "sess-1")
	}
}

func TestJSONRPCMessage_ErrorRoundTrip(t *testing.T) {
	id := int64(99)
	msg := JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Error:   &JSONRPCError{Code: -32601, Message: "method not found"},
	}

	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded JSONRPCMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Error == nil {
		t.Fatal("expected error in response")
	}
	if decoded.Error.Code != -32601 {
		t.Errorf("error code = %d, want %d", decoded.Error.Code, -32601)
	}
}

func TestSessionPromptRequest_Structure(t *testing.T) {
	msg, _ := newSessionPromptRequest("sess-1", "hello world")
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded JSONRPCMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Method != "session/prompt" {
		t.Errorf("method = %q, want %q", decoded.Method, "session/prompt")
	}

	var params SessionPromptParams
	if err := json.Unmarshal(decoded.Params, &params); err != nil {
		t.Fatalf("Unmarshal params: %v", err)
	}

	if params.SessionID != "sess-1" {
		t.Errorf("sessionId = %q, want %q", params.SessionID, "sess-1")
	}
	if len(params.Messages) != 1 {
		t.Fatalf("messages len = %d, want 1", len(params.Messages))
	}
	if params.Messages[0].Role != "user" {
		t.Errorf("role = %q, want %q", params.Messages[0].Role, "user")
	}
	if len(params.Messages[0].Content) != 1 || params.Messages[0].Content[0].Text != "hello world" {
		t.Errorf("content text = %q, want %q", params.Messages[0].Content[0].Text, "hello world")
	}
}

func TestNewRequest_IncrementingIDs(t *testing.T) {
	_, id1 := newRequest("test", nil)
	_, id2 := newRequest("test", nil)
	if id2 <= id1 {
		t.Errorf("IDs should be incrementing: %d, %d", id1, id2)
	}
}
