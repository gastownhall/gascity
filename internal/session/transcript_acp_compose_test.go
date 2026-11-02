package session

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/acp"
	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/testutil"
)

// composeFakeACPAgent is a minimal ACP agent: it answers the handshake and
// replies to each prompt with two message chunks and an end_turn response.
const composeFakeACPAgent = `exec python3 -u -c '
import sys, json
def send(msg):
    print(json.dumps(msg), flush=True)
for line in sys.stdin:
    try:
        msg = json.loads(line)
    except Exception:
        continue
    method, mid = msg.get("method", ""), msg.get("id")
    if method == "initialize":
        send({"jsonrpc": "2.0", "id": mid, "result": {"protocolVersion": 1}})
    elif method == "session/new":
        send({"jsonrpc": "2.0", "id": mid, "result": {"sessionId": "compose-1"}})
    elif method == "session/prompt":
        text = "".join(b.get("text", "") for b in msg["params"]["prompt"])
        for part in ("echo: ", text):
            send({"jsonrpc": "2.0", "method": "session/update", "params": {"sessionId": "compose-1",
                  "update": {"sessionUpdate": "agent_message_chunk", "messageId": "m-" + str(mid), "content": {"type": "text", "text": part}}}})
        send({"jsonrpc": "2.0", "id": mid, "result": {"stopReason": "end_turn"}})
'`

// TestACPCaptureComposesWithTranscriptDiscovery proves the three pieces line
// up: the ACP provider writes its capture where session discovery looks for
// it, and the sessionlog reader turns that capture into the conversation.
func TestACPCaptureComposesWithTranscriptDiscovery(t *testing.T) {
	city := t.TempDir()
	sp := acp.NewProviderWithDir(filepath.Join(testutil.ShortTempDir(t, "gc-t-"), "acp"), acp.Config{
		HandshakeTimeout:  5 * time.Second,
		NudgeBusyTimeout:  5 * time.Second,
		OutputBufferLines: 100,
		TranscriptRoot:    citylayout.ACPTranscriptsDir(city),
	})
	mgr := NewManagerWithOptions(beads.NewMemStore(), sp, WithCityPath(city))
	info, err := mgr.CreateSession(context.Background(), CreateOptions{
		Template:  "helper",
		Command:   composeFakeACPAgent,
		WorkDir:   t.TempDir(),
		Provider:  "fake-acp-agent",
		Transport: "acp",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	t.Cleanup(func() { _ = sp.Stop(info.SessionName) })

	if err := sp.Nudge(info.SessionName, runtime.TextContent("hello")); err != nil {
		t.Fatalf("first Nudge: %v", err)
	}
	// A nudge waits for the previous turn to finish before it prompts, so
	// once the second returns the first turn is complete in the capture.
	if err := sp.Nudge(info.SessionName, runtime.TextContent("again")); err != nil {
		t.Fatalf("second Nudge: %v", err)
	}
	// Stop flushes and closes the capture.
	if err := sp.Stop(info.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	path, lookup, err := mgr.TranscriptPathClassified(info.ID, []string{t.TempDir()})
	if err != nil {
		t.Fatalf("TranscriptPathClassified: %v", err)
	}
	want, err := citylayout.ACPTranscriptPath(city, info.ID, "1")
	if err != nil {
		t.Fatalf("ACPTranscriptPath: %v", err)
	}
	if path != want || lookup != TranscriptFound {
		t.Fatalf("TranscriptPathClassified = %q, %v; want the provider's capture %q", path, lookup, want)
	}

	sess, err := sessionlog.ReadProviderFile("fake-acp-agent", path, 0)
	if err != nil {
		t.Fatalf("ReadProviderFile: %v", err)
	}
	if len(sess.Messages) < 3 {
		t.Fatalf("got %d entries, want at least user, assistant, user", len(sess.Messages))
	}
	first, reply, second := sess.Messages[0], sess.Messages[1], sess.Messages[2]
	if first.Type != "user" || first.TextContent() != "hello" {
		t.Fatalf("entry 0 = %s %q, want the user prompt hello", first.Type, first.TextContent())
	}
	blocks := reply.ContentBlocks()
	if reply.Type != "assistant" || len(blocks) != 1 || blocks[0].Text != "echo: hello" {
		t.Fatalf("entry 1 = %s %+v, want one reassembled assistant reply", reply.Type, blocks)
	}
	if second.Type != "user" || second.TextContent() != "again" {
		t.Fatalf("entry 2 = %s %q, want the second prompt", second.Type, second.TextContent())
	}
	if sess.ID != "compose-1" {
		t.Fatalf("session id = %q, want the agent's ACP session id", sess.ID)
	}
}
