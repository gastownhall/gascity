package t3bridge

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func keyedNudgeIDs(t *testing.T, server *t3BridgeTestServer) [][2]string {
	t.Helper()
	var ids [][2]string
	for _, payload := range server.commandPayloads("thread.turn.start") {
		commandID, _ := payload["commandId"].(string)
		message, _ := payload["message"].(map[string]interface{})
		messageID, _ := message["messageId"].(string)
		ids = append(ids, [2]string{commandID, messageID})
	}
	return ids
}

// A redelivery of the same batch sends T3 the same command and message ids,
// so T3 replays its receipt instead of starting a second turn; another batch
// gets its own ids.
func TestNudgeKeyedDerivesStableT3IDs(t *testing.T) {
	server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
	t.Cleanup(server.Close)
	backed, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
	keyed, ok := sp.(runtime.KeyedNudgeProvider)
	if !ok {
		t.Fatalf("%T is not a runtime.KeyedNudgeProvider", sp)
	}

	for _, key := range []string{"batch-1", "batch-1", "batch-2"} {
		if err := keyed.NudgeKeyed("agent-a", runtime.TextContent("hello"), key); err != nil {
			t.Fatalf("NudgeKeyed(%s): %v", key, err)
		}
	}
	// An unkeyed nudge (the API path) keeps minting fresh ids.
	if err := backed.raw.Nudge("agent-a", runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}

	ids := keyedNudgeIDs(t, server)
	if len(ids) != 4 {
		t.Fatalf("turn starts = %d, want 4", len(ids))
	}
	if ids[0] != ids[1] {
		t.Fatalf("redelivery ids = %v then %v, want identical", ids[0], ids[1])
	}
	for i, other := range ids[2:] {
		if other[0] == ids[0][0] || other[1] == ids[0][1] {
			t.Fatalf("delivery %d ids = %v share an id with batch-1's %v", i+2, other, ids[0])
		}
	}
	commandID, messageID := ids[0][0], ids[0][1]
	if !strings.Contains(commandID, "thread.turn.start") {
		t.Fatalf("commandId %q does not name the command type", commandID)
	}
	if commandID == messageID {
		t.Fatalf("commandId and messageId are both %q, want distinct ids", commandID)
	}
	for _, id := range []string{commandID, messageID} {
		if !strings.Contains(id, "city-a") {
			t.Fatalf("id %q is not namespaced by the city", id)
		}
	}
}

// The same delivery key in another city, or on another thread, is another
// delivery: T3's ids are global, so the derived ids must differ.
func TestDeliveryIDsAreNamespaced(t *testing.T) {
	base := deriveDeliveryIDs("city-a", "thread-1", "batch-1")
	for _, other := range []turnStartIDs{
		deriveDeliveryIDs("city-b", "thread-1", "batch-1"),
		deriveDeliveryIDs("city-a", "thread-2", "batch-1"),
	} {
		if other.commandID == base.commandID || other.messageID == base.messageID {
			t.Fatalf("ids %+v collide with %+v", other, base)
		}
	}
	if got := deriveDeliveryIDs("city-a", "thread-1", "batch-1"); got != base {
		t.Fatalf("derived ids = %+v, then %+v; want deterministic", base, got)
	}
}

// T3's answer of Failure is a definitive negative: report it as a rejected
// nudge, so the caller redelivers under a new key.
func TestNudgeKeyedRejectionIsErrNudgeRejected(t *testing.T) {
	server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
	t.Cleanup(server.Close)
	server.rejectCommand("thread.turn.start", "thread is archived")
	_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())

	err := sp.(runtime.KeyedNudgeProvider).NudgeKeyed("agent-a", runtime.TextContent("hello"), "batch-1")
	if !errors.Is(err, runtime.ErrNudgeRejected) {
		t.Fatalf("NudgeKeyed with a rejected turn start = %v, want errors.Is(runtime.ErrNudgeRejected)", err)
	}
}

// A transport failure leaves the outcome unknown, so it is never reported as
// a rejection: the caller must redeliver under the same key.
func TestTransportFailureIsNotARejection(t *testing.T) {
	if isT3Rejection(errors.New("ws://127.0.0.1:1/ws: read: connection reset by peer")) {
		t.Fatal("a transport failure classified as a T3 rejection")
	}
	if !isT3Rejection(&t3RPCRejectedError{method: "orchestration.dispatchCommand", cause: "nope"}) {
		t.Fatal("a T3 Failure exit not classified as a rejection")
	}
}
