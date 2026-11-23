package t3bridge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

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
		if _, err := keyed.NudgeKeyed("agent-a", runtime.TextContent("hello"), key); err != nil {
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

	_, err := sp.(runtime.KeyedNudgeProvider).NudgeKeyed("agent-a", runtime.TextContent("hello"), "batch-1")
	if !errors.Is(err, runtime.ErrNudgeRejected) {
		t.Fatalf("NudgeKeyed with a rejected turn start = %v, want errors.Is(runtime.ErrNudgeRejected)", err)
	}
}

// Upstream T3 delivers an engine refusal rewrapped as
// OrchestrationDispatchCommandError; it is still a definitive rejection.
func TestNudgeKeyedUpstreamWrappedRejectionIsErrNudgeRejected(t *testing.T) {
	server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
	t.Cleanup(server.Close)
	server.failCommand("thread.turn.start", map[string]interface{}{
		"_tag": "Fail",
		"error": map[string]interface{}{
			"_tag":    "OrchestrationDispatchCommandError",
			"message": "Orchestration command invariant failed (thread.turn.start): Thread 'thread-1' does not exist.",
			"cause":   map[string]interface{}{"name": "Error", "message": "Orchestration command invariant failed (thread.turn.start): Thread 'thread-1' does not exist."},
		},
	})
	_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())

	_, err := sp.(runtime.KeyedNudgeProvider).NudgeKeyed("agent-a", runtime.TextContent("hello"), "batch-1")
	if !errors.Is(err, runtime.ErrNudgeRejected) {
		t.Fatalf("NudgeKeyed with an upstream-wrapped rejection = %v, want errors.Is(runtime.ErrNudgeRejected)", err)
	}
}

// A transport failure leaves the outcome unknown, so it is never reported as
// a rejection: the caller must redeliver under the same key.
func TestTransportFailureIsNotARejection(t *testing.T) {
	if isT3Rejection(errors.New("ws://127.0.0.1:1/ws: read: connection reset by peer")) {
		t.Fatal("a transport failure classified as a T3 rejection")
	}
}

// Only a Fail carrying one of T3's command errors is a definitive refusal.
// An interrupted or dying handler may have committed the command, so those
// causes, unknown errors and unparseable causes are ambiguous: the caller
// redelivers under the same key and T3 replays whatever it decided. Both
// Effect cause encodings (the v3 tagged tree and the flattened list) are read.
func TestT3ExitCauseIsRejection(t *testing.T) {
	fail := func(tag string) map[string]interface{} {
		return map[string]interface{}{"_tag": "Fail", "error": map[string]interface{}{"_tag": tag, "message": "no"}}
	}
	invariant := fail("OrchestrationCommandInvariantError")
	previously := fail("OrchestrationCommandPreviouslyRejectedError")
	die := map[string]interface{}{"_tag": "Die", "defect": "boom"}
	interrupt := map[string]interface{}{"_tag": "Interrupt", "fiberId": map[string]interface{}{"id": 3}}
	// Upstream T3's WS dispatchCommand rewraps every engine error as
	// OrchestrationDispatchCommandError{message, cause}; the message is the
	// engine error's own.
	wrapped := func(message string, cause interface{}) map[string]interface{} {
		failure := map[string]interface{}{"_tag": "OrchestrationDispatchCommandError", "message": message}
		if cause != nil {
			failure["cause"] = cause
		}
		return map[string]interface{}{"_tag": "Fail", "error": failure}
	}
	for _, tc := range []struct {
		name  string
		cause interface{}
		want  bool
	}{
		{name: "fail invariant", cause: invariant, want: true},
		{name: "fail previously rejected", cause: previously, want: true},
		{name: "fail unknown error", cause: fail("SqlError"), want: false},
		{name: "fail without error tag", cause: map[string]interface{}{"_tag": "Fail", "error": "nope"}, want: false},
		{name: "die", cause: die, want: false},
		{name: "interrupt", cause: interrupt, want: false},
		{name: "empty", cause: map[string]interface{}{"_tag": "Empty"}, want: false},
		{name: "sequential fails", cause: map[string]interface{}{"_tag": "Sequential", "left": invariant, "right": previously}, want: true},
		{name: "parallel fail and die", cause: map[string]interface{}{"_tag": "Parallel", "left": invariant, "right": die}, want: false},
		{name: "flattened failures", cause: map[string]interface{}{"failures": []interface{}{invariant}}, want: true},
		{name: "flattened reasons with interrupt", cause: map[string]interface{}{"reasons": []interface{}{invariant, interrupt}}, want: false},
		{name: "bare list", cause: []interface{}{invariant}, want: true},
		{name: "empty list", cause: []interface{}{}, want: false},
		{name: "string", cause: "thread is archived", want: false},
		{name: "wrapped invariant", cause: wrapped("Orchestration command invariant failed (thread.turn.start): Thread 't' does not exist", map[string]interface{}{"name": "Error", "message": "x"}), want: true},
		{name: "wrapped previously rejected", cause: wrapped("Command previously rejected (cmd-1): Thread 't' does not exist", nil), want: true},
		{name: "wrapped with a tagged engine cause", cause: wrapped("Failed to dispatch orchestration command", map[string]interface{}{"_tag": "OrchestrationCommandInvariantError", "detail": "no"}), want: true},
		{name: "wrapped storage failure", cause: wrapped("Failed to dispatch orchestration command", map[string]interface{}{"_tag": "PersistenceSqlError"}), want: false},
		{name: "wrapped id conflict", cause: wrapped("Command id 'c' already used for thread 'a'; refusing to replay its receipt for thread 'b'.", nil), want: false},
		{name: "wrapped without a message", cause: map[string]interface{}{"_tag": "Fail", "error": map[string]interface{}{"_tag": "OrchestrationDispatchCommandError"}}, want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.cause)
			if err != nil {
				t.Fatal(err)
			}
			if got := t3ExitCauseIsRejection(raw); got != tc.want {
				t.Fatalf("t3ExitCauseIsRejection(%s) = %v, want %v", raw, got, tc.want)
			}
		})
	}
}

