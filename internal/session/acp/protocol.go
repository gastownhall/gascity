// Package acp implements [session.Provider] using the Agent Client Protocol.
//
// ACP is a JSON-RPC 2.0 protocol for headless agent execution. Each agent
// process communicates over stdio — the provider spawns the process with
// pipes, performs the ACP handshake, then sends prompts and captures output
// via structured JSON-RPC messages.
//
// Process tracking reuses the subprocess pattern: in-memory for the same gc
// process, unix sockets for cross-process persistence. The ACP layer adds
// JSON-RPC framing and busy-state tracking on top.
package acp

import (
	"encoding/json"
	"sync/atomic"
)

// nextID is a package-level counter for JSON-RPC request IDs.
var nextID atomic.Int64

// JSONRPCMessage is a unified JSON-RPC 2.0 message. It can represent a
// request, response, or notification depending on which fields are set.
type JSONRPCMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`     // nil for notifications
	Method  string          `json:"method,omitempty"` // set for requests/notifications
	Params  json.RawMessage `json:"params,omitempty"` // set for requests/notifications
	Result  json.RawMessage `json:"result,omitempty"` // set for responses
	Error   *JSONRPCError   `json:"error,omitempty"`  // set for error responses
}

// JSONRPCError represents a JSON-RPC 2.0 error object.
type JSONRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ContentBlock represents a content block in ACP messages.
type ContentBlock struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// ClientInfo identifies the client in the initialize handshake.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// ServerInfo identifies the server in the initialize response.
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version,omitempty"`
}

// InitializeParams is the params for the "initialize" request.
type InitializeParams struct {
	ClientInfo ClientInfo `json:"clientInfo"`
}

// InitializeResult is the result of the "initialize" request.
type InitializeResult struct {
	ServerInfo ServerInfo `json:"serverInfo"`
}

// SessionNewResult is the result of the "session/new" request.
type SessionNewResult struct {
	SessionID string `json:"sessionId"`
}

// SessionPromptParams is the params for the "session/prompt" request.
type SessionPromptParams struct {
	SessionID string          `json:"sessionId"`
	Messages  []PromptMessage `json:"messages"`
}

// PromptMessage is a message within a session/prompt request.
type PromptMessage struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"content"`
}

// SessionUpdateParams is the params for "session/update" notifications.
type SessionUpdateParams struct {
	SessionID string         `json:"sessionId"`
	Content   []ContentBlock `json:"content"`
}

// newRequest creates a JSON-RPC request with a unique ID.
func newRequest(method string, params any) (JSONRPCMessage, int64) {
	id := nextID.Add(1)
	msg := JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
	}
	if params != nil {
		data, _ := json.Marshal(params)
		msg.Params = data
	}
	return msg, id
}

// newNotification creates a JSON-RPC notification (no ID, no response expected).
func newNotification(method string) JSONRPCMessage {
	return JSONRPCMessage{
		JSONRPC: "2.0",
		Method:  method,
	}
}

// newInitializeRequest creates an "initialize" request.
func newInitializeRequest() (JSONRPCMessage, int64) {
	return newRequest("initialize", InitializeParams{
		ClientInfo: ClientInfo{Name: "gc", Version: "1.0"},
	})
}

// newInitializedNotification creates an "initialized" notification.
func newInitializedNotification() JSONRPCMessage {
	return newNotification("initialized")
}

// newSessionNewRequest creates a "session/new" request.
func newSessionNewRequest() (JSONRPCMessage, int64) {
	return newRequest("session/new", nil)
}

// newSessionPromptRequest creates a "session/prompt" request.
func newSessionPromptRequest(sessionID, text string) (JSONRPCMessage, int64) {
	return newRequest("session/prompt", SessionPromptParams{
		SessionID: sessionID,
		Messages: []PromptMessage{
			{
				Role:    "user",
				Content: []ContentBlock{{Type: "text", Text: text}},
			},
		},
	})
}
