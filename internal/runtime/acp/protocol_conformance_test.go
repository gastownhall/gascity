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

func TestACPProtocolPermissionTimeoutRejects(t *testing.T) {
	// gc on this base does not answer session/request_permission, so the
	// fake's timeout path fires: $/cancel_request, then a rejected tool.
	s := startProtocolFake(t, "--request-permission", "--permission-timeout", "50ms")
	s.nudge(t, "needs approval")
	s.waitIdle(t)

	out := s.peek(t)
	if !strings.Contains(out, "permission rejected") || !strings.Contains(out, "echo: needs approval") {
		t.Fatalf("Peek = %q, want rejected tool output then the echo", out)
	}
	if got := s.jsonlRecords(t, "responses.jsonl"); len(got) != 0 {
		t.Fatalf("responses.jsonl = %v, want no client replies", got)
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
