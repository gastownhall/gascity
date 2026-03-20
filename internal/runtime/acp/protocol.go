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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/gastownhall/gascity/internal/runtime"
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
	Path string `json:"path,omitempty"` // reserved for future ACP file support
	Mime string `json:"mime,omitempty"` // reserved for future ACP file support
}

// maxFileInlineBytes is the maximum file size for inline content (1 MiB).
const maxFileInlineBytes = 1 << 20

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

// ClientCapabilities advertises client-side capabilities in the initialize request.
type ClientCapabilities struct {
	FS       *FileSystemCapabilities `json:"fs,omitempty"`
	Terminal bool                    `json:"terminal,omitempty"`
}

// FileSystemCapabilities describes client filesystem support.
type FileSystemCapabilities struct {
	ReadTextFile  bool `json:"readTextFile,omitempty"`
	WriteTextFile bool `json:"writeTextFile,omitempty"`
}

// InitializeParams is the params for the "initialize" request.
type InitializeParams struct {
	ProtocolVersion    int                 `json:"protocolVersion"`
	ClientInfo         ClientInfo          `json:"clientInfo"`
	ClientCapabilities *ClientCapabilities `json:"clientCapabilities,omitempty"`
}

// InitializeResult is the result of the "initialize" request.
type InitializeResult struct {
	ServerInfo ServerInfo `json:"serverInfo"`
}

// SessionNewParams is the params for the "session/new" request.
// Both cwd and mcpServers are required by strict ACP implementations (e.g. Copilot).
type SessionNewParams struct {
	Cwd        string      `json:"cwd"`
	MCPServers []MCPServer `json:"mcpServers"`
}

// MCPServer describes an MCP server to inject into the session.
type MCPServer struct {
	Name    string   `json:"name"`
	Command string   `json:"command"`
	Args    []string `json:"args"`
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
		ProtocolVersion: 1,
		ClientInfo:      ClientInfo{Name: "gc", Version: "1.0"},
		ClientCapabilities: &ClientCapabilities{
			Terminal: true,
			FS: &FileSystemCapabilities{
				ReadTextFile:  true,
				WriteTextFile: true,
			},
		},
	})
}

// newInitializedNotification creates an "initialized" notification.
func newInitializedNotification() JSONRPCMessage {
	return newNotification("initialized")
}

// newSessionNewRequest creates a "session/new" request with the given working directory.
// mcpServers is always sent as an array (empty if none) — Copilot requires both fields.
func newSessionNewRequest(cwd string) (JSONRPCMessage, int64) {
	return newRequest("session/new", SessionNewParams{
		Cwd:        cwd,
		MCPServers: []MCPServer{},
	})
}

// newSessionPromptRequest creates a "session/prompt" request from
// structured content blocks. File_path blocks are inlined as text
// with a preamble (ACP agents receive file content inline).
func newSessionPromptRequest(sessionID string, content []runtime.ContentBlock) (JSONRPCMessage, int64) {
	var blocks []ContentBlock
	for _, b := range content {
		switch b.Type {
		case "file_path":
			blocks = append(blocks, inlineFileBlock(b.Path))
		default: // "text"
			if b.Text != "" {
				blocks = append(blocks, ContentBlock{Type: "text", Text: b.Text})
			}
		}
	}
	if len(blocks) == 0 {
		blocks = []ContentBlock{{Type: "text", Text: ""}}
	}
	return newRequest("session/prompt", SessionPromptParams{
		SessionID: sessionID,
		Messages: []PromptMessage{
			{
				Role:    "user",
				Content: blocks,
			},
		},
	})
}

// inlineFileBlock reads a file and returns its content as a text block
// with a preamble header. Returns an error placeholder on failure.
func inlineFileBlock(path string) ContentBlock {
	base := filepath.Base(path)
	info, err := os.Stat(path)
	switch {
	case err != nil:
		return ContentBlock{Type: "text", Text: fmt.Sprintf("[Error reading %s: %v]", base, sanitizePathErr(err))}
	case info.Size() > maxFileInlineBytes:
		return ContentBlock{Type: "text", Text: fmt.Sprintf("[File %s too large to inline: %d bytes]", base, info.Size())}
	default:
		data, err := os.ReadFile(path)
		if err != nil {
			return ContentBlock{Type: "text", Text: fmt.Sprintf("[Error reading %s: %v]", base, sanitizePathErr(err))}
		}
		return ContentBlock{Type: "text", Text: fmt.Sprintf("--- %s ---\n%s", base, string(data))}
	}
}

// sanitizePathErr strips the full path from *os.PathError to avoid
// leaking server-side filesystem details.
func sanitizePathErr(err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err
	}
	return err
}
