package acp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
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
	msg, _ := newSessionPromptRequest("sess-1", runtime.TextContent("hello world"))
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
	if len(params.Prompt) != 1 {
		t.Fatalf("prompt len = %d, want 1", len(params.Prompt))
	}
	if params.Prompt[0].Type != "text" {
		t.Errorf("type = %q, want %q", params.Prompt[0].Type, "text")
	}
	if params.Prompt[0].Text != "hello world" {
		t.Errorf("text = %q, want %q", params.Prompt[0].Text, "hello world")
	}
}

func TestSessionPromptRequest_MultiBlock(t *testing.T) {
	content := []runtime.ContentBlock{
		{Type: "text", Text: "first"},
		{Type: "text", Text: "second"},
	}
	msg, _ := newSessionPromptRequest("sess-1", content)
	data, _ := json.Marshal(msg)

	var decoded JSONRPCMessage
	_ = json.Unmarshal(data, &decoded)
	var params SessionPromptParams
	_ = json.Unmarshal(decoded.Params, &params)

	if len(params.Prompt) != 2 {
		t.Fatalf("prompt blocks = %d, want 2", len(params.Prompt))
	}
	if params.Prompt[0].Text != "first" {
		t.Errorf("block[0] = %q, want %q", params.Prompt[0].Text, "first")
	}
	if params.Prompt[1].Text != "second" {
		t.Errorf("block[1] = %q, want %q", params.Prompt[1].Text, "second")
	}
}