// A turn start whose handler died or was interrupted may still commit, so it
// is not a rejection: redelivering under a new key could start a second turn.
// Nor is it unavailable, which runtime.TurnFenceProvider reserves for a send
// that sent nothing: deliver-now would queue the message a second time.
func TestNudgeKeyedAmbiguousFailureIsNotErrNudgeRejected(t *testing.T) {
	for name, cause := range map[string]interface{}{
		"die":       map[string]interface{}{"_tag": "Die", "defect": "boom"},
		"interrupt": map[string]interface{}{"_tag": "Interrupt", "fiberId": 7},
	} {
		t.Run(name, func(t *testing.T) {
			server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
			t.Cleanup(server.Close)
			server.failCommand("thread.turn.start", cause)
			_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())

			_, err := sp.(runtime.KeyedNudgeProvider).NudgeKeyed("agent-a", runtime.TextContent("hello"), "batch-1")
			if err == nil {
				t.Fatal("NudgeKeyed with a failed turn start = nil, want the failure")
			}
			if errors.Is(err, runtime.ErrNudgeRejected) {
				t.Fatalf("NudgeKeyed with a %s cause = %v, want an ambiguous (non-rejection) error", name, err)
			}
			if errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("NudgeKeyed with a %s cause = %v, want an error that does not claim nothing was sent", name, err)
			}
		})
	}
}

// sentTurnStart is the one turn start server saw: its message id and the
// createdAt it carried, parsed.
func sentTurnStart(t *testing.T, server *t3BridgeTestServer) (string, time.Time, string) {
	t.Helper()
	payloads := server.commandPayloads("thread.turn.start")
	if len(payloads) != 1 {
		t.Fatalf("turn starts = %+v, want one", payloads)
	}
	message, _ := payloads[0]["message"].(map[string]interface{})
	messageID, _ := message["messageId"].(string)
	created, _ := payloads[0]["createdAt"].(string)
	at, err := time.Parse(t3TimeLayout, created)
	if err != nil {
		t.Fatalf("createdAt %q: %v", created, err)
	}
	return messageID, at, created
}

// Kills a receipt naming the command id (M9) or a second time.Now() (M10):
// it is the wire message id and exactly the createdAt the send carried.
func TestNudgeKeyedReceiptIsTheSentMessage(t *testing.T) {
	server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
	t.Cleanup(server.Close)
	_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
	r, err := sp.(runtime.KeyedNudgeProvider).NudgeKeyed("agent-a", runtime.TextContent("hello"), "gc:op-1:1")
	if err != nil {
		t.Fatalf("NudgeKeyed: %v", err)
	}
	messageID, at, _ := sentTurnStart(t, server)
	if r.MessageID != messageID || r.MessageID != deriveDeliveryIDs("city-a", "thread-agent-a", "gc:op-1:1").messageID {
		t.Fatalf("receipt message id = %q, want the sent message id %q", r.MessageID, messageID)
	}
	if !r.MessageAt.Equal(at) || r.TurnID != "" {
		t.Fatalf("receipt = %+v, want MessageAt %v exactly and no turn", r, at)
	}
	if strings.Contains(r.MessageID, "op-1") {
		t.Fatalf("receipt message id %q carries the key", r.MessageID)
	}
}

