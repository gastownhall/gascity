package acp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// fakeACPPermissionCommand returns an inline python3 ACP agent that answers
// the handshake and, on session/prompt, sends one session/request_permission
// per raw JSON id in rawIDs. Every client reply (a message with an id and no
// method) is appended as one line to $GC_REPLY_FILE. The test drives the rest
// of the turn by writing private notifications to the agent's stdin:
// _test/finish answers the prompt, _test/cancel sends $/cancel_request for
// params.requestId, _test/ask sends one more permission request with id
// params.id, _test/exit exits. A session/cancel from gc answers the prompt
// canceled. $GC_PERM_OPTIONS (a JSON array) and
// $GC_PERM_TITLE ("-" omits the title) override the request contents. SIGINT
// is ignored so Interrupt leaves the agent running.
func fakeACPPermissionCommand(rawIDs ...string) string {
	args := ""
	for _, id := range rawIDs {
		args += " " + shellQuoteArg(id)
	}
	return `exec python3 -u -c '
import sys, json, os, signal
signal.signal(signal.SIGINT, signal.SIG_IGN)

def send(msg):
    print(json.dumps(msg), flush=True)

ids = [json.loads(a) for a in sys.argv[1:]]
options = json.loads(os.environ.get("GC_PERM_OPTIONS") or json.dumps([
    {"optionId": "yes", "name": "Allow once", "kind": "allow_once"},
    {"optionId": "yes-all", "name": "Always allow", "kind": "allow_always"},
    {"optionId": "no", "name": "Reject", "kind": "reject_once"},
    {"optionId": "no-all", "name": "Always reject", "kind": "reject_always"},
]))
title = os.environ.get("GC_PERM_TITLE", "Run: touch marker")
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
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {"serverInfo": {"name": "fake"}}})
    elif method == "session/new":
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {"sessionId": "s1"}})
    elif method == "session/prompt":
        prompt_id = msg["id"]
        for i, rid in enumerate(ids):
            tool = {"toolCallId": "call_%d" % i, "kind": "execute", "status": "pending"}
            if title != "-":
                tool["title"] = title
            send({"jsonrpc": "2.0", "id": rid, "method": "session/request_permission",
                  "params": {"sessionId": "s1", "toolCall": tool, "options": options}})
    elif method == "_test/finish":
        send({"jsonrpc": "2.0", "id": prompt_id, "result": {"stopReason": "end_turn"}})
    elif method == "session/cancel":
        send({"jsonrpc": "2.0", "id": prompt_id, "result": {"stopReason": "cancel" + "led"}})
    elif method == "_test/cancel":
        send({"jsonrpc": "2.0", "method": "$/cancel_request",
              "params": {"requestId": msg["params"]["requestId"]}})
    elif method == "_test/ask":
        send({"jsonrpc": "2.0", "id": msg["params"]["id"], "method": "session/request_permission",
              "params": {"sessionId": "s1", "options": options,
                         "toolCall": {"toolCallId": "call_ask", "kind": "execute", "title": title}}})
    elif method == "_test/exit":
        sys.exit(0)
    elif method == "" and "id" in msg:
        with open(os.environ["GC_REPLY_FILE"], "a") as f:
            f.write(line + "\n")
' ` + args
}

type permissionFake struct {
	p         *Provider
	name      string
	replyFile string
}

func startPermissionFake(t *testing.T, env map[string]string, rawIDs ...string) *permissionFake {
	t.Helper()
	f := &permissionFake{
		p:         newTestProvider(t),
		name:      testName(),
		replyFile: filepath.Join(t.TempDir(), "replies.jsonl"),
	}
	cfgEnv := map[string]string{"GC_REPLY_FILE": f.replyFile}
	for k, v := range env {
		cfgEnv[k] = v
	}
	if err := f.p.Start(context.Background(), f.name, runtime.Config{
		Command: fakeACPPermissionCommand(rawIDs...),
		WorkDir: t.TempDir(),
		Env:     cfgEnv,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = f.p.Stop(f.name) })
	return f
}

func (f *permissionFake) conn(t *testing.T) *sessionConn {
	t.Helper()
	f.p.mu.Lock()
	sc := f.p.conns[f.name]
	f.p.mu.Unlock()
	if sc == nil {
		t.Fatal("no in-process connection")
	}
	return sc
}

