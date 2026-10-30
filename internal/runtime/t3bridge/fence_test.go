package t3bridge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

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

// reuseFenceConfig starts turnFenceSession as incarnation f on the thread
// fencedSnapshot holds.
func reuseFenceConfig(f runtime.InputFence) runtime.Config {
	env := t3FenceEnv(f)
	env["GC_CITY_PATH"] = "/tmp/gc"
	env["GC_ALIAS"] = turnFenceSession
	env["GC_TEMPLATE"] = turnFenceSession
	env["GC_PROVIDER"] = "codex"
	env["GC_MODEL"] = "gpt-5.4"
	return runtime.Config{WorkDir: "/tmp/" + turnFenceSession, Command: "codex", Env: env}
}

func reuseFenceSnapshot(t *testing.T, cfg runtime.Config, prev runtime.InputFence) map[string]interface{} {
	t.Helper()
	snap := fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(prev))
	thread := snap["threads"].([]interface{})[0].(map[string]interface{})
	meta := thread["customMetadata"].(map[string]interface{})
	meta["gc.agent"] = turnFenceSession
	meta["gc.startupTemplate"] = turnFenceSession
	meta["gc.startupWorkDir"] = cfg.WorkDir
	meta["gc.runtimeProvider"] = "codex"
	meta["gc.startupModel"] = "gpt-5.4"
	thread["title"] = turnFenceSession + " · " + turnFenceSession
	snap["projects"] = []interface{}{map[string]interface{}{"id": "project-1", "workspaceRoot": cfg.WorkDir}}
	return snap
}

// Kills a reuse that stops or rebinds the thread before it records the new
// incarnation: the thread's gc.sessionEnv names the new fence before the
// session stop goes out, so no window leaves the reused thread proving the
// previous incarnation.
func TestT3StartReuseRecordsTheNewIncarnationBeforeStopping(t *testing.T) {
	next := runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok-next"}
	cfg := reuseFenceConfig(next)
	server := newTurnFenceServer(t, reuseFenceSnapshot(t, cfg, t3Fence))
	p := newTurnFenceTestProvider(t, server)
	t.Cleanup(func() { p.stopEventWatcher(turnFenceSession) })

	if err := p.Start(context.Background(), turnFenceSession, cfg); err != nil {
		t.Fatalf("Start(reuse): %v", err)
	}
	types := server.commandTypes()
	if len(types) == 0 || types[0] != "thread.meta.update" {
		t.Fatalf("commands = %v, want the metadata write first", types)
	}
	first := server.commandPayloads("thread.meta.update")[0]
	meta, _ := first["customMetadata"].(map[string]interface{})
	raw, _ := meta["gc.sessionEnv"].(string)
	got, ok := runtime.InputFenceFromEnv(ParseSessionEnv(raw))
	if !ok || got.Digest() != next.Digest() {
		t.Fatalf("first metadata write records %q, want the new incarnation", raw)
	}
	if !containsString(types, "thread.session.stop") {
		t.Fatalf("commands = %v, want the stale session stopped", types)
	}
}

// Kills a reuse that swallows a failed metadata write: the thread would keep
// proving the previous incarnation, so Start fails and neither stops nor
// rebinds the thread.
func TestT3StartReuseFailsWhenTheIncarnationCannotBeRecorded(t *testing.T) {
	next := runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok-next"}
	cfg := reuseFenceConfig(next)
	server := newTurnFenceServer(t, reuseFenceSnapshot(t, cfg, t3Fence))
	server.rejectCommand("thread.meta.update", "metadata refused")
	p := newTurnFenceTestProvider(t, server)
	t.Cleanup(func() { p.stopEventWatcher(turnFenceSession) })

	if err := p.Start(context.Background(), turnFenceSession, cfg); err == nil {
		t.Fatal("Start(reuse) with the metadata write refused = nil, want an error")
	}
	if got := server.commandTypes(); len(got) != 1 || got[0] != "thread.meta.update" {
		t.Fatalf("commands = %v, want only the refused metadata write", got)
	}
}

