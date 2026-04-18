package acp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// shortTempDir returns a temp directory short enough for Unix socket paths
// (macOS limit is 104 bytes). t.TempDir() paths often exceed this.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "gc-t-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// newTestProvider creates an ACP provider with an isolated temp directory.
func newTestProvider(t *testing.T) *Provider {
	t.Helper()
	dir := filepath.Join(shortTempDir(t), "acp")
	return NewProviderWithDir(dir, Config{
		HandshakeTimeout:  5 * time.Second,
		NudgeBusyTimeout:  2 * time.Second,
		OutputBufferLines: 100,
	})
}

var testCounter atomic.Int64

func testName() string {
	return fmt.Sprintf("gc-acp-test-%d", testCounter.Add(1))
}

// fakeACPCommand returns a shell command that runs a minimal ACP server
// implemented as a Go test binary using the testdata/fakeacp program.
// For unit tests, we use a simple shell script instead.
func fakeACPShellCommand() string {
	// This script implements a minimal ACP server in shell:
	// Reads JSON-RPC from stdin, responds to initialize and session/new,
	// echoes session/prompt text as session/update notifications.
	return `exec python3 -u -c '
import sys, json

def respond(id, result):
    msg = {"jsonrpc": "2.0", "id": id, "result": result}
    print(json.dumps(msg), flush=True)

def notify(method, params):
    msg = {"jsonrpc": "2.0", "method": method, "params": params}
    print(json.dumps(msg), flush=True)

session_id = "test-session-1"

for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except:
        continue
    method = msg.get("method", "")
    msg_id = msg.get("id")

    if method == "initialize":
        respond(msg_id, {"serverInfo": {"name": "fakeacp", "version": "1.0"}})
    elif method == "initialized":
        pass  # notification, no response
    elif method == "session/new":
        respond(msg_id, {"sessionId": session_id})
    elif method == "session/prompt":
        params = msg.get("params", {})
        messages = params.get("messages", [])
        text = ""
        for m in messages:
            for c in m.get("content", []):
                text += c.get("text", "")
        # Send update notification with echoed text
        notify("session/update", {
            "sessionId": session_id,
            "content": [{"type": "text", "text": "echo: " + text}]
        })
        # Send prompt response
        respond(msg_id, {})
'`
}

func TestStart_HandshakeSuccess(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	if !p.IsRunning(name) {
		t.Error("IsRunning = false after Start, want true")
	}
}

func TestStart_DuplicateReturnsError(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	err = p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	})
	if err == nil {
		t.Error("second Start should return error for duplicate name")
	}
}

func TestStart_HandshakeTimeout(t *testing.T) {
	p := newTestProvider(t)
	name := testName()

	// Use a command that doesn't speak ACP — just sleeps.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	err := p.Start(ctx, name, runtime.Config{
		Command: "sleep 300",
		WorkDir: t.TempDir(),
	})
	if err == nil {
		t.Cleanup(func() { _ = p.Stop(name) })
		t.Fatal("Start should fail when handshake times out")
	}
	if !strings.Contains(err.Error(), "handshake") {
		t.Errorf("error should mention handshake, got: %v", err)
	}
}

func TestStart_RequiresCommand(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	err := p.Start(context.Background(), name, runtime.Config{
		WorkDir: t.TempDir(),
	})
	if err == nil {
		t.Cleanup(func() { _ = p.Stop(name) })
		t.Error("Start with empty command should return error")
	}
}

