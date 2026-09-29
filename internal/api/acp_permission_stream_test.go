package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

// acpPermissionStreamAgent is an inline python3 ACP agent. On session/prompt
// it asks permission for one tool call (JSON-RPC id 7); when gc replies, it
// appends the reply to $GC_REPLY_FILE and finishes the turn.
const acpPermissionStreamAgent = `exec python3 -u -c '
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

// TestACPPermissionThroughPendingRespondAndStream composes the HTTP API with
// the real ACP provider: an agent's session/request_permission shows up as
// a "pending" stream frame and on GET /pending, a POST /respond approves it,
// the agent receives the selected option, and the stream emits
// "pending_cleared".
func TestACPPermissionThroughPendingRespondAndStream(t *testing.T) {
	fs := newSessionFakeState(t)
	sp := acp.NewProviderWithDir(filepath.Join(testutil.ShortTempDir(t, "gc-acp-"), "acp"), acp.Config{
		HandshakeTimeout: 10 * time.Second,
		NudgeBusyTimeout: 10 * time.Second,
	})
	fs.sessionProvider = sp
	srv := New(fs)
	peekPoll := make(chan time.Time)
	srv.structuredPeekPoll = peekPoll
	h := newTestCityHandlerWith(t, fs, srv)

	mgr := session.NewManagerWithOptions(fs.cityBeadStore, sp)
	replyFile := filepath.Join(t.TempDir(), "replies.jsonl")
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{
		Template:  "helper",
		Command:   acpPermissionStreamAgent,
		WorkDir:   t.TempDir(),
		Provider:  "fake",
		Transport: "acp",
		Env:       map[string]string{"GC_REPLY_FILE": replyFile},
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Cleanup(func() { _ = sp.Stop(info.SessionName) })

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	streamReq := httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+info.ID+"/stream?format=structured", nil).WithContext(ctx)
	rec := newSyncResponseRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, streamReq)
		close(done)
	}()
	// pollUntil ticks the stream's peek poll until its body contains want.
	pollUntil := func(want string) string {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			body := rec.BodyString()
			if strings.Contains(body, want) {
				return body
			}
			if time.Now().After(deadline) {
				t.Fatalf("stream never contained %q; body:\n%s", want, body)
			}
			select {
			case peekPoll <- time.Now():
			case <-time.After(10 * time.Millisecond):
			}
		}
	}

	if err := mgr.Send(context.Background(), info.ID, "do it", "", runtime.Config{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	body := pollUntil("event: pending\n")
	if !strings.Contains(body, "Run: touch marker") {
		t.Fatalf("pending frame missing the tool call title; body:\n%s", body)
	}

	pendingRec := httptest.NewRecorder()
	h.ServeHTTP(pendingRec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+info.ID+"/pending", nil))
	if pendingRec.Code != http.StatusOK {
		t.Fatalf("pending status = %d; body: %s", pendingRec.Code, pendingRec.Body.String())
	}
	var pendingResp sessionPendingResponse
	if err := json.NewDecoder(pendingRec.Body).Decode(&pendingResp); err != nil {
		t.Fatalf("decode pending: %v", err)
	}
	if !pendingResp.Supported || pendingResp.Pending == nil || !strings.HasPrefix(pendingResp.Pending.RequestID, "acp-") {
		t.Fatalf("pending response = %#v, want an ACP approval", pendingResp)
	}
	requestID := pendingResp.Pending.RequestID

	staleReq := newPostRequest(cityURL(fs, "/session/")+info.ID+"/respond",
		strings.NewReader(`{"request_id":"acp-000000000000-7","action":"approve"}`))
	staleRec := httptest.NewRecorder()
	h.ServeHTTP(staleRec, staleReq)
	if staleRec.Code != http.StatusConflict {
		t.Fatalf("respond with a stale request_id status = %d, want 409; body: %s", staleRec.Code, staleRec.Body.String())
	}

	respondReq := newPostRequest(cityURL(fs, "/session/")+info.ID+"/respond",
		strings.NewReader(`{"request_id":`+jsonString(t, requestID)+`,"action":"approve"}`))
	respondRec := httptest.NewRecorder()
	h.ServeHTTP(respondRec, respondReq)
	if respondRec.Code != http.StatusAccepted {
		t.Fatalf("respond status = %d, want 202; body: %s", respondRec.Code, respondRec.Body.String())
	}

	body = pollUntil("event: pending_cleared")
	if !strings.Contains(body, `"request_id":`+jsonString(t, requestID)) {
		t.Fatalf("pending_cleared frame missing request_id %q; body:\n%s", requestID, body)
	}
	var reply []byte
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for reply, _ = os.ReadFile(replyFile); len(reply) == 0; reply, _ = os.ReadFile(replyFile) {
		select {
		case <-deadline.C:
			t.Fatal("agent never received the reply")
		case <-tick.C:
		}
	}
	if !strings.Contains(string(reply), `"outcome":{"outcome":"selected","optionId":"ok"}`) {
		t.Fatalf("agent received %s, want the allow_once option selected", reply)
	}
	cancel()
	<-done
}

func jsonString(t *testing.T, s string) string {
	t.Helper()
	data, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
