package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeACPRequestingCommand returns an inline python3 ACP agent that, on
// session/prompt, sends fs/read_text_file with the given raw JSON id, writes
// the client's reply line to $GC_REPLY_FILE, then answers the prompt. It
// also writes the initialize params it received to $GC_INIT_FILE.
func fakeACPRequestingCommand(rawID string) string {
	return `exec python3 -u -c '
import sys, json, os

def send(msg):
    print(json.dumps(msg), flush=True)

req_id = json.loads(sys.argv[1])
prompt_id = None
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except Exception:
        continue
    method = msg.get("method", "")
    if method == "initialize":
        with open(os.environ["GC_INIT_FILE"], "w") as f:
            json.dump(msg.get("params", {}), f)
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {"serverInfo": {"name": "fake"}}})
    elif method == "session/new":
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {"sessionId": "s1"}})
    elif method == "session/prompt":
        prompt_id = msg["id"]
        send({"jsonrpc": "2.0", "id": req_id, "method": "fs/read_text_file",
              "params": {"sessionId": "s1", "path": "/etc/hostname"}})
    elif method == "" and "id" in msg and msg["id"] == req_id and prompt_id is not None:
        with open(os.environ["GC_REPLY_FILE"], "w") as f:
            f.write(line)
        send({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "s1",
              "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "done"}}}})
        send({"jsonrpc": "2.0", "id": prompt_id, "result": {"stopReason": "end_turn"}})
' ` + shellQuoteArg(rawID)
}

// protocolUnitWait bounds fact-based waits on the inline fakes; they answer in
// milliseconds, so the bound only turns a hang into a failure.
const protocolUnitWait = 10 * time.Second

