package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeACPInterruptCommand returns an inline python3 ACP agent for the
// Interrupt tests. Every inbound message line is appended to $GC_WIRE_LOG in
// arrival order, and every SIGINT as {"signal":"SIGINT"}, so the log shows
// the order in which gc wrote to the agent. A prompt whose text is "hold"
// stays open until canceled; any other prompt is echoed and answered. With
// $GC_ASK_PERMISSION set, a held prompt first sends session/request_permission
// with id 7. $GC_ON_CANCEL is reply (answer held prompts canceled after
// $GC_CANCEL_DELAY seconds) or ignore. $GC_SIGINT is ignore, cancel (answer
// held prompts canceled) or exit (status 130).
func fakeACPInterruptCommand() string {
	return `exec python3 -u -c '
import sys, json, os, signal, threading
# Reentrant: Python runs on_sig on the main thread, which may hold lock.
lock = threading.RLock()
held = []
log_path = os.environ["GC_WIRE_LOG"]
on_cancel = os.environ.get("GC_ON_CANCEL", "reply")
on_sigint = os.environ.get("GC_SIGINT", "ignore")
cancel_delay = float(os.environ.get("GC_CANCEL_DELAY", "0"))
STOP_CANCELED = "` + acpStopReasonCancelled + `"

def log(line):
    with lock:
        with open(log_path, "a") as f:
            f.write(line + "\n")

def send(msg):
    with lock:
        print(json.dumps(msg), flush=True)

def cancel_held():
    with lock:
        ids = list(held)
        held.clear()
    for pid in ids:
        send({"jsonrpc": "2.0", "id": pid, "result": {"stopReason": STOP_CANCELED}})

def on_sig(signum, frame):
    log(json.dumps({"signal": "SIGINT"}))
    if on_sigint == "exit":
        os._exit(130)
    if on_sigint == "cancel":
        cancel_held()

signal.signal(signal.SIGINT, on_sig)
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    try:
        msg = json.loads(line)
    except Exception:
        log(line)
        continue
    method = msg.get("method", "")
    text = ""
    if method == "session/prompt":
        text = "".join(b.get("text", "") for b in msg["params"].get("prompt", []))
    if text == "hold":
        # Hold the turn before logging its prompt, so a signal sent once the
        # log shows the prompt finds the turn to cancel.
        with lock:
            held.append(msg["id"])
    log(line)
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {"serverInfo": {"name": "fake"}}})
    elif method == "session/new":
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {"sessionId": "s1"}})
    elif method == "session/prompt":
        if text == "hold":
            if os.environ.get("GC_ASK_PERMISSION"):
                send({"jsonrpc": "2.0", "id": 7, "method": "session/request_permission",
                      "params": {"sessionId": "s1",
                                 "toolCall": {"toolCallId": "call_1", "kind": "execute", "title": "Run: touch marker"},
                                 "options": [{"optionId": "yes", "name": "Allow once", "kind": "allow_once"}]}})
        else:
            send({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "s1",
                  "update": {"sessionUpdate": "agent_message_chunk", "content": {"type": "text", "text": "echo: " + text}}}})
            send({"jsonrpc": "2.0", "id": msg["id"], "result": {"stopReason": "end_turn"}})
    elif method == "session/cancel" and on_cancel == "reply":
        threading.Timer(cancel_delay, cancel_held).start()
'`
}

// interruptFake is one inline python agent started through Provider.Start.
type interruptFake struct {
	p              *Provider
	name           string
	wireLog        string
	transcriptRoot string
}