func TestStop_MakesSessionNotRunning(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := p.Stop(name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if p.IsRunning(name) {
		t.Error("IsRunning = true after Stop, want false")
	}
}

func TestStop_Idempotent(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Stop("never-started"); err != nil {
		t.Errorf("Stop on never-started session: %v", err)
	}
}

func TestNudge_SendsPrompt(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	if err := p.Nudge(name, runtime.TextContent("hello world")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}

	// Wait for the echoed output to appear in the buffer.
	var output string
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, _ = p.Peek(name, 0)
		if strings.Contains(output, "echo: hello world") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !strings.Contains(output, "echo: hello world") {
		t.Errorf("Peek output = %q, want to contain %q", output, "echo: hello world")
	}
}

func TestNudge_MissingSession(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Nudge("nonexistent", runtime.TextContent("hello")); err != nil {
		t.Errorf("Nudge on missing session should not error: %v", err)
	}
}

func TestPeek_ReturnsOutput(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	// Send a nudge to generate output.
	_ = p.Nudge(name, runtime.TextContent("test line"))

	deadline := time.Now().Add(3 * time.Second)
	var output string
	for time.Now().Before(deadline) {
		output, _ = p.Peek(name, 10)
		if output != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if output == "" {
		t.Error("Peek returned empty output after nudge")
	}
}

func TestPeek_MissingSession(t *testing.T) {
	p := newTestProvider(t)
	output, err := p.Peek("nonexistent", 10)
	if err != nil {
		t.Fatalf("Peek error: %v", err)
	}
	if output != "" {
		t.Errorf("Peek on missing session = %q, want empty", output)
	}
}

func TestGetLastActivity_UpdatedOnOutput(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	before, _ := p.GetLastActivity(name)

	// Send nudge to trigger output.
	_ = p.Nudge(name, runtime.TextContent("activity test"))

	deadline := time.Now().Add(3 * time.Second)
	var after time.Time
	for time.Now().Before(deadline) {
		after, _ = p.GetLastActivity(name)
		if after.After(before) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if !after.After(before) {
		t.Error("GetLastActivity should increase after nudge")
	}
}

func TestClearScrollback_ClearsBuffer(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	_ = p.Nudge(name, runtime.TextContent("some text"))
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, _ := p.Peek(name, 0)
		if output != "" {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	_ = p.ClearScrollback(name)
	output, _ := p.Peek(name, 0)
	if output != "" {
		t.Errorf("Peek after ClearScrollback = %q, want empty", output)
	}
}

func TestMeta_RoundTrip(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	if err := p.SetMeta(name, "key1", "val1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	val, err := p.GetMeta(name, "key1")
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if val != "val1" {
		t.Errorf("GetMeta = %q, want %q", val, "val1")
	}

	if err := p.RemoveMeta(name, "key1"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
	val, err = p.GetMeta(name, "key1")
	if err != nil {
		t.Fatalf("GetMeta after remove: %v", err)
	}
	if val != "" {
		t.Errorf("GetMeta after remove = %q, want empty", val)
	}
}

func TestMetaPath_HashesUntrustedNameAndKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acp")
	p := NewProviderWithDir(dir, Config{})

	path := p.metaPath("../escape", "../key")
	if filepath.Dir(path) != dir {
		t.Fatalf("metaPath escaped provider dir: %q", path)
	}
	if base := filepath.Base(path); strings.Contains(base, "..") || strings.ContainsAny(base, `/\`) {
		t.Fatalf("metaPath base = %q, want hashed file name without path tokens", base)
	}

	if err := p.SetMeta("../escape", "../key", "secret"); err != nil {
		t.Fatalf("SetMeta with untrusted tokens: %v", err)
	}
	got, err := p.GetMeta("../escape", "../key")
	if err != nil {
		t.Fatalf("GetMeta with untrusted tokens: %v", err)
	}
	if got != "secret" {
		t.Fatalf("GetMeta = %q, want secret", got)
	}
	if err := p.RemoveMeta("../escape", "../key"); err != nil {
		t.Fatalf("RemoveMeta with untrusted tokens: %v", err)
	}
}

func TestAttach_ReturnsError(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Attach("any"); err == nil {
		t.Error("Attach should return error for ACP provider")
	}
}

func TestIsAttached_AlwaysFalse(t *testing.T) {
	p := newTestProvider(t)
	if p.IsAttached("any") {
		t.Error("IsAttached should always be false for ACP provider")
	}
}

func TestProcessAlive_EmptyNamesReturnsTrue(t *testing.T) {
	p := newTestProvider(t)
	if !p.ProcessAlive("any", nil) {
		t.Error("ProcessAlive with nil names should return true")
	}
}

func TestBusyState_SetAndCleared(t *testing.T) {
	// Test the sessionConn busy tracking directly.
	sc := &sessionConn{
		outputBufMax: 100,
		pending:      make(map[int64]chan JSONRPCMessage),
	}

	if sc.isBusy() {
		t.Error("should not be busy initially")
	}

	sc.setActivePrompt(42)
	if !sc.isBusy() {
		t.Error("should be busy after setActivePrompt")
	}

	// Simulate receiving a response that matches the active prompt.
	sc.mu.Lock()
	id := int64(42)
	sc.activePromptID = 0 // as dispatch would do
	_ = id
	sc.mu.Unlock()

	if sc.isBusy() {
		t.Error("should not be busy after clearing activePromptID")
	}
}

func TestOutputBuffer_CircularEviction(t *testing.T) {
	sc := &sessionConn{
		outputBufMax: 5,
		pending:      make(map[int64]chan JSONRPCMessage),
	}

	// Add 8 lines — should keep only the last 5.
	sc.mu.Lock()
	for i := 0; i < 8; i++ {
		sc.appendLine(fmt.Sprintf("line-%d", i))
	}
	sc.mu.Unlock()

	output := sc.peekLines(0)
	lines := strings.Split(output, "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d: %v", len(lines), lines)
	}
	if lines[0] != "line-3" {
		t.Errorf("first line = %q, want %q", lines[0], "line-3")
	}
	if lines[4] != "line-7" {
		t.Errorf("last line = %q, want %q", lines[4], "line-7")
	}
}

func TestOutputBuffer_PeekNLines(t *testing.T) {
	sc := &sessionConn{
		outputBufMax: 100,
		pending:      make(map[int64]chan JSONRPCMessage),
	}

	sc.mu.Lock()
	for i := 0; i < 10; i++ {
		sc.appendLine(fmt.Sprintf("line-%d", i))
	}
	sc.mu.Unlock()

	output := sc.peekLines(3)
	lines := strings.Split(output, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d", len(lines))
	}
	if lines[0] != "line-7" {
		t.Errorf("first peeked line = %q, want %q", lines[0], "line-7")
	}
}

func TestDispatch_RoutesUpdateNotification(t *testing.T) {
	sc := &sessionConn{
		outputBufMax: 100,
		pending:      make(map[int64]chan JSONRPCMessage),
	}

	params, _ := json.Marshal(SessionUpdateParams{
		SessionID: "s1",
		Content:   []ContentBlock{{Type: "text", Text: "hello"}},
	})
	sc.dispatch(JSONRPCMessage{
		JSONRPC: "2.0",
		Method:  "session/update",
		Params:  params,
	})

	output := sc.peekLines(0)
	if output != "hello" {
		t.Errorf("output = %q, want %q", output, "hello")
	}

	if sc.getLastActivity().IsZero() {
		t.Error("lastActivity should be set after update")
	}
}

func TestDispatch_RoutesResponseToWaiter(t *testing.T) {
	sc := &sessionConn{
		outputBufMax: 100,
		pending:      make(map[int64]chan JSONRPCMessage),
	}

	ch := make(chan JSONRPCMessage, 1)
	id := int64(7)
	sc.mu.Lock()
	sc.pending[id] = ch
	sc.mu.Unlock()

	result, _ := json.Marshal(map[string]string{"ok": "true"})
	sc.dispatch(JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Result:  result,
	})

	select {
	case resp := <-ch:
		if resp.ID == nil || *resp.ID != 7 {
			t.Errorf("response ID = %v, want 7", resp.ID)
		}
	default:
		t.Error("expected response on channel")
	}
}

func TestDispatch_ClearsActivePromptOnResponse(t *testing.T) {
	sc := &sessionConn{
		outputBufMax: 100,
		pending:      make(map[int64]chan JSONRPCMessage),
	}

	ch := make(chan JSONRPCMessage, 1)
	id := int64(42)
	sc.mu.Lock()
	sc.pending[id] = ch
	sc.activePromptID = id
	sc.mu.Unlock()

	sc.dispatch(JSONRPCMessage{
		JSONRPC: "2.0",
		ID:      &id,
		Result:  json.RawMessage(`{}`),
	})

	if sc.isBusy() {
		t.Error("should not be busy after prompt response")
	}
}

func TestListRunning_FindsSessions(t *testing.T) {
	p := newTestProvider(t)
	name := testName()
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	names, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	found := false
	for _, n := range names {
		if n == name {
			found = true
		}
	}
	if !found {
		t.Errorf("ListRunning should include %q, got %v", name, names)
	}
}

func TestStartLongSocketPathUsesShortSocketName(t *testing.T) {
	root, err := os.MkdirTemp("", "gc-acp-sock-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	const name = "control-dispatcher"
	longDir := ""
	for i := 1; i <= 32; i++ {
		candidate := filepath.Join(root, strings.Repeat("deep-path-", i), "acp")
		p := NewProviderWithDir(candidate, Config{
			HandshakeTimeout:  5 * time.Second,
			NudgeBusyTimeout:  2 * time.Second,
			OutputBufferLines: 100,
		})
		if len(p.legacySockPath(name)) > 108 && len(p.sockPath(name)) < 108 {
			longDir = candidate
			break
		}
	}
	if longDir == "" {
		t.Fatal("failed to construct path where legacy socket is too long but short socket fits")
	}
	if err := os.MkdirAll(longDir, 0o755); err != nil {
		t.Fatalf("mkdir longDir: %v", err)
	}

	p := NewProviderWithDir(longDir, Config{
		HandshakeTimeout:  5 * time.Second,
		NudgeBusyTimeout:  2 * time.Second,
		OutputBufferLines: 100,
	})
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: fakeACPShellCommand(),
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })

	if _, err := os.Stat(p.sockPath(name)); err != nil {
		t.Fatalf("short socket path missing: %v", err)
	}
	if got, want := filepath.Base(p.sockPath(name)), name+".sock"; got == want {
		t.Fatalf("socket filename = %q, want shortened hashed filename", got)
	}
	if len(p.sockPath(name)) >= len(p.legacySockPath(name)) {
		t.Fatalf("short socket path = %q, legacy = %q; want shorter path", p.sockPath(name), p.legacySockPath(name))
	}
}

func TestSendKeysAndRunLive_NoOp(t *testing.T) {
	p := newTestProvider(t)
	if err := p.SendKeys("any", "Enter"); err != nil {
		t.Errorf("SendKeys: %v", err)
	}
	if err := p.RunLive("any", runtime.Config{}); err != nil {
		t.Errorf("RunLive: %v", err)
	}
}

func TestPendingAndRespondUnsupported(t *testing.T) {
	p := newTestProvider(t)

	if _, err := p.Pending("any"); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("Pending error = %v, want ErrInteractionUnsupported", err)
	}
	if err := p.Respond("any", runtime.InteractionResponse{Action: "approve"}); !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("Respond error = %v, want ErrInteractionUnsupported", err)
	}
}

func TestHandshakeTimeout_RespectsConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "acp")
	p := NewProviderWithDir(dir, Config{
		HandshakeTimeout: 500 * time.Millisecond, // short timeout
	})
	name := testName()

	// Use a long parent context — the handshake timeout should still apply.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	start := time.Now()
	err := p.Start(ctx, name, runtime.Config{
		Command: "sleep 300", // doesn't speak ACP
		WorkDir: t.TempDir(),
	})
	elapsed := time.Since(start)

	if err == nil {
		t.Cleanup(func() { _ = p.Stop(name) })
		t.Fatal("Start should fail when handshake times out")
	}

	// Should fail in ~500ms, not 30s.
	if elapsed > 3*time.Second {
		t.Errorf("handshake took %v, expected ~500ms (handshake_timeout should apply)", elapsed)
	}
}

func TestReadLoopDeath_ClearsBusyState(t *testing.T) {
	sc := &sessionConn{
		done:         make(chan struct{}),
		outputBufMax: 100,
		pending:      make(map[int64]chan JSONRPCMessage),
	}

	// Set up busy state with a pending response.
	ch := make(chan JSONRPCMessage, 1)
	sc.mu.Lock()
	sc.activePromptID = 42
	sc.pending[42] = ch
	sc.mu.Unlock()

	if !sc.isBusy() {
		t.Fatal("should be busy before drainPending")
	}

	// Simulate readLoop exit (calls drainPending).
	sc.drainPending()

	if sc.isBusy() {
		t.Error("should not be busy after drainPending")
	}

	// The pending channel should be closed.
	_, ok := <-ch
	if ok {
		t.Error("pending channel should be closed after drainPending")
	}
}

func TestDrainPending_Idempotent(t *testing.T) {
	sc := &sessionConn{
		done:         make(chan struct{}),
		outputBufMax: 100,
		pending:      make(map[int64]chan JSONRPCMessage),
	}

	ch := make(chan JSONRPCMessage, 1)
	sc.mu.Lock()
	sc.pending[1] = ch
	sc.mu.Unlock()

	// Call twice — should not panic on double-close.
	sc.drainPending()
	sc.drainPending() // second call should be a no-op

	if sc.isBusy() {
		t.Error("should not be busy after double drain")
	}
}

func TestStderrCaptured_InHandshakeError(t *testing.T) {
	p := newTestProvider(t)
	name := testName()

	// Use a command that writes to stderr then exits.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := p.Start(ctx, name, runtime.Config{
		Command: "echo 'fatal: bad config' >&2; sleep 300",
		WorkDir: t.TempDir(),
	})
	if err == nil {
		t.Cleanup(func() { _ = p.Stop(name) })
		t.Fatal("Start should fail on handshake timeout")
	}

	if !strings.Contains(err.Error(), "fatal: bad config") {
		t.Errorf("error should contain stderr output, got: %v", err)
	}
}