// Kills a fenced path that ignores the receipt slot (M11); without a slot
// the wire is the same and nothing panics.
func TestNudgeFencedFillsTheReceiptSlot(t *testing.T) {
	server := newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(t3Fence)))
	p := newTurnFenceTestProvider(t, server)
	var r runtime.DeliveryReceipt
	if err := p.NudgeFenced(t.Context(), turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence, DeliveryKey: "k1", Receipt: &r}); err != nil {
		t.Fatalf("NudgeFenced: %v", err)
	}
	messageID, at, _ := sentTurnStart(t, server)
	if r.MessageID != messageID || !r.MessageAt.Equal(at) {
		t.Fatalf("receipt = %+v, want message %q at %v", r, messageID, at)
	}
	if err := p.NudgeFenced(t.Context(), turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence, DeliveryKey: "k1"}); err != nil {
		t.Fatalf("NudgeFenced without a slot: %v", err)
	}
	if ids := keyedNudgeIDs(t, server); len(ids) != 2 || ids[0] != ids[1] {
		t.Fatalf("turn ids = %v, want the same ids twice", ids)
	}
}

// Kills a receipt reported with an error (M12): a rejection, an unconfirmed
// send and an outage all report none.
func TestNudgeKeyedErrorsReportNoReceipt(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*t3BridgeTestServer)
		url   bool
		want  error
	}{
		{name: "rejected", setup: func(s *t3BridgeTestServer) { s.rejectCommand("thread.turn.start", "archived") }, want: runtime.ErrNudgeRejected},
		{name: "ambiguous", setup: func(s *t3BridgeTestServer) {
			s.failCommand("thread.turn.start", map[string]interface{}{"_tag": "Die", "defect": "boom"})
		}},
		{name: "unavailable", url: true, want: runtime.ErrRuntimeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			url := unreachableBridgeURL
			if !tc.url {
				server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
				t.Cleanup(server.Close)
				tc.setup(server)
				url = server.wsURL()
			}
			_, sp := newSeamBackedSnapshotTestProvider(t, url)
			r, err := sp.(runtime.KeyedNudgeProvider).NudgeKeyed("agent-a", runtime.TextContent("hello"), "k")
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || !r.IsZero() {
				t.Fatalf("NudgeKeyed = %+v, %v; want no receipt and an error (%v)", r, err, tc.want)
			}
		})
	}
	t.Run("unconfirmed fenced", func(t *testing.T) {
		next := runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok-next"}
		server := newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(t3Fence)))
		p := newTurnFenceTestProvider(t, server)
		server.dropNextCommand()
		server.setCommandHook(func(map[string]interface{}) {
			server.setSnapshots(fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(next)))
		})
		var r runtime.DeliveryReceipt
		err := p.NudgeFenced(t.Context(), turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence, DeliveryKey: "k1", Receipt: &r})
		if !errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) || !r.IsZero() {
			t.Fatalf("NudgeFenced = %v with receipt %+v; want unconfirmed and no receipt", err, r)
		}
	})
}

// Pins the basis of DES-O1 §4.8 row 3 on T3: the receipt's MessageAt is the
// started turn's requestedAt as TurnHistory reads it.
func TestNudgeKeyedReceiptMatchesTheTurnsRequestedAt(t *testing.T) {
	server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
	t.Cleanup(server.Close)
	server.setCommandHook(func(payload map[string]interface{}) {
		created, _ := payload["createdAt"].(string)
		snapshot := boundThreadSnapshot("running")
		thread := snapshot["threads"].([]interface{})[0].(map[string]interface{})
		thread["session"] = map[string]interface{}{"status": "running", "activeTurnId": "t1"}
		thread["latestTurn"] = latestTurn("t1", "running", created, "")
		server.setSnapshots(snapshot)
	})
	backed, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
	r, err := sp.(runtime.KeyedNudgeProvider).NudgeKeyed("agent-a", runtime.TextContent("hello"), "k")
	if err != nil {
		t.Fatal(err)
	}
	h, err := backed.raw.TurnHistory(t.Context(), "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if !h.CurrentStart.Equal(r.MessageAt) || r.MessageAt.IsZero() {
		t.Fatalf("CurrentStart = %v, receipt MessageAt = %v; want equal", h.CurrentStart, r.MessageAt)
	}
}
