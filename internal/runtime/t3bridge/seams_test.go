package t3bridge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// t3SeamProvider wires a Provider (via its seams) to a mock T3 bridge whose
// snapshot binds session "agent-a" to a ready thread (with a runtime provider/model
// so Nudge can dispatch a turn). It returns the bridge server too, so driving
// tests can inspect the dispatched commands. (Provision→Start's full
// thread-creation flow is exercised in provider_test.go; the seam delegation is
// trivial, so these tests drive via Open.)
func t3SeamProvider(t *testing.T) (runtime.Runtime, runtime.Transport, *t3BridgeTestServer) {
	t.Helper()
	resetBridgeAuthCacheForTest(t)
	server := newT3BridgeTestServer(t, map[string]interface{}{
		"threads": []interface{}{
			map[string]interface{}{
				"id":        "thread-1",
				"projectId": "project-1",
				"customMetadata": map[string]interface{}{
					"gc.agent":           "agent-a",
					"gc.sessionName":     "agent-a",
					"gc.runtimeProvider": "codex",
					"gc.startupModel":    "gpt-5.4",
				},
				"session": map[string]interface{}{"status": "ready"},
			},
		},
	})
	t.Cleanup(server.Close)
	t.Setenv("T3_BEARER_TOKEN", "test-bearer")
	t.Setenv("T3_WS_URL", server.wsURL())
	t.Setenv("GC_T3BRIDGE_STATE_DIR", t.TempDir())
	p := &Provider{
		watchers:     make(map[string]context.CancelFunc),
		recentStarts: make(map[string]time.Time),
	}
	rt, tp := p.Seams()
	return rt, tp, server
}

// TestSeamsT3ExecUnsupported proves Place.Exec is unsupported (t3bridge speaks
// turns, not in-box commands).
func TestSeamsT3ExecUnsupported(t *testing.T) {
	rt, _, _ := t3SeamProvider(t)
	ctx := context.Background()

	place, ok, err := rt.Open(ctx, "agent-a")
	if err != nil || !ok {
		t.Fatalf("Open(live) = %v, %v; want true, nil", ok, err)
	}
	if _, err := place.Exec(ctx, runtime.ExecRequest{Argv: []string{"echo", "hi"}}); !errors.Is(err, runtime.ErrExecUnsupported) {
		t.Fatalf("Exec err = %v; want ErrExecUnsupported", err)
	}
}

// TestSeamsT3OpenLiveAndAbsent proves Open reflects T3 thread liveness.
func TestSeamsT3OpenLiveAndAbsent(t *testing.T) {
	rt, _, _ := t3SeamProvider(t)
	ctx := context.Background()

	if _, ok, err := rt.Open(ctx, "agent-a"); err != nil || !ok {
		t.Fatalf("Open(live) = %v, %v; want true, nil", ok, err)
	}
	if pl, ok, err := rt.Open(ctx, "ghost"); pl != nil || ok || err != nil {
		t.Fatalf("Open(absent) = %v, %v, %v; want nil, false, nil", pl, ok, err)
	}
}

// TestSeamsT3CapabilitiesAndTransport proves the capability split and the
// bespoke "t3" transport identity.
func TestSeamsT3CapabilitiesAndTransport(t *testing.T) {
	rt, tp, _ := t3SeamProvider(t)

	if caps := rt.Capabilities(); !caps.ReportActivity {
		t.Fatalf("PlaceCapabilities = %+v; want ReportActivity true", caps)
	}
	if tp.Capabilities().ReportAttachment {
		t.Fatal("TransportCapabilities.ReportAttachment should be false for t3bridge")
	}
	if tp.Name() != "t3" {
		t.Fatalf("Name = %q; want t3", tp.Name())
	}
}