// startInterruptFake starts fakeACPInterruptCommand with env and a provider
// whose CancelTimeout is cancelTimeout. JSON-RPC capture is on, with the
// session identity of captureSessionEnv.
func startInterruptFake(t *testing.T, cancelTimeout time.Duration, env map[string]string) *interruptFake {
	t.Helper()
	dir := filepath.Join(shortTempDir(t), "acp")
	transcriptRoot := t.TempDir()
	f := &interruptFake{
		p: NewProviderWithDir(dir, Config{
			HandshakeTimeout:  protocolUnitWait,
			NudgeBusyTimeout:  protocolUnitWait,
			OutputBufferLines: 100,
			CancelTimeout:     cancelTimeout,
			TranscriptRoot:    transcriptRoot,
		}),
		name:           testName(),
		wireLog:        filepath.Join(t.TempDir(), "wire.jsonl"),
		transcriptRoot: transcriptRoot,
	}
	cfgEnv := captureSessionEnv()
	cfgEnv["GC_WIRE_LOG"] = f.wireLog
	for k, v := range env {
		cfgEnv[k] = v
	}
	if err := f.p.Start(context.Background(), f.name, runtime.Config{
		Command: fakeACPInterruptCommand(),
		WorkDir: t.TempDir(),
		Env:     cfgEnv,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.p.Stop(f.name) })
	return f
}

func (f *interruptFake) conn(t *testing.T) *sessionConn {
	t.Helper()
	f.p.mu.Lock()
	sc := f.p.conns[f.name]
	f.p.mu.Unlock()
	if sc == nil {
		t.Fatal("no in-process connection")
	}
	return sc
}

// hold sends a prompt the fake keeps open and waits until the fake has
// logged it, so a cancel or signal sent afterwards finds the turn.
func (f *interruptFake) hold(t *testing.T) {
	t.Helper()
	if err := f.p.Nudge(f.name, runtime.TextContent("hold")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	waitUntil(t, "the fake to log the held prompt", func() bool {
		return f.countMethod(t, "session/prompt") == 1
	})
}

// endTurnBySIGINT ends the open turn without session/cancel: under
// GC_SIGINT=cancel the fake answers its held prompts on SIGINT.
func endTurnBySIGINT(t *testing.T, sc *sessionConn) {
	t.Helper()
	if err := sc.cmd.Process.Signal(syscall.SIGINT); err != nil {
		t.Fatalf("SIGINT: %v", err)
	}
	if !sc.waitIdle(protocolUnitWait) {
		t.Fatal("held turn did not end on SIGINT")
	}
}

// wire returns the fake's inbound log records in arrival order.
func (f *interruptFake) wire(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	file, err := os.Open(f.wireLog)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close() //nolint:errcheck // read-only
	var out []map[string]json.RawMessage
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var rec map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatalf("decoding wire record %q: %v", scanner.Text(), err)
		}
		out = append(out, rec)
	}
	return out
}

func (f *interruptFake) countMethod(t *testing.T, method string) int {
	t.Helper()
	n := 0
	want := `"` + method + `"`
	for _, rec := range f.wire(t) {
		if string(rec["method"]) == want {
			n++
		}
	}
	return n
}

func (f *interruptFake) countSIGINT(t *testing.T) int {
	t.Helper()
	n := 0
	for _, rec := range f.wire(t) {
		if string(rec["signal"]) == `"SIGINT"` {
			n++
		}
	}
	return n
}

// wireOrder returns each inbound record's method, "SIGINT" for a signal, or
// "reply" for a response, in arrival order, for failure messages.
func (f *interruptFake) wireOrder(t *testing.T) []string {
	t.Helper()
	var order []string
	for _, rec := range f.wire(t) {
		var method string
		switch {
		case json.Unmarshal(rec["method"], &method) == nil:
			order = append(order, method)
		case string(rec["signal"]) == `"SIGINT"`:
			order = append(order, "SIGINT")
		default:
			order = append(order, "reply")
		}
	}
	return order
}

// stopForTranscript stops the agent, which flushes and closes its capture
// transcript, and returns the transcript's shape (see captureShape).
func (f *interruptFake) stopForTranscript(t *testing.T) []string {
	t.Helper()
	if err := f.p.Stop(f.name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(f.transcriptRoot, "s-1", "2.jsonl"))
	if err != nil {
		t.Fatalf("reading capture transcript: %v", err)
	}
	return captureShape(t, parseCaptureLines(t, data))
}