func shellQuoteArg(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

type requestingFake struct {
	p         *Provider
	name      string
	replyFile string
	initFile  string
}

func startRequestingFake(t *testing.T, rawID string) *requestingFake {
	t.Helper()
	dir := t.TempDir()
	f := &requestingFake{
		p:         newTestProvider(t),
		name:      testName(),
		replyFile: filepath.Join(dir, "reply.json"),
		initFile:  filepath.Join(dir, "init.json"),
	}
	if err := f.p.Start(context.Background(), f.name, runtime.Config{
		Command: fakeACPRequestingCommand(rawID),
		WorkDir: t.TempDir(),
		Env: map[string]string{
			"GC_REPLY_FILE": f.replyFile,
			"GC_INIT_FILE":  f.initFile,
		},
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.p.Stop(f.name) })
	return f
}

// promptAndSettle sends one prompt and waits until its response arrives.
func (f *requestingFake) promptAndSettle(t *testing.T) {
	t.Helper()
	if err := f.p.Nudge(f.name, runtime.TextContent("read something")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	f.p.mu.Lock()
	sc := f.p.conns[f.name]
	f.p.mu.Unlock()
	if sc == nil {
		t.Fatal("no in-process connection")
	}
	if !sc.waitIdle(protocolUnitWait) {
		t.Fatal("prompt never completed; gc did not answer the agent's request")
	}
}

func (f *requestingFake) reply(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(f.replyFile)
	if err != nil {
		t.Fatalf("reading recorded reply: %v", err)
	}
	var reply map[string]json.RawMessage
	if err := json.Unmarshal(raw, &reply); err != nil {
		t.Fatalf("decoding reply %q: %v", raw, err)
	}
	return reply
}

func assertMethodNotFound(t *testing.T, reply map[string]json.RawMessage, wantID, method string) {
	t.Helper()
	if got := string(reply["id"]); got != wantID {
		t.Errorf("reply id = %s, want %s", got, wantID)
	}
	if got := string(reply["jsonrpc"]); got != `"2.0"` {
		t.Errorf("reply jsonrpc = %s, want \"2.0\"", got)
	}
	if _, ok := reply["result"]; ok {
		t.Errorf("reply has result, want error only: %v", reply)
	}
	var rpcErr JSONRPCError
	if err := json.Unmarshal(reply["error"], &rpcErr); err != nil {
		t.Fatalf("decoding reply error: %v", err)
	}
	if rpcErr.Code != -32601 {
		t.Errorf("error code = %d, want -32601", rpcErr.Code)
	}
	if want := "method not found: " + method; rpcErr.Message != want {
		t.Errorf("error message = %q, want %q", rpcErr.Message, want)
	}
}

func TestAgentRequestNumericIDAnsweredWithMethodNotFound(t *testing.T) {
	f := startRequestingFake(t, "7")
	f.promptAndSettle(t)
	assertMethodNotFound(t, f.reply(t), "7", "fs/read_text_file")

	out, err := f.p.Peek(f.name, 0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	if !strings.Contains(out, "done") {
		t.Errorf("Peek = %q, want the turn's output after the reply", out)
	}
}

func TestAgentRequestStringIDEchoedAsString(t *testing.T) {
	f := startRequestingFake(t, `"abc"`)
	f.promptAndSettle(t)
	assertMethodNotFound(t, f.reply(t), `"abc"`, "fs/read_text_file")
}

func TestInitializeAdvertisesNoFSOrTerminal(t *testing.T) {
	f := startRequestingFake(t, "7")
	raw, err := os.ReadFile(f.initFile)
	if err != nil {
		t.Fatalf("reading initialize params: %v", err)
	}
	var params map[string]json.RawMessage
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatalf("decoding initialize params: %v", err)
	}
	var got any
	if err := json.Unmarshal(params["clientCapabilities"], &got); err != nil {
		t.Fatalf("decoding clientCapabilities %s: %v", params["clientCapabilities"], err)
	}
	gotJSON, _ := json.Marshal(got)
	const want = `{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false}`
	if string(gotJSON) != want {
		t.Errorf("clientCapabilities = %s, want %s", gotJSON, want)
	}
}

func TestClassifyInboundLine(t *testing.T) {
	cases := []struct {
		name       string
		line       string
		wantReq    bool
		wantID     string
		wantMethod string
	}{
		{"numeric id request", `{"jsonrpc":"2.0","id":3,"method":"fs/read_text_file","params":{}}`, true, "3", "fs/read_text_file"},
		{"string id request", `{"jsonrpc":"2.0","id":"perm-1","method":"session/request_permission"}`, true, `"perm-1"`, "session/request_permission"},
		{"terminal request", `{"jsonrpc":"2.0","id":0,"method":"terminal/create"}`, true, "0", "terminal/create"},
		{"session update notification", `{"jsonrpc":"2.0","method":"session/update","params":{}}`, false, "", ""},
		{"cancel request notification", `{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`, false, "", ""},
		{"null id is not a request", `{"jsonrpc":"2.0","id":null,"method":"x"}`, false, "", ""},
		{"response", `{"jsonrpc":"2.0","id":4,"result":{}}`, false, "", ""},
		{"string id response", `{"jsonrpc":"2.0","id":"abc","result":{}}`, false, "", ""},
		{"banner", `starting agent...`, false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, ok := parseAgentRequest([]byte(tc.line))
			if ok != tc.wantReq {
				t.Fatalf("parseAgentRequest ok = %v, want %v", ok, tc.wantReq)
			}
			if !ok {
				return
			}
			if string(req.ID) != tc.wantID || req.Method != tc.wantMethod {
				t.Errorf("request = {id %s, method %q}, want {id %s, method %q}", req.ID, req.Method, tc.wantID, tc.wantMethod)
			}
		})
	}
}

func TestMethodNotFoundReplyEchoesIDBytes(t *testing.T) {
	for _, id := range []string{`12`, `"abc"`, `"<a&b>"`, `"é"`, `-1`, `1.5e3`} {
		data, err := encodeMethodNotFound(agentRequest{ID: json.RawMessage(id), Method: "fs/write_text_file"})
		if err != nil {
			t.Fatalf("encodeMethodNotFound(%s): %v", id, err)
		}
		want := `{"jsonrpc":"2.0","id":` + id + `,"error":{"code":-32601,"message":"method not found: fs/write_text_file"}}`
		if string(data) != want {
			t.Errorf("reply = %s, want %s", data, want)
		}
	}
}

func TestUnsupportedMethodLoggedOncePerConnection(t *testing.T) {
	sc := newSessionConn(nil, nil, nil, 10, nil)
	if !sc.firstUnsupported("fs/read_text_file") {
		t.Fatal("first fs/read_text_file not reported")
	}
	if sc.firstUnsupported("fs/read_text_file") {
		t.Fatal("repeated fs/read_text_file reported again")
	}
	if !sc.firstUnsupported("terminal/create") {
		t.Fatal("first terminal/create not reported")
	}
	other := newSessionConn(nil, nil, nil, 10, nil)
	if !other.firstUnsupported("fs/read_text_file") {
		t.Fatal("dedup leaked across connections")
	}
}

// TestAgentRequestReplyDoesNotBlockReadLoop proves a reply stuck behind a
// backpressured stdin does not stall routing of later agent messages.
func TestAgentRequestReplyDoesNotBlockReadLoop(t *testing.T) {
	stdinR, stdinW := io.Pipe() // nobody reads until the end: stdin is backpressured
	sc := newSessionConn(nil, stdinW, nil, 10, nil)
	respCh := make(chan JSONRPCMessage, 1)
	sc.pending[42] = respCh

	lines := strings.Join([]string{
		`{"jsonrpc":"2.0","id":"r1","method":"fs/read_text_file","params":{}}`,
		`{"jsonrpc":"2.0","id":42,"result":{}}`,
	}, "\n") + "\n"
	go sc.readLoop(strings.NewReader(lines))
	loopTimer := time.NewTimer(protocolUnitWait)
	defer loopTimer.Stop()
	select {
	case <-sc.readDone:
	case <-loopTimer.C:
		t.Fatal("read loop blocked behind the reply to the agent request")
	}

	select {
	case msg := <-respCh:
		if msg.ID == nil || *msg.ID != 42 {
			t.Fatalf("routed response id = %v, want 42", msg.ID)
		}
	default:
		t.Fatal("response after the agent request was not routed")
	}

	// Unblock stdin: the queued reply is delivered intact.
	type readResult struct {
		line string
		err  error
	}
	got := make(chan readResult, 1)
	go func() {
		line, err := bufio.NewReader(stdinR).ReadString('\n')
		got <- readResult{line, err}
	}()
	t.Cleanup(func() { _ = stdinR.Close() })
	timer := time.NewTimer(protocolUnitWait)
	defer timer.Stop()
	select {
	case r := <-got:
		if r.err != nil {
			t.Fatalf("reading queued reply: %v", r.err)
		}
		want := `{"jsonrpc":"2.0","id":"r1","error":{"code":-32601,"message":"method not found: fs/read_text_file"}}` + "\n"
		if r.line != want {
			t.Errorf("reply = %q, want %q", r.line, want)
		}
	case <-timer.C:
		t.Fatal("no reply written to stdin for the agent request")
	}
}