// Kills a fenced nudge that starts a turn on a thread whose T3 session the
// fresh read shows stopped: T3 would relaunch a provider for an incarnation
// that is gone, so the nudge is a miss finding it dead and dispatches
// nothing. A legacy session status (none, gone) is dead too, except within
// the start grace, where it still delivers.
func TestT3FencedNudgeRefusesAStoppedSession(t *testing.T) {
	server := newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "stopped", t3FenceEnv(t3Fence)))
	p := newTurnFenceTestProvider(t, server)
	err := p.NudgeFenced(t.Context(), turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence})
	var miss *runtime.FenceMismatchError
	if !errors.As(err, &miss) || miss.Found != runtime.FenceFoundDead || miss.Partial {
		t.Fatalf("NudgeFenced of a stopped session = %v, want a miss finding it dead", err)
	}
	if got := server.commandTypes(); len(got) != 0 {
		t.Fatalf("dispatched %v to a stopped session", got)
	}

	server.setSnapshots(fencedSnapshot(t, "thread-1", "none", t3FenceEnv(t3Fence)))
	if err := p.NudgeFenced(t.Context(), turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence}); !errors.As(err, &miss) || miss.Found != runtime.FenceFoundDead {
		t.Fatalf("NudgeFenced of a legacy status outside the start grace = %v, want a miss finding it dead", err)
	}
	if got := server.commandTypes(); len(got) != 0 {
		t.Fatalf("dispatched %v to a legacy status outside the start grace", got)
	}
	p.setRecentStart(turnFenceSession, time.Now())
	if err := p.NudgeFenced(t.Context(), turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence}); err != nil {
		t.Fatalf("NudgeFenced of a legacy status within the start grace = %v, want delivered", err)
	}
	if got := server.commandPayloads("thread.turn.start"); len(got) != 1 {
		t.Fatalf("turn starts = %d within the start grace, want 1", len(got))
	}
	p.clearRecentStart(turnFenceSession)
	server = newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "weird", t3FenceEnv(t3Fence)))
	p = newTurnFenceTestProvider(t, server)

	server.setSnapshots(fencedSnapshot(t, "thread-1", "weird", t3FenceEnv(t3Fence)))
	if err := p.NudgeFenced(t.Context(), turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence}); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("NudgeFenced of an unclassified status = %v, want ErrRuntimeUnavailable", err)
	}
	if got := server.commandTypes(); len(got) != 0 {
		t.Fatalf("dispatched %v to an unclassified session", got)
	}
}

// Kills a fenced nudge whose transient retry goes out without re-checking the
// fence: once T3 took the first turn start unanswered and the session was
// rebound to another incarnation, the retry is withheld and the call reports
// it cannot tell (the first attempt may have landed). Under an unchanged
// fence the retry goes out with the same ids.
func TestT3FencedNudgeRetryRechecksTheFence(t *testing.T) {
	next := runtime.InputFence{Generation: 2, Epoch: 3, Token: "tok-next"}
	server := newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(t3Fence)))
	p := newTurnFenceTestProvider(t, server)
	server.dropNextCommand()
	server.setCommandHook(func(map[string]interface{}) {
		server.setSnapshots(fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(next)))
	})
	err := p.NudgeFenced(t.Context(), turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence, DeliveryKey: "k1"})
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) {
		t.Fatalf("NudgeFenced after a rebind between attempts = %v, want ErrRuntimeUnavailable, unconfirmed: T3 took the first attempt", err)
	}
	if got := server.commandPayloads("thread.turn.start"); len(got) != 1 {
		t.Fatalf("turn starts = %d, want the first attempt only", len(got))
	}

	server = newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(t3Fence)))
	p = newTurnFenceTestProvider(t, server)
	server.dropNextCommand()
	if err := p.NudgeFenced(t.Context(), turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence, DeliveryKey: "k1"}); err != nil {
		t.Fatalf("NudgeFenced retried under the same fence: %v", err)
	}
	if got := keyedNudgeIDs(t, server); len(got) != 2 || got[0] != got[1] {
		t.Fatalf("turn ids = %v, want the same ids on both attempts", got)
	}

	server = newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(t3Fence)))
	p = newTurnFenceTestProvider(t, server)
	server.dropNextCommand()
	server.setCommandHook(func(map[string]interface{}) {
		server.setSnapshots(fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(next)))
	})
	if err := p.InterruptFenced(t.Context(), turnFenceSession, t3Fence); !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed) {
		t.Fatalf("InterruptFenced after a rebind between attempts = %v, want ErrRuntimeUnavailable and no nudge outcome", err)
	}
	if got := server.commandPayloads("thread.turn.interrupt"); len(got) != 1 {
		t.Fatalf("interrupts = %d, want the first attempt only", len(got))
	}
}