// prompt sends one prompt and waits until the first permission is pending.
func (f *permissionFake) prompt(t *testing.T) *runtime.PendingInteraction {
	t.Helper()
	if err := f.p.Nudge(f.name, runtime.TextContent("do the thing")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	return f.waitPending(t)
}

// waitPending polls Pending, a read of in-memory state, until an entry shows.
func (f *permissionFake) waitPending(t *testing.T) *runtime.PendingInteraction {
	t.Helper()
	var got *runtime.PendingInteraction
	waitUntil(t, "a pending permission", func() bool {
		pending, err := f.p.Pending(f.name)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		got = pending
		return pending != nil
	})
	return got
}

func (f *permissionFake) waitNoPending(t *testing.T) {
	t.Helper()
	waitUntil(t, "no pending permission", func() bool {
		pending, err := f.p.Pending(f.name)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		return pending == nil
	})
}

// tell writes a private test notification to the agent's stdin.
func (f *permissionFake) tell(t *testing.T, method string, params any) {
	t.Helper()
	msg := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		msg["params"] = params
	}
	data, err := json.Marshal(msg)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.conn(t).writeMessage(data); err != nil {
		t.Fatalf("writing %s: %v", method, err)
	}
}

func (f *permissionFake) replies(t *testing.T) []map[string]json.RawMessage {
	t.Helper()
	file, err := os.Open(f.replyFile)
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
			t.Fatalf("decoding reply %q: %v", scanner.Text(), err)
		}
		out = append(out, rec)
	}
	return out
}

// waitReplies waits until the agent has recorded n client replies.
func (f *permissionFake) waitReplies(t *testing.T, n int) []map[string]json.RawMessage {
	t.Helper()
	var got []map[string]json.RawMessage
	waitUntil(t, "client replies", func() bool {
		got = f.replies(t)
		return len(got) >= n
	})
	return got
}

// waitUntil polls cond, a cheap read of a fact, until it holds or
// protocolUnitWait elapses.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(protocolUnitWait)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

// waitExited waits until the agent process has exited and been drained.
func waitExited(t *testing.T, sc *sessionConn) {
	t.Helper()
	timer := time.NewTimer(protocolUnitWait)
	defer timer.Stop()
	select {
	case <-sc.done:
	case <-timer.C:
		t.Fatal("agent did not exit")
	}
}

// assertOutcome checks one reply's id and its RequestPermissionOutcome.
func assertOutcome(t *testing.T, reply map[string]json.RawMessage, wantID, wantOutcome string) {
	t.Helper()
	if got := string(reply["id"]); got != wantID {
		t.Errorf("reply id = %s, want %s", got, wantID)
	}
	if _, ok := reply["error"]; ok {
		t.Errorf("reply has error, want result: %v", reply)
	}
	var result any
	if err := json.Unmarshal(reply["result"], &result); err != nil {
		t.Fatalf("decoding reply result %s: %v", reply["result"], err)
	}
	got, _ := json.Marshal(result)
	if string(got) != wantOutcome {
		t.Errorf("reply result = %s, want %s", got, wantOutcome)
	}
}

// requestIDPattern is the RequestID shape: "acp-", a 12-hex-digit
// per-connection nonce, "-", then the raw JSON id.
var requestIDPattern = regexp.MustCompile(`^acp-[0-9a-f]{12}-(.+)$`)

// assertRequestID checks that got names the raw JSON id rawID on some
// connection.
func assertRequestID(t *testing.T, got, rawID string) {
	t.Helper()
	m := requestIDPattern.FindStringSubmatch(got)
	if m == nil || m[1] != rawID {
		t.Fatalf("RequestID = %q, want acp-<nonce>-%s", got, rawID)
	}
}

const outcomeCancelled = `{"outcome":{"outcome":"cancelled"}}` //nolint:misspell // ACP wire spelling

func selected(optionID string) string {
	return `{"outcome":{"optionId":"` + optionID + `","outcome":"selected"}}`
}

