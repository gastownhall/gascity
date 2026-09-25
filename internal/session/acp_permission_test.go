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
	if pending.RequestID != "acp-7" || pending.Kind != "approval" || pending.Prompt != "Run: touch marker" {
		t.Fatalf("Pending = %#v, want acp-7 approval for the tool call", pending)
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

// TestSendProceedsWhenPendingProbeCannotSeeRuntimeSession covers a Manager
// whose provider does not own the live connection (ACP reports
// ErrSessionNotFound from Pending then). The probe finds nothing pending, and
// delivery is left to the provider.
func TestSendProceedsWhenPendingProbeCannotSeeRuntimeSession(t *testing.T) {
	sp := &pendingSessionGoneProvider{Fake: runtime.NewFake()}
	mgr := NewManagerWithOptions(beads.NewMemStore(), sp)
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: "claude", WorkDir: "/tmp", Provider: "claude", ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Send(context.Background(), info.ID, "hello", "", runtime.Config{}); err != nil {
		t.Fatalf("Send = %v, want delivery when the pending probe cannot see the session", err)
	}
	nudged := false
	for _, call := range sp.Calls {
		if call.Method == "Nudge" && call.Name == info.SessionName {
			nudged = true
		}
	}
	if !nudged {
		t.Fatalf("Send did not nudge; calls = %#v", sp.Calls)
	}
}