// Kills a fenced retry whose re-check accepts a binding the first attempt
// was not checked against: the session rebound to another thread under the
// same fence, or the same thread whose session stopped between attempts.
// Either way the retry is withheld and the call reports it cannot tell.
func TestT3FencedRetryNeedsTheSameThreadAndALiveSession(t *testing.T) {
	for label, snapshot := range map[string]func() map[string]interface{}{
		"rebound to another thread": func() map[string]interface{} {
			return fencedSnapshot(t, "thread-2", "ready", t3FenceEnv(t3Fence))
		},
		"session stopped": func() map[string]interface{} {
			return fencedSnapshot(t, "thread-1", "stopped", t3FenceEnv(t3Fence))
		},
	} {
		t.Run(label, func(t *testing.T) {
			for op, call := range map[string]struct {
				command string
				run     func(*Provider) error
			}{
				"nudge": {"thread.turn.start", func(p *Provider) error {
					return p.NudgeFenced(t.Context(), turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence, DeliveryKey: "k1"})
				}},
				"interrupt": {"thread.turn.interrupt", func(p *Provider) error {
					return p.InterruptFenced(t.Context(), turnFenceSession, t3Fence)
				}},
			} {
				server := newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(t3Fence)))
				p := newTurnFenceTestProvider(t, server)
				server.dropNextCommand()
				server.setCommandHook(func(map[string]interface{}) { server.setSnapshots(snapshot()) })
				err := call.run(p)
				if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrFenceMismatch) {
					t.Fatalf("%s = %v, want ErrRuntimeUnavailable", op, err)
				}
				if got, want := errors.Is(err, runtime.ErrNudgeDeliveryUnconfirmed), op == "nudge"; got != want {
					t.Fatalf("%s = %v, unconfirmed %v, want %v: T3 took the first attempt unanswered", op, err, got, want)
				}
				if got := server.commandPayloads(call.command); len(got) != 1 {
					t.Fatalf("%s: %s commands = %d, want the first attempt only", op, call.command, len(got))
				}
			}
		})
	}
}

// Kills a fenced interrupt that drops the caller's ctx: a ctx that ends
// after the fence check sends no interrupt.
func TestT3FencedInterruptHonorsTheCallersContext(t *testing.T) {
	server := newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(t3Fence)))
	p := newTurnFenceTestProvider(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	p.fenceChecked = func(string) { cancel() }
	if err := p.InterruptFenced(ctx, turnFenceSession, t3Fence); !errors.Is(err, context.Canceled) {
		t.Fatalf("InterruptFenced with ctx canceled after the check = %v, want context.Canceled", err)
	}
	if got := server.commandPayloads("thread.turn.interrupt"); len(got) != 0 {
		t.Fatalf("interrupts = %d after ctx ended, want none", len(got))
	}
}

// Kills a fenced nudge that drops the caller's ctx: a ctx that ends after the
// fence check sends no turn.
func TestT3FencedNudgeHonorsTheCallersContext(t *testing.T) {
	server := newTurnFenceServer(t, fencedSnapshot(t, "thread-1", "ready", t3FenceEnv(t3Fence)))
	p := newTurnFenceTestProvider(t, server)
	ctx, cancel := context.WithCancel(t.Context())
	p.fenceChecked = func(string) { cancel() }
	err := p.NudgeFenced(ctx, turnFenceSession, runtime.TextContent("hello"), runtime.FencedNudge{Fence: t3Fence})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("NudgeFenced with ctx canceled after the check = %v, want context.Canceled", err)
	}
	if got := server.commandPayloads("thread.turn.start"); len(got) != 0 {
		t.Fatalf("turn starts = %d after ctx ended, want none", len(got))
	}
}
