package main

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionacp "github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/testutil"
	"github.com/gastownhall/gascity/internal/worker"
)

// holdingACPAgent answers the ACP handshake and never answers a
// session/prompt, so the first prompt leaves the session busy.
const holdingACPAgent = `exec python3 -u -c '
import sys, json
for line in sys.stdin:
    msg = json.loads(line)
    method = msg.get("method", "")
    if method == "initialize":
        print(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": {"serverInfo": {"name": "holding"}}}), flush=True)
    elif method == "session/new":
        print(json.dumps({"jsonrpc": "2.0", "id": msg["id"], "result": {"sessionId": "held-1"}}), flush=True)
'`

// TestPollerSessionIdleEnoughUsesACPIdleWait pins the ACP behavior of the
// queued-nudge quiescence check when no activity stamp is observed: the ACP
// provider now answers WaitForIdle, so an idle session it owns accepts
// delivery, a session with an outstanding prompt does not, and a provider
// that does not own the connection reports not-idle.
func TestPollerSessionIdleEnoughUsesACPIdleWait(t *testing.T) {
	dir := filepath.Join(testutil.ShortTempDir(t, "gc-acp-"), "acp")
	sp := sessionacp.NewSeamBackedWithDir(dir, sessionacp.Config{HandshakeTimeout: 10 * time.Second})
	name := "gc-acp-poller-idle"
	if err := sp.Start(context.Background(), name, runtime.Config{Command: holdingACPAgent, WorkDir: t.TempDir()}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = sp.Stop(name) })
	target := nudgeTarget{sessionName: name}
	noActivity := worker.LiveObservation{}

	if !pollerSessionIdleEnough(target, sp, time.Second, noActivity) {
		t.Fatal("pollerSessionIdleEnough = false, want true for an idle ACP session")
	}

	other := sessionacp.NewSeamBackedWithDir(dir, sessionacp.Config{})
	if pollerSessionIdleEnough(target, other, time.Second, noActivity) {
		t.Fatal("pollerSessionIdleEnough = true through a provider that does not own the ACP connection")
	}

	if err := sp.Nudge(name, runtime.TextContent("hold")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if pollerSessionIdleEnough(target, sp, 20*time.Millisecond, noActivity) {
		t.Fatal("pollerSessionIdleEnough = true while an ACP prompt is outstanding")
	}
}