// TestSeamsT3Observe proves Observe folds the liveness reads against the bridge:
// ProcessAlive from the ready thread status, Attached false (headless), and
// LastActivity zero (the snapshot carries no updated-at).
func TestSeamsT3Observe(t *testing.T) {
	rt, tp, _ := t3SeamProvider(t)
	ctx := context.Background()

	place, ok, err := rt.Open(ctx, "agent-a")
	if err != nil || !ok {
		t.Fatalf("Open: %v, %v", ok, err)
	}
	att, err := tp.Launch(ctx, place, runtime.LaunchSpec{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	obs, err := att.Observe(ctx, nil)
	if err != nil {
		t.Fatalf("Observe: %v", err)
	}
	if !obs.ProcessAlive {
		t.Fatal("Observe ProcessAlive = false; want true (thread status ready)")
	}
	if obs.Attached {
		t.Fatal("Observe Attached = true; want false (T3 is headless)")
	}
	if !obs.LastActivity.IsZero() {
		t.Fatalf("Observe LastActivity = %v; want zero (no updated-at)", obs.LastActivity)
	}

	// The headless no-op verbs return nil.
	if err := att.SendKeys(ctx, "Enter"); err != nil {
		t.Fatalf("SendKeys: %v", err)
	}
	if err := att.ClearScrollback(ctx); err != nil {
		t.Fatalf("ClearScrollback: %v", err)
	}
}

// TestSeamsT3Driving proves the t3-specific mapping of driving verbs onto TURNS:
// Nudge dispatches thread.turn.start and Interrupt dispatches thread.turn.interrupt.
func TestSeamsT3Driving(t *testing.T) {
	rt, tp, server := t3SeamProvider(t)
	ctx := context.Background()

	place, ok, err := rt.Open(ctx, "agent-a")
	if err != nil || !ok {
		t.Fatalf("Open: %v, %v", ok, err)
	}
	att, err := tp.Launch(ctx, place, runtime.LaunchSpec{})
	if err != nil {
		t.Fatalf("launch: %v", err)
	}

	if err := att.Nudge(ctx, runtime.TextContent("go")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if err := att.Interrupt(ctx); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}

	got := server.commandTypes()
	if !containsString(got, "thread.turn.start") {
		t.Fatalf("commands = %v; want a thread.turn.start (Nudge)", got)
	}
	if !containsString(got, "thread.turn.interrupt") {
		t.Fatalf("commands = %v; want a thread.turn.interrupt (Interrupt)", got)
	}
}

// TestSeamsT3MetaStore proves the MetaStore seam round-trips through the
// provider's file-backed meta.
func TestSeamsT3MetaStore(t *testing.T) {
	rt, _, _ := t3SeamProvider(t)

	ms, ok := rt.(runtime.MetaStore)
	if !ok {
		t.Fatal("t3bridge Runtime should implement runtime.MetaStore")
	}
	if err := ms.SetMeta("agent-a", "k", "v"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if got, err := ms.GetMeta("agent-a", "k"); err != nil || got != "v" {
		t.Fatalf("GetMeta = %q, %v; want v, nil", got, err)
	}
	if err := ms.RemoveMeta("agent-a", "k"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
	if got, _ := ms.GetMeta("agent-a", "k"); got != "" {
		t.Fatalf("GetMeta after remove = %q; want empty", got)
	}
}

// TestSeamsT3StageAndTeardown proves Stage is best-effort (no workdir in the
// snapshot → no-op) and Teardown delegates to Stop.
func TestSeamsT3StageAndTeardown(t *testing.T) {
	rt, _, _ := t3SeamProvider(t)
	ctx := context.Background()

	place, ok, err := rt.Open(ctx, "agent-a")
	if err != nil || !ok {
		t.Fatalf("Open: %v, %v", ok, err)
	}
	if err := place.Stage(ctx, []runtime.CopyEntry{{Src: "/a", RelDst: "x"}}); err != nil {
		t.Fatalf("Stage = %v; want nil (best-effort, no workdir)", err)
	}
	if err := place.Teardown(ctx); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
