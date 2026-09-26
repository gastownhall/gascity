package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/testutil"
)

// acpPermissionAgent is an inline python3 ACP agent. On session/prompt it
// asks permission for one tool call (JSON-RPC id 7); when gc replies, it
// appends the reply to $GC_REPLY_FILE and finishes the turn.
const acpPermissionAgent = `exec python3 -u -c '
import sys, json, os

def send(msg):
    print(json.dumps(msg), flush=True)

prompt_id = None
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    method = msg.get("method", "")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {}})
    elif method == "session/new":
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {"sessionId": "s1"}})
    elif method == "session/prompt":
        prompt_id = msg["id"]
        send({"jsonrpc": "2.0", "id": 7, "method": "session/request_permission", "params": {
            "sessionId": "s1",
            "toolCall": {"toolCallId": "call_1", "title": "Run: touch marker", "kind": "execute"},
            "options": [{"optionId": "ok", "name": "Allow", "kind": "allow_once"},
                        {"optionId": "no", "name": "Reject", "kind": "reject_once"}]}})
    elif method == "" and msg.get("id") == 7:
        with open(os.environ["GC_REPLY_FILE"], "a") as f:
            f.write(line + "\n")
        send({"jsonrpc": "2.0", "id": prompt_id, "result": {"stopReason": "end_turn"}})
'`

// acpPermissionWait bounds fact-based waits; the fake answers in
// milliseconds, so the bound only turns a hang into a failure.
const acpPermissionWait = 10 * time.Second

