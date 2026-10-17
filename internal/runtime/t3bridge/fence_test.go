package t3bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

var t3Fence = runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok-a"}

// fencedSnapshot holds turnFenceSession's thread threadID, whose recorded
// session env names env's incarnation (none when env is nil), with a ready
// session (or status).
func fencedSnapshot(t *testing.T, threadID, status string, env map[string]string) map[string]interface{} {
	t.Helper()
	meta := map[string]interface{}{"gc.sessionName": turnFenceSession}
	if env != nil {
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatal(err)
		}
		meta["gc.sessionEnv"] = string(raw)
	}
	return map[string]interface{}{"threads": []interface{}{map[string]interface{}{
		"id":             threadID,
		"projectId":      "project-1",
		"customMetadata": meta,
		"session":        map[string]interface{}{"status": status, "activeTurnId": nil},
	}}}
}

func t3FenceEnv(f runtime.InputFence) map[string]string {
	return map[string]string{
		runtime.RuntimeEpochEnv:      "2",
		runtime.ContinuationEpochEnv: "3",
		runtime.InstanceTokenEnv:     f.Token,
		"GC_SESSION_NAME":            turnFenceSession,
	}
}

// Kills a t3bridge fence that does not compare with the thread's binding: a
// fenced nudge to the thread recording the fence's incarnation starts one
// turn there (with the delivery key's ids); every op under another fence, to
// a thread that records no incarnation or to a session with no thread is a
// miss that dispatches nothing.
//
// It runs on the raw bridge and on the seam-backed provider production
// builds, so a seam that forwards a fenced op to its unfenced twin fails.
func TestT3FencedInputComparesTheThreadBinding(t *testing.T) {
	for label, newProvider := range map[string]func(*testing.T, *t3BridgeTestServer) runtime.FencedInputProvider{
		"raw": func(t *testing.T, server *t3BridgeTestServer) runtime.FencedInputProvider {
			return newTurnFenceTestProvider(t, server)
		},
		"seam-backed": func(t *testing.T, server *t3BridgeTestServer) runtime.FencedInputProvider {
			_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
			fp, ok := sp.(runtime.FencedInputProvider)
			if !ok {
				t.Fatalf("NewSeamBacked() = %T, want a fenced input provider", sp)
			}
			return fp
		},
	} {
		t.Run(label, func(t *testing.T) {
			server := newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(t3Fence)))
			testT3FencedInputComparesTheThreadBinding(t, server, newProvider(t, server))
		})
	}
}

func testT3FencedInputComparesTheThreadBinding(t *testing.T, server *t3BridgeTestServer, p runtime.FencedInputProvider) {
	if got := p.InputFencing(turnFenceSession); got != runtime.InputFencingChecked {
		t.Fatalf("fencing = %q, want checked", got)
	}
	ctx := t.Context()
	if err := p.NudgeFenced(ctx, turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence, DeliveryKey: "k1"}); err != nil {
		t.Fatalf("NudgeFenced under the thread's fence: %v", err)
	}
	starts := server.commandPayloads("thread.turn.start")
	if len(starts) != 1 || starts[0]["threadId"] != "thread-1" {
		t.Fatalf("turn starts = %+v, want one on thread-1", starts)
	}
	want := deriveDeliveryIDs("", "thread-1", "k1")
	if got := keyedNudgeIDs(t, server); len(got) != 1 || got[0] != [2]string{want.commandID, want.messageID} {
		t.Fatalf("turn ids = %v, want the delivery key's", got)
	}
	if err := p.SendKeysFenced(ctx, turnFenceSession, t3Fence, "Enter"); err != nil {
		t.Fatalf("SendKeysFenced: %v", err)
	}
	if err := p.InterruptFenced(ctx, turnFenceSession, t3Fence); err != nil {
		t.Fatalf("InterruptFenced: %v", err)
	}
	if got := server.commandPayloads("thread.turn.interrupt"); len(got) != 1 || got[0]["threadId"] != "thread-1" {
		t.Fatalf("interrupts = %+v, want one on thread-1", got)
	}

	other := runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok-b"}
	for label, tc := range map[string]struct {
		snapshot map[string]interface{}
		fence    runtime.InputFence
		found    string
	}{
		"other fence": {fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(t3Fence)), other, t3Fence.Digest()},
		"no identity": {fencedSnapshot(t, "thread-1", "ready", nil), t3Fence, runtime.FenceFoundUnproven},
		"no thread":   {map[string]interface{}{"threads": []interface{}{}}, t3Fence, runtime.FenceFoundAbsent},
	} {
		server.setSnapshots(tc.snapshot)
		before := len(server.commandTypes())
		for op, err := range map[string]error{
			"nudge":     p.NudgeFenced(ctx, turnFenceSession, runtime.TextContent("stale"), runtime.FencedNudge{Fence: tc.fence}),
			"keys":      p.SendKeysFenced(ctx, turnFenceSession, tc.fence, "Enter"),
			"interrupt": p.InterruptFenced(ctx, turnFenceSession, tc.fence),
		} {
			var miss *runtime.FenceMismatchError
			if !errors.As(err, &miss) || miss.Found != tc.found || miss.Partial || runtime.IsSessionGone(err) {
				t.Errorf("%s %s: %v, want a miss finding %s", label, op, err, tc.found)
			}
		}
		if got := server.commandTypes()[before:]; len(got) != 0 {
			t.Errorf("%s: dispatched %v after a miss", label, got)
		}
	}

	server.setSnapshots(fencedSnapshot(t, "thread-1", "stopped", t3FenceEnv(t3Fence)))
	err := p.InterruptFenced(ctx, turnFenceSession, t3Fence)
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundDead {
		t.Fatalf("InterruptFenced of a stopped session = %v, want a miss finding it dead", err)
	}
	if err := p.NudgeFenced(ctx, turnFenceSession, runtime.TextContent("x"), runtime.FencedNudge{Fence: runtime.InputFence{Generation: 1, Epoch: 1}}); !errors.Is(err, runtime.ErrInvalidInputFence) {
		t.Fatalf("NudgeFenced(invalid) = %v, want ErrInvalidInputFence", err)
	}
}

// Kills a fenced nudge that follows the session name to wherever it lives at
// send time: a respawn between the read and the dispatch that binds the
// session to a new thread leaves the turn on the thread the read proved. A
// respawn that rebinds the same thread is the checked residual the trait
// reports: T3 cannot condition the turn on the thread's binding.
func TestT3RespawnBetweenCheckAndSendStaysOnTheCheckedThread(t *testing.T) {
	server := newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(t3Fence)))
	p := newTurnFenceTestProvider(t, server)
	next := runtime.InputFence{Generation: 3, Epoch: 3, Token: "tok-next"}
	p.fenceChecked = func(string) {
		p.fenceChecked = nil
		server.setSnapshots(fencedSnapshot(t, "thread-2", "ready", t3FenceEnv(next)))
		p.clearSnapshotCache()
	}
	if err := p.NudgeFenced(context.Background(), turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence}); err != nil {
		t.Fatalf("NudgeFenced: %v", err)
	}
	starts := server.commandPayloads("thread.turn.start")
	if len(starts) != 1 || starts[0]["threadId"] != "thread-1" {
		t.Fatalf("turn starts = %+v, want one on the checked thread-1", starts)
	}
	if got := p.InputFencing(turnFenceSession); got != runtime.InputFencingChecked {
		t.Fatalf("fencing = %q, want checked: the same-thread residual must be reported", got)
	}
}
