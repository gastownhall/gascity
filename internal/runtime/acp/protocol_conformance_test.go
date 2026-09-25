//go:build integration

package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// protocolWait bounds every fact-based wait in the ACP protocol cases. The
// fake answers in microseconds; the bound only turns a hang into a failure.
const protocolWait = 10 * time.Second

var protocolCounter atomic.Int64

// protocolSession is one fakeacp process started through Provider.Start.
type protocolSession struct {
	p      *Provider
	name   string
	logDir string
}

// startProtocolFake builds testdata/fakeacp (once per test), starts it with
// the given scenario flags plus --log-dir, and registers cleanup.
func startProtocolFake(t *testing.T, args ...string) *protocolSession {
	t.Helper()
	var fixture acpConformanceFixture
	if err := prepareACPConformanceFixture(t, &fixture); err != nil {
		t.Fatal(err)
	}
	logDir := t.TempDir()
	p := NewProviderWithDir(fixture.dir, Config{
		HandshakeTimeout:  protocolWait,
		NudgeBusyTimeout:  protocolWait,
		OutputBufferLines: 100,
	})
	name := fmt.Sprintf("gc-acp-proto-%d-%d", os.Getpid(), protocolCounter.Add(1))
	command := "exec " + shellQuote(fixture.command) + " --log-dir " + shellQuote(logDir)
	for _, arg := range args {
		command += " " + shellQuote(arg)
	}
	if err := p.Start(context.Background(), name, runtime.Config{
		Command: command,
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = p.Stop(name) })
	return &protocolSession{p: p, name: name, logDir: logDir}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// conn returns the in-process connection for the session.
func (s *protocolSession) conn(t *testing.T) *sessionConn {
	t.Helper()
	s.p.mu.Lock()
	sc, ok := s.p.conns[s.name]
	s.p.mu.Unlock()
	if !ok {
		t.Fatalf("no in-process connection for %q", s.name)
	}
	return sc
}

// nudge sends one text prompt.
func (s *protocolSession) nudge(t *testing.T, text string) {
	t.Helper()
	if err := s.p.Nudge(s.name, runtime.TextContent(text)); err != nil {
		t.Fatalf("Nudge(%q): %v", text, err)
	}
}

// waitIdle waits on the connection's idle channel, which closes when the
// in-flight session/prompt is answered (or the connection drains).
func (s *protocolSession) waitIdle(t *testing.T) {
	t.Helper()
	if !s.conn(t).waitIdle(protocolWait) {
		t.Fatalf("session %q still busy after %s", s.name, protocolWait)
	}
}

func (s *protocolSession) peek(t *testing.T) string {
	t.Helper()
	out, err := s.p.Peek(s.name, 0)
	if err != nil {
		t.Fatalf("Peek: %v", err)
	}
	return out
}

// jsonlRecords decodes one fake log file (missing file = no records).
func (s *protocolSession) jsonlRecords(t *testing.T, file string) []map[string]any {
	t.Helper()
	f, err := os.Open(filepath.Join(s.logDir, file))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("open %s: %v", file, err)
	}
	defer f.Close() //nolint:errcheck // read-only
	var out []map[string]any
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		var rec map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("%s: decode %q: %v", file, scanner.Text(), err)
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("%s: scan: %v", file, err)
	}
	return out
}

func (s *protocolSession) methods(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, rec := range s.jsonlRecords(t, "methods.jsonl") {
		m, _ := rec["method"].(string)
		out = append(out, m)
	}
	return out
}

func TestACPProtocolPromptEchoFidelity(t *testing.T) {
	s := startProtocolFake(t)
	s.nudge(t, "hello")
	s.waitIdle(t)

	if out := s.peek(t); !strings.Contains(out, "echo: hello") {
		t.Fatalf("Peek = %q, want it to contain %q", out, "echo: hello")
	}
	prompts := s.jsonlRecords(t, "prompts.jsonl")
	if len(prompts) != 1 || prompts[0]["text"] != "hello" {
		t.Fatalf("prompts.jsonl = %v, want one prompt with text hello", prompts)
	}
}