func TestPendingAndRespondUnknownSession(t *testing.T) {
	p := newTestProvider(t)

	pending, err := p.Pending("missing")
	if !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Pending error = %v, want ErrSessionNotFound", err)
	}
	if pending != nil {
		t.Fatalf("Pending = %#v, want nil", pending)
	}
	if err := p.Respond("missing", runtime.InteractionResponse{Action: "approve"}); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Respond error = %v, want ErrSessionNotFound", err)
	}
}

func TestPendingIdleSessionHasNothing(t *testing.T) {
	f := startPermissionFake(t, nil, "7")

	pending, err := f.p.Pending(f.name)
	if err != nil || pending != nil {
		t.Fatalf("Pending = %#v, %v; want nil, nil", pending, err)
	}
	err = f.p.Respond(f.name, runtime.InteractionResponse{RequestID: "acp-7", Action: "approve"})
	if !errors.Is(err, runtime.ErrInteractionResponseInvalid) {
		t.Fatalf("Respond with nothing outstanding = %v, want ErrInteractionResponseInvalid", err)
	}
}

func TestPermissionPendingShape(t *testing.T) {
	f := startPermissionFake(t, nil, "7")
	got := f.prompt(t)

	assertRequestID(t, got.RequestID, "7")
	want := &runtime.PendingInteraction{
		RequestID: got.RequestID,
		Kind:      "approval",
		Prompt:    "Run: touch marker",
		Options:   []string{"Allow once", "Always allow", "Reject", "Always reject"},
		Metadata: map[string]string{
			"source":        "acp",
			"tool_call_id":  "call_0",
			"tool_kind":     "execute",
			"option_count":  "4",
			"option_0_id":   "yes",
			"option_0_kind": "allow_once",
			"option_0_name": "Allow once",
			"option_1_id":   "yes-all",
			"option_1_kind": "allow_always",
			"option_1_name": "Always allow",
			"option_2_id":   "no",
			"option_2_kind": "reject_once",
			"option_2_name": "Reject",
			"option_3_id":   "no-all",
			"option_3_kind": "reject_always",
			"option_3_name": "Always reject",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Pending = %#v\nwant %#v", got, want)
	}
	again, err := f.p.Pending(f.name)
	if err != nil {
		t.Fatalf("Pending again: %v", err)
	}
	if !reflect.DeepEqual(again, want) {
		t.Fatalf("second Pending = %#v, want the same interaction", again)
	}
	if !f.conn(t).isBusy() {
		t.Fatal("turn not busy while a permission is outstanding")
	}
}

func TestPermissionPendingStringIDAndTitleFallback(t *testing.T) {
	f := startPermissionFake(t, map[string]string{"GC_PERM_TITLE": "-"}, `"perm-1"`)
	got := f.prompt(t)
	assertRequestID(t, got.RequestID, `"perm-1"`)
	if got.Prompt != "Permission requested" {
		t.Errorf("Prompt = %q, want fallback", got.Prompt)
	}

	if err := f.p.Respond(f.name, runtime.InteractionResponse{RequestID: got.RequestID, Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	assertOutcome(t, f.waitReplies(t, 1)[0], `"perm-1"`, selected("yes"))
}

func TestPermissionRespondActions(t *testing.T) {
	cases := []struct {
		action string
		want   string
	}{
		{"approve", selected("yes")},
		{"approve_always", selected("yes-all")},
		{"deny", selected("no")},
		{"deny_always", selected("no-all")},
		{"no-all", selected("no-all")}, // literal optionId
		{"cancel", outcomeCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			f := startPermissionFake(t, nil, "7")
			pending := f.prompt(t)
			if err := f.p.Respond(f.name, runtime.InteractionResponse{RequestID: pending.RequestID, Action: tc.action}); err != nil {
				t.Fatalf("Respond(%q): %v", tc.action, err)
			}
			replies := f.waitReplies(t, 1)
			assertOutcome(t, replies[0], "7", tc.want)

			after, err := f.p.Pending(f.name)
			if err != nil || after != nil {
				t.Fatalf("Pending after Respond = %#v, %v; want nil, nil", after, err)
			}
			f.tell(t, "_test/finish", nil)
			if !f.conn(t).waitIdle(protocolUnitWait) {
				t.Fatal("prompt never completed")
			}
			if n := len(f.replies(t)); n != 1 {
				t.Fatalf("agent received %d replies, want exactly 1", n)
			}
		})
	}
}

func TestPermissionRespondEmptyRequestIDTargetsOldest(t *testing.T) {
	f := startPermissionFake(t, nil, "7")
	f.prompt(t)
	if err := f.p.Respond(f.name, runtime.InteractionResponse{Action: "deny"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	assertOutcome(t, f.waitReplies(t, 1)[0], "7", selected("no"))
}

func TestPermissionRespondInvalidKeepsPending(t *testing.T) {
	onlyOnce := `[{"optionId":"yes","name":"Allow","kind":"allow_once"},{"optionId":"no","name":"Reject","kind":"reject_once"}]`
	f := startPermissionFake(t, map[string]string{"GC_PERM_OPTIONS": onlyOnce}, "7")
	pending := f.prompt(t)

	for _, action := range []string{"bogus", "approve_always", "deny_always", ""} {
		err := f.p.Respond(f.name, runtime.InteractionResponse{RequestID: pending.RequestID, Action: action})
		if !errors.Is(err, runtime.ErrInteractionResponseInvalid) {
			t.Fatalf("Respond(%q) = %v, want ErrInteractionResponseInvalid", action, err)
		}
	}
	err := f.p.Respond(f.name, runtime.InteractionResponse{RequestID: strings.TrimSuffix(pending.RequestID, "7") + "99", Action: "approve"})
	if !errors.Is(err, runtime.ErrInteractionResponseInvalid) {
		t.Fatalf("Respond(mismatched id) = %v, want ErrInteractionResponseInvalid", err)
	}

	still, err := f.p.Pending(f.name)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if still == nil || still.RequestID != pending.RequestID {
		t.Fatalf("Pending after invalid responses = %#v, want %q still pending", still, pending.RequestID)
	}
	if got := f.replies(t); len(got) != 0 {
		t.Fatalf("agent received replies %v for rejected responses", got)
	}
	if err := f.p.Respond(f.name, runtime.InteractionResponse{RequestID: pending.RequestID, Action: "approve"}); err != nil {
		t.Fatalf("Respond(approve): %v", err)
	}
	assertOutcome(t, f.waitReplies(t, 1)[0], "7", selected("yes"))
}

func TestPermissionFIFO(t *testing.T) {
	f := startPermissionFake(t, nil, `"perm-a"`, "2")
	first := f.prompt(t)
	assertRequestID(t, first.RequestID, `"perm-a"`)
	if err := f.p.Respond(f.name, runtime.InteractionResponse{RequestID: first.RequestID, Action: "approve"}); err != nil {
		t.Fatalf("Respond first: %v", err)
	}
	second := f.waitPending(t)
	assertRequestID(t, second.RequestID, "2")
	if second.Metadata["tool_call_id"] != "call_1" {
		t.Fatalf("second pending = %#v, want call_1", second)
	}
	if err := f.p.Respond(f.name, runtime.InteractionResponse{RequestID: second.RequestID, Action: "deny"}); err != nil {
		t.Fatalf("Respond second: %v", err)
	}
	replies := f.waitReplies(t, 2)
	assertOutcome(t, replies[0], `"perm-a"`, selected("yes"))
	assertOutcome(t, replies[1], "2", selected("no"))
}

func TestPermissionCancelledWhenPromptAnswered(t *testing.T) {
	f := startPermissionFake(t, nil, "7", "8")
	f.prompt(t)
	f.tell(t, "_test/finish", nil)
	if !f.conn(t).waitIdle(protocolUnitWait) {
		t.Fatal("prompt never completed")
	}
	pending, err := f.p.Pending(f.name)
	if err != nil || pending != nil {
		t.Fatalf("Pending after turn end = %#v, %v; want nil, nil", pending, err)
	}
	replies := f.waitReplies(t, 2)
	assertOutcome(t, replies[0], "7", outcomeCancelled)
	assertOutcome(t, replies[1], "8", outcomeCancelled)
}

func TestPermissionClearedOnProcessExit(t *testing.T) {
	f := startPermissionFake(t, nil, "7")
	f.prompt(t)
	sc := f.conn(t)
	f.tell(t, "_test/exit", nil)
	waitExited(t, sc)
	pending, err := f.p.Pending(f.name)
	if err != nil || pending != nil {
		t.Fatalf("Pending after exit = %#v, %v; want nil, nil", pending, err)
	}
}

// TestPermissionCancelRequestAnsweredCancelled pins ACP cancellation: a
// receiver that acts on $/cancel_request must still answer the original
// request, so gc forgets it and replies with the canceled outcome.
func TestPermissionCancelRequestAnsweredCancelled(t *testing.T) {
	f := startPermissionFake(t, nil, "7", `"p8"`)
	f.prompt(t)
	f.tell(t, "_test/cancel", map[string]any{"requestId": 7})
	assertOutcome(t, f.waitReplies(t, 1)[0], "7", outcomeCancelled)
	next, err := f.p.Pending(f.name)
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if next == nil {
		t.Fatal("canceling one request cleared the other")
	}
	assertRequestID(t, next.RequestID, `"p8"`)

	f.tell(t, "_test/cancel", map[string]any{"requestId": "p8"})
	replies := f.waitReplies(t, 2)
	assertOutcome(t, replies[1], `"p8"`, outcomeCancelled)
	f.waitNoPending(t)

	// Canceling an id gc does not hold is ignored, and ending the turn does
	// not answer the already-canceled requests a second time.
	f.tell(t, "_test/cancel", map[string]any{"requestId": 99})
	f.tell(t, "_test/finish", nil)
	if !f.conn(t).waitIdle(protocolUnitWait) {
		t.Fatal("prompt never completed")
	}
	sc := f.conn(t)
	f.tell(t, "_test/exit", nil)
	waitExited(t, sc)
	if got := f.replies(t); len(got) != 2 {
		t.Fatalf("agent received replies %v, want exactly the two canceled replies", got)
	}
}

// TestPermissionStaleRequestIDRejectedAfterRestart pins that RequestIDs are
// unique per connection: agents number requests from 1 on every connection,
// so an answer to the old incarnation's request must not apply to the new
// incarnation's request with the same JSON-RPC id.
func TestPermissionStaleRequestIDRejectedAfterRestart(t *testing.T) {
	f := startPermissionFake(t, nil, "1")
	stale := f.prompt(t)
	if err := f.p.Stop(f.name); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := f.p.Start(context.Background(), f.name, runtime.Config{
		Command: fakeACPPermissionCommand("1"),
		WorkDir: t.TempDir(),
		Env:     map[string]string{"GC_REPLY_FILE": f.replyFile, "GC_PERM_TITLE": "Run: rm -rf build"},
	}); err != nil {
		t.Fatalf("restart: %v", err)
	}
	fresh := f.prompt(t)
	assertRequestID(t, fresh.RequestID, "1")
	if fresh.RequestID == stale.RequestID {
		t.Fatalf("restarted agent reused RequestID %q", stale.RequestID)
	}

	err := f.p.Respond(f.name, runtime.InteractionResponse{RequestID: stale.RequestID, Action: "approve"})
	if !errors.Is(err, runtime.ErrInteractionResponseInvalid) {
		t.Fatalf("Respond(stale RequestID) = %v, want ErrInteractionResponseInvalid", err)
	}
	still, err := f.p.Pending(f.name)
	if err != nil || still == nil || still.RequestID != fresh.RequestID {
		t.Fatalf("Pending after stale answer = %#v, %v; want %q still pending", still, err, fresh.RequestID)
	}
	if got := f.replies(t); len(got) != 0 {
		t.Fatalf("agent received replies %v for a stale answer", got)
	}
}

// TestNudgeRefusedWhilePermissionPending pins that a nudge does not wait out
// the busy timeout behind a turn held open by a permission request: it
// refuses at once, so the session layer can report the pending interaction
// and the answer is not queued behind the wait.
func TestNudgeRefusedWhilePermissionPending(t *testing.T) {
	f := startPermissionFake(t, nil, "7")
	f.p.cfg.NudgeBusyTimeout = time.Hour
	f.prompt(t)

	start := time.Now()
	err := f.p.Nudge(f.name, runtime.TextContent("more"))
	if !errors.Is(err, runtime.ErrNudgeRefusedPendingInteraction) {
		t.Fatalf("Nudge while a permission is pending = %v, want ErrNudgeRefusedPendingInteraction", err)
	}
	if elapsed := time.Since(start); elapsed > protocolUnitWait {
		t.Fatalf("Nudge took %s to refuse", elapsed)
	}
	if pending, err := f.p.Pending(f.name); err != nil || pending == nil {
		t.Fatalf("Pending after refused Nudge = %#v, %v; want the permission still pending", pending, err)
	}
}

// TestNudgeWaitingForIdleRefusedWhenPermissionArrives pins the wake-up: a
// nudge already waiting for a busy turn refuses as soon as the agent asks
// for permission, instead of holding the caller until the busy timeout.
func TestNudgeWaitingForIdleRefusedWhenPermissionArrives(t *testing.T) {
	f := startPermissionFake(t, nil) // the prompt asks nothing up front
	f.p.cfg.NudgeBusyTimeout = time.Hour
	if err := f.p.Nudge(f.name, runtime.TextContent("start")); err != nil {
		t.Fatalf("first Nudge: %v", err)
	}
	sc := f.conn(t)
	if !sc.isBusy() {
		t.Fatal("turn not busy after the first prompt")
	}

	result := make(chan error, 1)
	go func() { result <- f.p.Nudge(f.name, runtime.TextContent("queued")) }()
	// The waiting Nudge holds nudgeMu for its whole wait.
	waitUntil(t, "the second Nudge to start waiting", func() bool {
		if sc.nudgeMu.TryLock() {
			sc.nudgeMu.Unlock()
			return false
		}
		return true
	})
	f.tell(t, "_test/ask", map[string]any{"id": 11})

	select {
	case err := <-result:
		if !errors.Is(err, runtime.ErrNudgeRefusedPendingInteraction) {
			t.Fatalf("waiting Nudge = %v, want ErrNudgeRefusedPendingInteraction", err)
		}
	case <-time.After(protocolUnitWait):
		t.Fatal("waiting Nudge did not refuse when the permission arrived")
	}
	pending := f.waitPending(t)
	assertRequestID(t, pending.RequestID, "11")
	if err := f.p.Respond(f.name, runtime.InteractionResponse{RequestID: pending.RequestID, Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	assertOutcome(t, f.waitReplies(t, 1)[0], "11", selected("yes"))
}

func TestInterruptCancelsOutstandingPermissions(t *testing.T) {
	f := startPermissionFake(t, nil, "7")
	f.prompt(t)
	if err := f.p.Interrupt(f.name); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	pending, err := f.p.Pending(f.name)
	if err != nil || pending != nil {
		t.Fatalf("Pending after Interrupt = %#v, %v; want nil, nil", pending, err)
	}
	assertOutcome(t, f.waitReplies(t, 1)[0], "7", outcomeCancelled)
}

// TestMalformedPermissionRequestAnsweredInvalidParams feeds the read loop a
// permission request whose params cannot be decoded; gc answers -32602 and
// holds nothing.
func TestMalformedPermissionRequestAnsweredInvalidParams(t *testing.T) {
	stdinR, stdinW := io.Pipe()
	t.Cleanup(func() { _ = stdinR.Close() })
	sc := newSessionConn(nil, stdinW, nil, 10, nil)
	go sc.readLoop(strings.NewReader(`{"jsonrpc":"2.0","id":5,"method":"session/request_permission","params":{"options":"nope"}}` + "\n"))

	line, err := bufio.NewReader(stdinR).ReadString('\n')
	if err != nil {
		t.Fatalf("reading reply: %v", err)
	}
	var reply struct {
		ID    json.RawMessage `json:"id"`
		Error JSONRPCError    `json:"error"`
	}
	if err := json.Unmarshal([]byte(line), &reply); err != nil {
		t.Fatalf("decoding %q: %v", line, err)
	}
	if string(reply.ID) != "5" || reply.Error.Code != -32602 {
		t.Fatalf("reply = %s, want id 5 with error -32602", line)
	}
	<-sc.readDone
	if got := sc.oldestPermission(); got != nil {
		t.Fatalf("malformed request held as pending: %#v", got)
	}
}