// withInterruptSIGINTWait shortens the wait after each fallback SIGINT. It
// sets a package variable, so a test that uses it must not run in parallel.
func withInterruptSIGINTWait(t *testing.T, d time.Duration) {
	t.Helper()
	prev := interruptSIGINTWait
	interruptSIGINTWait = d
	t.Cleanup(func() { interruptSIGINTWait = prev })
}

func TestInterruptIdleSendsNoCancelOrSignal(t *testing.T) {
	f := startInterruptFake(t, time.Second, nil)
	if err := f.p.Interrupt(f.name); err != nil {
		t.Fatalf("Interrupt(idle) = %v, want nil", err)
	}
	// A full prompt round trip after the Interrupt is the barrier: anything
	// the Interrupt wrote or signaled is logged before this prompt is.
	if err := f.p.Nudge(f.name, runtime.TextContent("after")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if !f.conn(t).waitIdle(protocolUnitWait) {
		t.Fatal("prompt after an idle Interrupt never completed")
	}
	if n := f.countMethod(t, methodSessionCancel); n != 0 {
		t.Fatalf("idle Interrupt sent %d session/cancel, want 0", n)
	}
	if n := f.countSIGINT(t); n != 0 {
		t.Fatalf("idle Interrupt sent %d SIGINT, want 0", n)
	}
}

func TestInterruptSettlesBySessionCancel(t *testing.T) {
	f := startInterruptFake(t, protocolUnitWait, map[string]string{"GC_CANCEL_DELAY": "0.2"})
	f.hold(t)
	start := time.Now()
	if err := f.p.Interrupt(f.name); err != nil {
		t.Fatalf("Interrupt = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed < 200*time.Millisecond {
		t.Fatalf("Interrupt returned after %v, want it to wait for the 200ms settle", elapsed)
	}
	if f.conn(t).isBusy() {
		t.Fatal("turn still busy after Interrupt returned")
	}
	if n := f.countMethod(t, methodSessionCancel); n != 1 {
		t.Fatalf("session/cancel count = %d, want 1", n)
	}
	if n := f.countSIGINT(t); n != 0 {
		t.Fatalf("SIGINT count = %d, want 0 when session/cancel settles the turn", n)
	}
}

func TestInterruptSessionCancelCarriesSessionID(t *testing.T) {
	f := startInterruptFake(t, protocolUnitWait, nil)
	f.hold(t)
	if err := f.p.Interrupt(f.name); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	for _, rec := range f.wire(t) {
		if string(rec["method"]) != `"`+methodSessionCancel+`"` {
			continue
		}
		if _, ok := rec["id"]; ok {
			t.Fatalf("session/cancel = %v, want a notification without an id", rec)
		}
		if got := string(rec["params"]); got != `{"sessionId":"s1"}` {
			t.Fatalf("session/cancel params = %s, want {\"sessionId\":\"s1\"}", got)
		}
		return
	}
	t.Fatal("no session/cancel logged")
}

func TestInterruptAnswersPermissionsBeforeSessionCancel(t *testing.T) {
	f := startInterruptFake(t, protocolUnitWait, map[string]string{"GC_ASK_PERMISSION": "1"})
	if err := f.p.Nudge(f.name, runtime.TextContent("hold")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	waitUntil(t, "a pending permission", func() bool {
		pending, err := f.p.Pending(f.name)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		return pending != nil
	})
	if err := f.p.Interrupt(f.name); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if pending, err := f.p.Pending(f.name); err != nil || pending != nil {
		t.Fatalf("Pending after Interrupt = %#v, %v; want nil, nil", pending, err)
	}
	var order []string
	for _, rec := range f.wire(t) {
		switch {
		case string(rec["id"]) == "7" && len(rec["method"]) == 0:
			var result any
			if err := json.Unmarshal(rec["result"], &result); err != nil {
				t.Fatalf("decoding permission reply %v: %v", rec, err)
			}
			got, _ := json.Marshal(result)
			if string(got) != outcomeCancelled {
				t.Fatalf("permission reply result = %s, want %s", got, outcomeCancelled)
			}
			order = append(order, "permission-reply")
		case string(rec["method"]) == `"`+methodSessionCancel+`"`:
			order = append(order, "session/cancel")
		}
	}
	if len(order) != 2 || order[0] != "permission-reply" || order[1] != "session/cancel" {
		t.Fatalf("wire order = %v, want [permission-reply session/cancel]", order)
	}
}

// TestInterruptCancelNeverFollowsTheNextPrompt is the regression for a stale
// session/cancel. The interrupted turn ends, and Nudge starts the next one,
// after cancelTurn has written the permission replies but before it writes
// session/cancel. A cancel written now would follow the next prompt on the
// wire and cancel that turn instead, so it must not be sent at all. Nor may
// it be captured: the transcript must still mirror the wire. Only a next
// turn still open at that write tells a guard on the interrupted turn from a
// guard on whichever turn is current.
func TestInterruptCancelNeverFollowsTheNextPrompt(t *testing.T) {
	cases := []struct {
		name string
		// next is the next turn's prompt. The fake keeps a "hold" turn open
		// and answers any other prompt at once.
		next string
	}{
		{name: "next turn over", next: "next"},
		{name: "next turn open", next: "hold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := startInterruptFake(t, protocolUnitWait, map[string]string{"GC_SIGINT": "cancel"})
			sc := f.conn(t)
			f.hold(t)
			sc.mu.Lock()
			idleCh := sc.idleCh
			sessionID := sc.sessionID
			sc.mu.Unlock()
			sc.beforeTurnCancel = func() {
				endTurnBySIGINT(t, sc)
				if err := f.p.Nudge(f.name, runtime.TextContent(tc.next)); err != nil {
					t.Fatalf("Nudge: %v", err)
				}
				waitUntil(t, "the fake to log the next prompt", func() bool {
					return f.countMethod(t, "session/prompt") == 2
				})
				if tc.next != "hold" && !sc.waitIdle(protocolUnitWait) {
					t.Fatal("next turn never completed")
				}
			}
			sc.cancelTurn(f.name, sessionID, idleCh, nil)
			if tc.next == "hold" {
				endTurnBySIGINT(t, sc)
			}
			// A full prompt round trip is the barrier: anything cancelTurn
			// wrote is logged before this prompt is.
			if err := f.p.Nudge(f.name, runtime.TextContent("after")); err != nil {
				t.Fatalf("Nudge: %v", err)
			}
			if !sc.waitIdle(protocolUnitWait) {
				t.Fatal("barrier prompt never completed")
			}
			if n := f.countMethod(t, methodSessionCancel); n != 0 {
				t.Fatalf("session/cancel sent %d time(s) after its turn ended; wire order = %v", n, f.wireOrder(t))
			}
			prompts, cancels := 0, 0
			shape := f.stopForTranscript(t)
			for _, s := range shape {
				switch s {
				case "out session/prompt":
					prompts++
				case "out " + methodSessionCancel:
					cancels++
				}
			}
			// The three prompts show the transcript ran through the barrier,
			// so an absent cancel is evidence rather than a capture that
			// never started.
			if prompts != 3 {
				t.Fatalf("transcript holds %d session/prompt records, want 3 (held, %s, after); shape = %v", prompts, tc.next, shape)
			}
			if cancels != 0 {
				t.Fatalf("transcript holds %d session/cancel record(s) that never reached the agent; shape = %v", cancels, shape)
			}
		})
	}
}

// TestInterruptSendsNoSIGINTAfterTheTurnEnds is the regression for a late
// fallback SIGINT. The turn ends after the wait for session/cancel has given
// up on it but before the SIGINT goes out. A SIGINT sent now would reach an
// idle agent, or the next turn, so it must not be sent at all.
func TestInterruptSendsNoSIGINTAfterTheTurnEnds(t *testing.T) {
	f := startInterruptFake(t, 50*time.Millisecond, map[string]string{
		"GC_ON_CANCEL": "ignore",
		"GC_SIGINT":    "cancel",
	})
	sc := f.conn(t)
	f.hold(t)
	fallbacks := 0
	sc.beforeTurnSignal = func() {
		fallbacks++
		endTurnBySIGINT(t, sc)
	}
	if err := f.p.Interrupt(f.name); err != nil {
		t.Fatalf("Interrupt = %v, want nil once the turn has ended", err)
	}
	// A full prompt round trip is the barrier: anything Interrupt signaled
	// is logged before this prompt is.
	if err := f.p.Nudge(f.name, runtime.TextContent("after")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if !sc.waitIdle(protocolUnitWait) {
		t.Fatal("barrier prompt never completed")
	}
	if fallbacks != 1 {
		t.Fatalf("Interrupt reached its SIGINT fallback %d time(s), want 1", fallbacks)
	}
	if n := f.countSIGINT(t); n != 1 {
		t.Fatalf("SIGINT count = %d, want only the test's own 1; wire order = %v", n, f.wireOrder(t))
	}
}

func TestInterruptEscalatesToSIGINTWhenCancelIgnored(t *testing.T) {
	withInterruptSIGINTWait(t, protocolUnitWait)
	f := startInterruptFake(t, 100*time.Millisecond, map[string]string{
		"GC_ON_CANCEL": "ignore",
		"GC_SIGINT":    "cancel",
	})
	f.hold(t)
	if err := f.p.Interrupt(f.name); err != nil {
		t.Fatalf("Interrupt = %v, want nil after SIGINT settles the turn", err)
	}
	if f.conn(t).isBusy() {
		t.Fatal("turn still busy after Interrupt returned")
	}
	if n := f.countSIGINT(t); n != 1 {
		t.Fatalf("SIGINT count = %d, want 1", n)
	}
	if !f.conn(t).alive() {
		t.Fatal("agent exited; Interrupt must not kill it")
	}
}

func TestInterruptReturnsNotSettledAndLeavesAgentRunning(t *testing.T) {
	withInterruptSIGINTWait(t, 50*time.Millisecond)
	f := startInterruptFake(t, 50*time.Millisecond, map[string]string{"GC_ON_CANCEL": "ignore"})
	f.hold(t)
	err := f.p.Interrupt(f.name)
	if !errors.Is(err, runtime.ErrInterruptNotSettled) {
		t.Fatalf("Interrupt = %v, want ErrInterruptNotSettled", err)
	}
	if !f.conn(t).alive() {
		t.Fatal("agent exited; Interrupt must never kill it")
	}
	if !f.conn(t).isBusy() {
		t.Fatal("turn reads idle although the agent never answered it")
	}
	waitUntil(t, "both SIGINTs to be logged", func() bool { return f.countSIGINT(t) == interruptSIGINTAttempts })
}

func TestInterruptAgentExitCountsAsSettled(t *testing.T) {
	withInterruptSIGINTWait(t, protocolUnitWait)
	f := startInterruptFake(t, 50*time.Millisecond, map[string]string{
		"GC_ON_CANCEL": "ignore",
		"GC_SIGINT":    "exit",
	})
	f.hold(t)
	if err := f.p.Interrupt(f.name); err != nil {
		t.Fatalf("Interrupt = %v, want nil when the agent exits", err)
	}
	waitExited(t, f.conn(t))
}

// TestInterruptKeepsAgentThatExitsOnSIGINT is the regression for agents that
// exit on SIGINT: session/cancel settles the turn, no signal is sent, and the
// same process serves the next prompt.
func TestInterruptKeepsAgentThatExitsOnSIGINT(t *testing.T) {
	f := startInterruptFake(t, protocolUnitWait, map[string]string{"GC_SIGINT": "exit"})
	sc := f.conn(t)
	pid := sc.cmd.Process.Pid
	f.hold(t)
	if err := f.p.Interrupt(f.name); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	if err := f.p.Nudge(f.name, runtime.TextContent("next")); err != nil {
		t.Fatalf("Nudge after Interrupt: %v", err)
	}
	if !sc.waitIdle(protocolUnitWait) {
		t.Fatal("prompt after Interrupt never completed")
	}
	if !sc.alive() {
		t.Fatal("agent exited after Interrupt")
	}
	if got := f.conn(t); got != sc || got.cmd.Process.Pid != pid {
		t.Fatalf("connection replaced after Interrupt; want the same pid %d", pid)
	}
	if out, _ := f.p.Peek(f.name, 0); out != "echo: next" {
		t.Fatalf("Peek = %q, want the next prompt served", out)
	}
	if n := f.countSIGINT(t); n != 0 {
		t.Fatalf("SIGINT count = %d, want 0", n)
	}
}

// TestInterruptCrossProcessRunsCancelInOwner drives Interrupt from a provider
// that does not own the connection: the control socket routes it to the
// owner, which sends session/cancel rather than a bare SIGINT. The call is
// fire-and-forget: it returns once the owner has the command, while the turn
// is still settling, and the owner carries the settle through to its SIGINT
// fallback after the caller has gone.
func TestInterruptCrossProcessRunsCancelInOwner(t *testing.T) {
	// Each case settles 1s after session/cancel, midway inside the 2s socket
	// deadline of Provider.Interrupt. A handler that waited for the settle
	// would reply before that deadline and fail the busy check below. Past
	// 2s the client would time out while the turn is still open, and that
	// regression would pass, so the settle must stay under the deadline.
	cases := []struct {
		name          string
		cancelTimeout time.Duration
		env           map[string]string
		wantSIGINT    int
	}{
		{
			name:          "cancel settles",
			cancelTimeout: protocolUnitWait,
			env:           map[string]string{"GC_CANCEL_DELAY": "1"},
		},
		{
			// The agent ignores session/cancel, so the turn ends only when
			// the owner's 1s cancel timeout runs out and its SIGINT lands.
			name:          "SIGINT fallback settles",
			cancelTimeout: time.Second,
			env:           map[string]string{"GC_ON_CANCEL": "ignore", "GC_SIGINT": "cancel"},
			wantSIGINT:    1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := startInterruptFake(t, tc.cancelTimeout, tc.env)
			f.hold(t)
			other := NewProviderWithDir(f.p.dir, Config{})
			// A cross-process Interrupt always returns nil, so this checks the
			// contract only; the owner's state and wire log below are the
			// evidence.
			if err := other.Interrupt(f.name); err != nil {
				t.Fatalf("cross-process Interrupt = %v, want nil", err)
			}
			if !f.conn(t).isBusy() {
				t.Fatal("cross-process Interrupt waited for the owner's 1s settle; want it to return while the turn is still open")
			}
			if !f.conn(t).waitIdle(protocolUnitWait) {
				t.Fatal("cross-process Interrupt did not settle the turn")
			}
			if n := f.countMethod(t, methodSessionCancel); n != 1 {
				t.Fatalf("session/cancel count = %d, want 1", n)
			}
			if n := f.countSIGINT(t); n != tc.wantSIGINT {
				t.Fatalf("SIGINT count = %d, want %d; wire order = %v", n, tc.wantSIGINT, f.wireOrder(t))
			}
		})
	}
}

func TestInterruptUnknownSessionIsNil(t *testing.T) {
	p := newTestProvider(t)
	if err := p.Interrupt("no-such-session"); err != nil {
		t.Fatalf("Interrupt(unknown) = %v, want nil", err)
	}
}

func TestNewSessionCancelNotification(t *testing.T) {
	msg := newSessionCancelNotification("sess-9")
	if msg.ID != nil {
		t.Fatalf("session/cancel has id %d, want a notification", *msg.ID)
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"sess-9"}}`
	if string(data) != want {
		t.Fatalf("session/cancel = %s, want %s", data, want)
	}
}