func TestACPProtocolFakeLogsHandshake(t *testing.T) {
	s := startProtocolFake(t)
	s.nudge(t, "ping")
	s.waitIdle(t)

	raw, err := os.ReadFile(filepath.Join(s.logDir, "initialize.json"))
	if err != nil {
		t.Fatalf("initialize.json: %v", err)
	}
	var params InitializeParams
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatalf("initialize.json decode: %v", err)
	}
	if params.ProtocolVersion != 1 || params.ClientInfo.Name != "gc" {
		t.Fatalf("initialize params = %+v, want protocolVersion 1 from gc", params)
	}

	got := s.methods(t)
	want := []string{"initialize", "initialized", "session/new", "session/prompt"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("methods.jsonl = %v, want %v", got, want)
	}
	for _, rec := range s.jsonlRecords(t, "methods.jsonl") {
		if _, ok := rec["ts"].(string); !ok {
			t.Fatalf("methods.jsonl record %v has no ts", rec)
		}
	}
}

// TestACPProtocolChunkFragmentation pins current gc behavior: every
// agent_message_chunk becomes its own Peek line, even when the chunks share
// one messageId. Later PRs that reassemble chunks update this expectation.
func TestACPProtocolChunkFragmentation(t *testing.T) {
	s := startProtocolFake(t, "--chunks", "alpha|beta|gamma")
	s.nudge(t, "split")
	s.waitIdle(t)

	if out := s.peek(t); out != "alpha\nbeta\ngamma" {
		t.Fatalf("Peek = %q, want three chunk lines", out)
	}
}

func TestACPProtocolThoughtAndToolCall(t *testing.T) {
	s := startProtocolFake(t, "--thought", "thinking hard", "--tool-call", "--message-ids")
	s.nudge(t, "work")
	s.waitIdle(t)

	out := s.peek(t)
	for _, want := range []string{"thinking hard", "[tool: Read fixture]", "fixture contents", "echo: work"} {
		if !strings.Contains(out, want) {
			t.Fatalf("Peek = %q, want it to contain %q", out, want)
		}
	}
	if strings.Index(out, "thinking hard") > strings.Index(out, "echo: work") {
		t.Fatalf("Peek = %q, want thought before the message", out)
	}
}

func TestACPProtocolPromptErrorSettles(t *testing.T) {
	s := startProtocolFake(t, "--prompt-error", "boom")
	s.nudge(t, "fail please")
	s.waitIdle(t)

	if !s.conn(t).alive() {
		t.Fatal("fake exited after answering the prompt with an error")
	}
	if out := s.peek(t); strings.Contains(out, "echo:") {
		t.Fatalf("Peek = %q, want no echo for an errored prompt", out)
	}
}

func TestACPProtocolExitDuringPromptDrains(t *testing.T) {
	s := startProtocolFake(t, "--exit-during-prompt")
	s.nudge(t, "die")
	s.waitIdle(t)

	sc := s.conn(t)
	select {
	case <-sc.done:
	case <-time.After(protocolWait):
		t.Fatal("fake did not exit after --exit-during-prompt")
	}
	if got := s.methods(t); len(got) == 0 || got[len(got)-1] != "session/prompt" {
		t.Fatalf("methods.jsonl = %v, want the prompt logged before exit", got)
	}
}

func TestACPProtocolSigintCancelAnswersInFlightPrompt(t *testing.T) {
	s := startProtocolFake(t, "--sigint", "cancel", "--turn-delay", "1h")
	s.nudge(t, "long turn")
	if !s.conn(t).isBusy() {
		t.Fatal("prompt not in flight after Nudge")
	}
	// Interrupt signals the process group only after the fake has logged the
	// prompt, so the cancel cannot race the prompt's arrival.
	waitForMethod(t, s, "session/prompt")
	if err := s.p.Interrupt(s.name); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	s.waitIdle(t)

	if !s.conn(t).alive() {
		t.Fatal("fake exited on SIGINT with --sigint cancel")
	}
	signals := s.jsonlRecords(t, "signals.jsonl")
	if len(signals) == 0 || signals[0]["signal"] != syscall.SIGINT.String() {
		t.Fatalf("signals.jsonl = %v, want a SIGINT record", signals)
	}
	if out := s.peek(t); strings.Contains(out, "echo:") {
		t.Fatalf("Peek = %q, want no echo for a cancelled turn", out)
	}
}