func waitForACPFact(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.NewTimer(acpPermissionWait)
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

// TestACPPermissionRoundTripThroughManager composes the session Manager with
// the real ACP provider: an agent's permission request blocks new submits,
// surfaces through Pending, rejects an invalid answer as a mismatch, and is
// resolved by Respond.
func TestACPPermissionRoundTripThroughManager(t *testing.T) {
	sp := acp.NewProviderWithDir(filepath.Join(testutil.ShortTempDir(t, "gc-acp-"), "acp"), acp.Config{
		HandshakeTimeout: acpPermissionWait,
		NudgeBusyTimeout: acpPermissionWait,
	})
	mgr := NewManagerWithOptions(beads.NewMemStore(), sp)
	replyFile := filepath.Join(t.TempDir(), "replies.jsonl")
	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template:  "helper",
		Command:   acpPermissionAgent,
		WorkDir:   t.TempDir(),
		Provider:  "fake",
		Transport: "acp",
		Env:       map[string]string{"GC_REPLY_FILE": replyFile},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Cleanup(func() { _ = sp.Stop(info.SessionName) })

	pending, supported, err := mgr.Pending(info.ID)
	if err != nil || !supported || pending != nil {
		t.Fatalf("Pending before prompt = %#v, %v, %v; want nil, true, nil", pending, supported, err)
	}

	if err := mgr.Send(context.Background(), info.ID, "do it", "", runtime.Config{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	waitForACPFact(t, "the permission request", func() bool {
		pending, _, err = mgr.Pending(info.ID)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		return pending != nil
	})
	if !strings.HasPrefix(pending.RequestID, "acp-") || !strings.HasSuffix(pending.RequestID, "-7") ||
		pending.Kind != "approval" || pending.Prompt != "Run: touch marker" {
		t.Fatalf("Pending = %#v, want an acp-<nonce>-7 approval for the tool call", pending)
	}

	if err := mgr.Send(context.Background(), info.ID, "another", "", runtime.Config{}); !errors.Is(err, ErrPendingInteraction) {
		t.Fatalf("Send while pending = %v, want ErrPendingInteraction", err)
	}
	err = mgr.Respond(info.ID, runtime.InteractionResponse{RequestID: pending.RequestID, Action: "approve_always"})
	if !errors.Is(err, ErrInteractionMismatch) {
		t.Fatalf("Respond(approve_always, not offered) = %v, want ErrInteractionMismatch", err)
	}
	if still, _, _ := mgr.Pending(info.ID); still == nil {
		t.Fatal("invalid response cleared the pending interaction")
	}

	if err := mgr.Respond(info.ID, runtime.InteractionResponse{Action: "approve"}); err != nil {
		t.Fatalf("Respond(approve): %v", err)
	}
	var reply []byte
	waitForACPFact(t, "the agent to receive the reply", func() bool {
		reply, _ = os.ReadFile(replyFile)
		return len(reply) > 0
	})
	if !strings.Contains(string(reply), `"outcome":{"outcome":"selected","optionId":"ok"}`) {
		t.Fatalf("agent received %s, want the allow_once option selected", reply)
	}
	pending, _, err = mgr.Pending(info.ID)
	if err != nil || pending != nil {
		t.Fatalf("Pending after Respond = %#v, %v; want nil, nil", pending, err)
	}
	if err := mgr.Respond(info.ID, runtime.InteractionResponse{Action: "approve"}); !errors.Is(err, ErrNoPendingInteraction) {
		t.Fatalf("second Respond = %v, want ErrNoPendingInteraction", err)
	}
}

// acpLatePermissionAgent is an inline python3 ACP agent whose first prompt
// asks permission (JSON-RPC id 7) only once $GC_ASK_FILE exists, so a test
// can queue a submit behind the busy turn first. When gc replies it appends
// the reply to $GC_REPLY_FILE and finishes the turn; later prompts finish at
// once.
const acpLatePermissionAgent = `exec python3 -u -c '
import sys, json, os, threading, time

lock = threading.Lock()
def send(msg):
    with lock:
        print(json.dumps(msg), flush=True)

def ask_when_told():
    while not os.path.exists(os.environ["GC_ASK_FILE"]):
        time.sleep(0.005)
    send({"jsonrpc": "2.0", "id": 7, "method": "session/request_permission", "params": {
        "sessionId": "s1",
        "toolCall": {"toolCallId": "call_1", "title": "Run: touch marker", "kind": "execute"},
        "options": [{"optionId": "ok", "name": "Allow", "kind": "allow_once"}]}})

first_prompt = None
for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    method = msg.get("method", "")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {}})
    elif method == "session/new":
        send({"jsonrpc": "2.0", "id": msg["id"], "result": {"sessionId": "s1"}})
    elif method == "session/prompt":
        if first_prompt is None:
            first_prompt = msg["id"]
            threading.Thread(target=ask_when_told, daemon=True).start()
        else:
            send({"jsonrpc": "2.0", "id": msg["id"], "result": {"stopReason": "end_turn"}})
    elif method == "" and msg.get("id") == 7:
        with open(os.environ["GC_REPLY_FILE"], "a") as f:
            f.write(line + "\n")
        send({"jsonrpc": "2.0", "id": first_prompt, "result": {"stopReason": "end_turn"}})
'`

// acpStallBound is how quickly a refused submit and a Respond must return.
// The provider's busy timeout is far longer, so a Respond stuck behind a
// waiting submit, or a submit waiting out the timeout, fails the bound.
const acpStallBound = 5 * time.Second

type acpLatePermissionSession struct {
	mgr       *Manager
	id        string
	askFile   string
	replyFile string
}

func startACPLatePermissionSession(t *testing.T) *acpLatePermissionSession {
	t.Helper()
	sp := acp.NewProviderWithDir(filepath.Join(testutil.ShortTempDir(t, "gc-acp-"), "acp"), acp.Config{
		HandshakeTimeout: acpPermissionWait,
		NudgeBusyTimeout: time.Hour,
	})
	s := &acpLatePermissionSession{
		mgr:       NewManagerWithOptions(beads.NewMemStore(), sp),
		askFile:   filepath.Join(t.TempDir(), "ask"),
		replyFile: filepath.Join(t.TempDir(), "replies.jsonl"),
	}
	info, err := s.mgr.CreateSession(context.Background(), CreateOptions{
		Template:  "helper",
		Command:   acpLatePermissionAgent,
		WorkDir:   t.TempDir(),
		Provider:  "fake",
		Transport: "acp",
		Env:       map[string]string{"GC_ASK_FILE": s.askFile, "GC_REPLY_FILE": s.replyFile},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Cleanup(func() { _ = sp.Stop(info.SessionName) })
	s.id = info.ID
	// The first prompt keeps the turn busy until the agent is told to ask.
	if err := s.mgr.Send(context.Background(), s.id, "start", "", runtime.Config{}); err != nil {
		t.Fatalf("first Send: %v", err)
	}
	return s
}

func (s *acpLatePermissionSession) ask(t *testing.T) {
	t.Helper()
	if err := os.WriteFile(s.askFile, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (s *acpLatePermissionSession) waitPending(t *testing.T) *runtime.PendingInteraction {
	t.Helper()
	var pending *runtime.PendingInteraction
	waitForACPFact(t, "the permission request", func() bool {
		var err error
		pending, _, err = s.mgr.Pending(s.id)
		if err != nil {
			t.Fatalf("Pending: %v", err)
		}
		return pending != nil
	})
	return pending
}

// respondQuickly answers the permission and checks the answer did not wait
// behind another session mutation.
func (s *acpLatePermissionSession) respondQuickly(t *testing.T, pending *runtime.PendingInteraction) {
	t.Helper()
	start := time.Now()
	if err := s.mgr.Respond(s.id, runtime.InteractionResponse{RequestID: pending.RequestID, Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if elapsed := time.Since(start); elapsed > acpStallBound {
		t.Fatalf("Respond took %s, want well under the busy timeout", elapsed)
	}
	waitForACPFact(t, "the agent to receive the reply", func() bool {
		reply, _ := os.ReadFile(s.replyFile)
		return len(reply) > 0
	})
}

// sessionMutationLockHeld reports whether some caller holds id's session
// mutation lock.
func sessionMutationLockHeld(id string) bool {
	sessionMutationLocksMu.Lock()
	lock := sessionMutationLocks[id]
	sessionMutationLocksMu.Unlock()
	if lock == nil {
		return false
	}
	if lock.mu.TryLock() {
		lock.mu.Unlock()
		return false
	}
	return true
}

// TestACPSubmitWaitingForIdleRefusedWhenPermissionArrives composes the
// Manager with the real ACP provider for the sequence where a submit is
// already waiting on a busy turn (holding the session mutation lock) when
// the agent asks permission. The submit must refuse promptly with
// ErrPendingInteraction so Respond, which needs the same lock, is not stuck
// behind it until the busy timeout.
func TestACPSubmitWaitingForIdleRefusedWhenPermissionArrives(t *testing.T) {
	s := startACPLatePermissionSession(t)

	result := make(chan error, 1)
	start := time.Now()
	go func() { result <- s.mgr.Send(context.Background(), s.id, "queued", "", runtime.Config{}) }()
	waitForACPFact(t, "the second submit to hold the session lock", func() bool {
		return sessionMutationLockHeld(s.id)
	})
	s.ask(t)

	select {
	case err := <-result:
		if !errors.Is(err, ErrPendingInteraction) {
			t.Fatalf("waiting Send = %v, want ErrPendingInteraction", err)
		}
	case <-time.After(acpStallBound):
		t.Fatalf("waiting Send still blocked %s after the permission arrived", time.Since(start))
	}
	s.respondQuickly(t, s.waitPending(t))
}

// TestACPLiveOnlyNudgeRefusedWhilePermissionPending covers the controller's
// queued-nudge path (SendLiveOnly), which has no pending pre-check of its
// own: with a permission already outstanding it must refuse promptly with
// ErrPendingInteraction, leaving Respond free.
func TestACPLiveOnlyNudgeRefusedWhilePermissionPending(t *testing.T) {
	s := startACPLatePermissionSession(t)
	s.ask(t)
	pending := s.waitPending(t)

	start := time.Now()
	delivered, err := s.mgr.SendLiveOnly(context.Background(), s.id, "queued nudge")
	if !errors.Is(err, ErrPendingInteraction) || delivered {
		t.Fatalf("SendLiveOnly = %v, %v; want not delivered with ErrPendingInteraction", delivered, err)
	}
	if elapsed := time.Since(start); elapsed > acpStallBound {
		t.Fatalf("SendLiveOnly took %s to refuse", elapsed)
	}
	s.respondQuickly(t, pending)
}