func TestSessionPromptRequest_FilePath(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(f, []byte("file content here"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	content := []runtime.ContentBlock{
		{Type: "file_path", Path: f},
	}
	msg, _ := newSessionPromptRequest("sess-1", content)
	data, _ := json.Marshal(msg)

	var decoded JSONRPCMessage
	_ = json.Unmarshal(data, &decoded)
	var params SessionPromptParams
	_ = json.Unmarshal(decoded.Params, &params)

	if len(params.Prompt) != 1 {
		t.Fatalf("prompt blocks = %d, want 1", len(params.Prompt))
	}
	block := params.Prompt[0]
	if block.Type != "text" {
		t.Errorf("type = %q, want %q", block.Type, "text")
	}
	if !strings.Contains(block.Text, "file content here") {
		t.Errorf("block should contain file content, got %q", block.Text)
	}
	if !strings.Contains(block.Text, "test.txt") {
		t.Errorf("block should reference filename, got %q", block.Text)
	}
}

func TestSessionPromptRequest_FilePathError(t *testing.T) {
	content := []runtime.ContentBlock{
		{Type: "file_path", Path: "/nonexistent/path/to/file.txt"},
	}
	msg, _ := newSessionPromptRequest("sess-1", content)
	data, _ := json.Marshal(msg)

	var decoded JSONRPCMessage
	_ = json.Unmarshal(data, &decoded)
	var params SessionPromptParams
	_ = json.Unmarshal(decoded.Params, &params)

	block := params.Prompt[0]
	if !strings.Contains(block.Text, "Error reading") {
		t.Errorf("block should contain error, got %q", block.Text)
	}
	// Should NOT contain the full path (sanitized).
	if strings.Contains(block.Text, "/nonexistent/path/to/") {
		t.Errorf("block should not contain full path, got %q", block.Text)
	}
}

func TestNewRequest_IncrementingIDs(t *testing.T) {
	_, id1 := newRequest("test", nil)
	_, id2 := newRequest("test", nil)
	if id2 <= id1 {
		t.Errorf("IDs should be incrementing: %d, %d", id1, id2)
	}
}

func TestInitializeRequest_IncludesProtocolVersion(t *testing.T) {
	msg, _ := newInitializeRequest()
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	// Verify raw JSON contains protocolVersion (not omitted via omitempty).
	if !strings.Contains(string(data), `"protocolVersion":1`) {
		t.Errorf("raw JSON should contain \"protocolVersion\":1, got %s", data)
	}

	var decoded JSONRPCMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	var params InitializeParams
	if err := json.Unmarshal(decoded.Params, &params); err != nil {
		t.Fatalf("Unmarshal params: %v", err)
	}
	if params.ProtocolVersion != 1 {
		t.Errorf("protocolVersion = %d, want 1", params.ProtocolVersion)
	}
}

func TestSessionNewRequest_IncludesCwdAndMcpServers(t *testing.T) {
	msg, _ := newSessionNewRequest("/home/user/project", nil)
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded JSONRPCMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if decoded.Method != "session/new" {
		t.Errorf("method = %q, want %q", decoded.Method, "session/new")
	}

	var params SessionNewParams
	if err := json.Unmarshal(decoded.Params, &params); err != nil {
		t.Fatalf("Unmarshal params: %v", err)
	}
	if params.Cwd != "/home/user/project" {
		t.Errorf("cwd = %q, want %q", params.Cwd, "/home/user/project")
	}
	if params.McpServers == nil {
		t.Fatal("mcpServers should be non-nil empty array")
	}
	if len(params.McpServers) != 0 {
		t.Errorf("mcpServers len = %d, want 0", len(params.McpServers))
	}
	// Verify raw JSON has [] not null for mcpServers.
	if !strings.Contains(string(data), `"mcpServers":[]`) {
		t.Errorf("raw JSON should contain \"mcpServers\":[], got %s", data)
	}
}

func TestSessionNewRequest_SerializesMCPServersByTransport(t *testing.T) {
	msg, _ := newSessionNewRequest("/home/user/project", []runtime.MCPServerConfig{
		{
			Name:      "filesystem",
			Transport: runtime.MCPTransportStdio,
			Command:   "/bin/mcp-fs",
			Args:      []string{"--stdio"},
			Env: map[string]string{
				"HOME":  "/tmp/home",
				"TOKEN": "secret",
			},
		},
		{
			Name:      "remote",
			Transport: runtime.MCPTransportHTTP,
			URL:       "https://mcp.example.test",
			Headers: map[string]string{
				"Authorization": "Bearer token",
			},
		},
		{
			Name:      "stream",
			Transport: runtime.MCPTransportSSE,
			URL:       "https://mcp.example.test/sse",
			Headers: map[string]string{
				"X-Test": "1",
			},
		},
	})
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var decoded JSONRPCMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	var params struct {
		Cwd        string            `json:"cwd"`
		McpServers []json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(decoded.Params, &params); err != nil {
		t.Fatalf("Unmarshal params: %v", err)
	}
	if len(params.McpServers) != 3 {
		t.Fatalf("mcpServers len = %d, want 3", len(params.McpServers))
	}

	var stdio struct {
		Type    string                `json:"type"`
		Name    string                `json:"name"`
		Command string                `json:"command"`
		Args    []string              `json:"args"`
		Env     []runtime.MCPKeyValue `json:"env"`
	}
	if err := json.Unmarshal(params.McpServers[0], &stdio); err != nil {
		t.Fatalf("Unmarshal stdio server: %v", err)
	}
	if stdio.Type != "" {
		t.Fatalf("stdio type = %q, want omitted", stdio.Type)
	}
	if stdio.Command != "/bin/mcp-fs" {
		t.Fatalf("stdio command = %q, want /bin/mcp-fs", stdio.Command)
	}
	if len(stdio.Env) != 2 || stdio.Env[0].Name != "HOME" || stdio.Env[1].Name != "TOKEN" {
		t.Fatalf("stdio env = %#v, want sorted HOME/TOKEN", stdio.Env)
	}

	var http struct {
		Type    string                `json:"type"`
		Name    string                `json:"name"`
		URL     string                `json:"url"`
		Headers []runtime.MCPKeyValue `json:"headers"`
	}
	if err := json.Unmarshal(params.McpServers[1], &http); err != nil {
		t.Fatalf("Unmarshal http server: %v", err)
	}
	if http.Type != "http" {
		t.Fatalf("http type = %q, want http", http.Type)
	}
	if http.URL != "https://mcp.example.test" {
		t.Fatalf("http url = %q, want https://mcp.example.test", http.URL)
	}
	if len(http.Headers) != 1 || http.Headers[0].Name != "Authorization" {
		t.Fatalf("http headers = %#v, want Authorization", http.Headers)
	}

	var sse struct {
		Type    string                `json:"type"`
		Name    string                `json:"name"`
		URL     string                `json:"url"`
		Headers []runtime.MCPKeyValue `json:"headers"`
	}
	if err := json.Unmarshal(params.McpServers[2], &sse); err != nil {
		t.Fatalf("Unmarshal sse server: %v", err)
	}
	if sse.Type != "sse" {
		t.Fatalf("sse type = %q, want sse", sse.Type)
	}
	if sse.URL != "https://mcp.example.test/sse" {
		t.Fatalf("sse url = %q, want https://mcp.example.test/sse", sse.URL)
	}
	if len(sse.Headers) != 1 || sse.Headers[0].Name != "X-Test" {
		t.Fatalf("sse headers = %#v, want X-Test", sse.Headers)
	}
}

func TestSessionPromptRequest_UsesPromptFieldNotMessages(t *testing.T) {
	msg, _ := newSessionPromptRequest("sess-1", runtime.TextContent("test"))
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	raw := string(data)
	if !strings.Contains(raw, `"prompt":[`) {
		t.Errorf("raw JSON should contain \"prompt\":[ field, got %s", raw)
	}
	if strings.Contains(raw, `"messages"`) {
		t.Errorf("raw JSON should NOT contain \"messages\" field, got %s", raw)
	}
}