// waitPermission waits until the fake's permission request is pending.
func (s *protocolSession) waitPermission(t *testing.T) *runtime.PendingInteraction {
	t.Helper()
	deadline := time.NewTimer(protocolWait)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		pending, err := s.p.Pending(s.name)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		if pending != nil {
			return pending
		}
		select {
		case <-deadline.C:
			t.Fatal("permission request never became pending")
		case <-tick.C:
		}
	}
}

// assertPermissionReply checks that the fake received exactly one client
// reply, echoing wantID, with the given RequestPermissionOutcome.
func assertPermissionReply(t *testing.T, s *protocolSession, wantID any, wantOutcome map[string]any) {
	t.Helper()
	// A cancelled reply is written asynchronously and can land after the
	// turn settles, so wait for the fake to log it.
	deadline := time.NewTimer(protocolWait)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	replies := s.jsonlRecords(t, "responses.jsonl")
	for len(replies) == 0 {
		select {
		case <-deadline.C:
			t.Fatal("fake never logged a client reply")
		case <-tick.C:
		}
		replies = s.jsonlRecords(t, "responses.jsonl")
	}
	if len(replies) != 1 {
		t.Fatalf("responses.jsonl = %v, want one client reply", replies)
	}
	reply := replies[0]
	if reply["id"] != wantID {
		t.Fatalf("reply id = %#v, want %#v", reply["id"], wantID)
	}
	result, _ := reply["result"].(map[string]any)
	got, _ := json.Marshal(result["outcome"])
	want, _ := json.Marshal(wantOutcome)
	if string(got) != string(want) {
		t.Fatalf("reply outcome = %s, want %s (reply %v)", got, want, reply)
	}
}

func TestACPProtocolPermissionApproveRoundTrip(t *testing.T) {
	s := startProtocolFake(t, "--request-permission")
	s.nudge(t, "needs approval")
	pending := s.waitPermission(t)
	if pending.RequestID != "acp-1" || pending.Kind != "approval" || pending.Prompt != "Run: touch marker" {
		t.Fatalf("Pending = %#v, want acp-1 approval for the tool call", pending)
	}
	if !s.conn(t).isBusy() {
		t.Fatal("turn not busy while the permission is outstanding")
	}
	if err := s.p.Respond(s.name, runtime.InteractionResponse{RequestID: pending.RequestID, Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	s.waitIdle(t)

	out := s.peek(t)
	if !strings.Contains(out, "permission granted: allow_once") || !strings.Contains(out, "echo: needs approval") {
		t.Fatalf("Peek = %q, want granted tool output then the echo", out)
	}
	assertPermissionReply(t, s, 1.0, map[string]any{"outcome": "selected", "optionId": "allow_once"})
}

func TestACPProtocolPermissionDenyStringID(t *testing.T) {
	s := startProtocolFake(t, "--request-permission", "--request-id-string")
	s.nudge(t, "needs approval")
	pending := s.waitPermission(t)
	if pending.RequestID != `acp-"perm-1"` {
		t.Fatalf("RequestID = %q, want acp-\"perm-1\"", pending.RequestID)
	}
	if err := s.p.Respond(s.name, runtime.InteractionResponse{RequestID: pending.RequestID, Action: "deny"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	s.waitIdle(t)

	if out := s.peek(t); !strings.Contains(out, "permission rejected: reject_once") {
		t.Fatalf("Peek = %q, want rejected tool output", out)
	}
	assertPermissionReply(t, s, "perm-1", map[string]any{"outcome": "selected", "optionId": "reject_once"})
}

func TestACPProtocolPermissionCancelledOnInterrupt(t *testing.T) {
	s := startProtocolFake(t, "--request-permission", "--sigint", "cancel")
	s.nudge(t, "needs approval")
	s.waitPermission(t)
	if err := s.p.Interrupt(s.name); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if pending, err := s.p.Pending(s.name); err != nil || pending != nil {
		t.Fatalf("Pending after Interrupt = %#v, %v; want nil, nil", pending, err)
	}
	s.waitIdle(t)

	if out := s.peek(t); strings.Contains(out, "echo:") || strings.Contains(out, "permission granted") {
		t.Fatalf("Peek = %q, want the cancelled turn to produce no echo or grant", out)
	}
	assertPermissionReply(t, s, 1.0, map[string]any{"outcome": "cancelled"}) //nolint:misspell // ACP wire spelling
}

func TestACPProtocolPermissionDroppedWhenAgentCancelsRequest(t *testing.T) {
	// The fake gives up on the request, sends $/cancel_request, and finishes
	// the turn; gc forgets the request without replying.
	s := startProtocolFake(t, "--request-permission", "--permission-timeout", "50ms")
	s.nudge(t, "needs approval")
	s.waitIdle(t)

	if pending, err := s.p.Pending(s.name); err != nil || pending != nil {
		t.Fatalf("Pending after the turn = %#v, %v; want nil, nil", pending, err)
	}
	if out := s.peek(t); !strings.Contains(out, "permission rejected: timeout") || !strings.Contains(out, "echo: needs approval") {
		t.Fatalf("Peek = %q, want the timed-out tool output then the echo", out)
	}
	if replies := s.jsonlRecords(t, "responses.jsonl"); len(replies) != 0 {
		t.Fatalf("responses.jsonl = %v, want no reply to a request the agent cancelled", replies)
	}
}

func TestACPProtocolFSReadAnsweredMethodNotFound(t *testing.T) {
	s := startProtocolFake(t, "--request-fs-read", "/etc/hostname")
	s.nudge(t, "read it")
	s.waitIdle(t)

	if out := s.peek(t); !strings.Contains(out, "echo: read it") {
		t.Fatalf("Peek = %q, want the turn to complete after the reply", out)
	}
	assertMethodNotFoundReplies(t, s, 1.0)
}

func TestACPProtocolStringIDRequestAnsweredMethodNotFound(t *testing.T) {
	s := startProtocolFake(t, "--request-fs-read", "/etc/hostname", "--request-id-string")
	s.nudge(t, "read it")
	s.waitIdle(t)

	if out := s.peek(t); !strings.Contains(out, "echo: read it") {
		t.Fatalf("Peek = %q, want the turn to complete after the reply", out)
	}
	assertMethodNotFoundReplies(t, s, "fs-1")
}

func TestACPProtocolInitializeAdvertisesNoFSOrTerminal(t *testing.T) {
	s := startProtocolFake(t)
	raw, err := os.ReadFile(filepath.Join(s.logDir, "initialize.json"))
	if err != nil {
		t.Fatalf("initialize.json: %v", err)
	}
	var params struct {
		ClientCapabilities json.RawMessage `json:"clientCapabilities"`
	}
	if err := json.Unmarshal(raw, &params); err != nil {
		t.Fatalf("initialize.json decode: %v", err)
	}
	var caps any
	if err := json.Unmarshal(params.ClientCapabilities, &caps); err != nil {
		t.Fatalf("clientCapabilities decode %s: %v", params.ClientCapabilities, err)
	}
	got, _ := json.Marshal(caps)
	const want = `{"fs":{"readTextFile":false,"writeTextFile":false},"terminal":false}`
	if string(got) != want {
		t.Fatalf("clientCapabilities = %s, want %s", got, want)
	}
}

// assertMethodNotFoundReplies checks that the fake received exactly one
// client reply, a -32601 error echoing wantID (a JSON number decodes as
// float64, a string id stays a string).
func assertMethodNotFoundReplies(t *testing.T, s *protocolSession, wantID any) {
	t.Helper()
	replies := s.jsonlRecords(t, "responses.jsonl")
	if len(replies) != 1 {
		t.Fatalf("responses.jsonl = %v, want one client reply", replies)
	}
	reply := replies[0]
	if reply["id"] != wantID {
		t.Fatalf("reply id = %#v, want %#v", reply["id"], wantID)
	}
	rpcErr, _ := reply["error"].(map[string]any)
	if rpcErr == nil || rpcErr["code"] != float64(-32601) {
		t.Fatalf("reply = %v, want error code -32601", reply)
	}
	if _, ok := reply["result"]; ok {
		t.Fatalf("reply = %v, want no result", reply)
	}
}

// waitForMethod waits until the fake has logged an inbound method. The log
// line is written before the fake acts on the message; each poll is a read
// of that fact, bounded by protocolWait.
func waitForMethod(t *testing.T, s *protocolSession, method string) {
	t.Helper()
	deadline := time.NewTimer(protocolWait)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		for _, m := range s.methods(t) {
			if m == method {
				return
			}
		}
		select {
		case <-deadline.C:
			t.Fatalf("fake never logged %q; methods = %v", method, s.methods(t))
		case <-tick.C:
		}
	}
}
