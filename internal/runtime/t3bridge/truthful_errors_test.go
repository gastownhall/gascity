package t3bridge

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// unreachableBridgeURL is a loopback endpoint nothing listens on: every
// snapshot read fails with a soft (transient) bridge error.
const unreachableBridgeURL = "ws://127.0.0.1:1/ws"

// boundThreadSnapshot is a T3 snapshot binding session "agent-a" to a thread
// whose session has status.
func boundThreadSnapshot(status string) map[string]interface{} {
	const name = "agent-a"
	return map[string]interface{}{
		"threads": []interface{}{
			map[string]interface{}{
				"id":        "thread-" + name,
				"projectId": "project-1",
				"customMetadata": map[string]interface{}{
					"gc.agent":           name,
					"gc.sessionName":     name,
					"gc.city":            "city-a",
					"gc.bead":            "bead-1",
					"gc.runtimeProvider": "codex",
					"gc.startupModel":    "gpt-5.4",
				},
				"session": map[string]interface{}{"status": status},
			},
		},
	}
}

// A nudge the bridge could not deliver because T3 was unreachable is an
// observation failure, never a silent success the nudge queue would ack.
func TestSeamBackedNudgeSoftUnavailableIsRuntimeUnavailable(t *testing.T) {
	_, sp := newSeamBackedSnapshotTestProvider(t, unreachableBridgeURL)
	err := sp.Nudge("agent-a", runtime.TextContent("hello"))
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("Nudge during bridge outage = %v, want errors.Is(runtime.ErrRuntimeUnavailable)", err)
	}
}

// The startup grace answers only for a session T3 shows no thread for, not
// for one T3 could not be read about: an outage is reported inside it too.
func TestSeamBackedInterruptSoftUnavailableIsRuntimeUnavailable(t *testing.T) {
	for _, tc := range []struct {
		name            string
		startedRecently bool
	}{
		{name: "no recent start"},
		{name: "inside the startup grace", startedRecently: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backed, sp := newSeamBackedSnapshotTestProvider(t, unreachableBridgeURL)
			if tc.startedRecently {
				backed.raw.setRecentStart("agent-a", time.Now())
			}
			err := sp.Interrupt("agent-a")
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("Interrupt during bridge outage = %v, want errors.Is(runtime.ErrRuntimeUnavailable)", err)
			}
		})
	}
}

// With T3 reachable, a session that has no bound thread cannot take a turn
// or be interrupted: report it as not found and dispatch nothing.
func TestSeamBackedNudgeAndInterruptWithoutThreadAreSessionNotFound(t *testing.T) {
	server := newT3BridgeTestServer(t, map[string]interface{}{"threads": []interface{}{}})
	t.Cleanup(server.Close)
	_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())

	if err := sp.Nudge("agent-a", runtime.TextContent("hello")); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Nudge = %v, want errors.Is(runtime.ErrSessionNotFound)", err)
	}
	if err := sp.Interrupt("agent-a"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Interrupt = %v, want errors.Is(runtime.ErrSessionNotFound)", err)
	}
	if got := server.commandTypes(); len(got) != 0 {
		t.Fatalf("dispatched commands = %v, want none for a session without a thread", got)
	}
}

// A bound thread whose T3 session is not live still takes a nudge: T3
// relaunches the provider on thread.turn.start, which is how a reused or
// idle-reaped thread wakes with its message. There is no turn to interrupt,
// so Interrupt reports the session as not found.
func TestSeamBackedNudgeWakesBoundThreadWithoutLiveSession(t *testing.T) {
	for _, status := range []string{"stopped", "error", "interrupted"} {
		t.Run(status, func(t *testing.T) {
			server := newT3BridgeTestServer(t, boundThreadSnapshot(status))
			t.Cleanup(server.Close)
			_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())

			if err := sp.Nudge("agent-a", runtime.TextContent("hello")); err != nil {
				t.Fatalf("Nudge to a %s thread = %v, want nil", status, err)
			}
			if got := server.commandPayloads("thread.turn.start"); len(got) != 1 {
				t.Fatalf("turn starts = %d, want 1 waking the %s thread", len(got), status)
			}
			if err := sp.Interrupt("agent-a"); !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("Interrupt of a %s thread = %v, want errors.Is(runtime.ErrSessionNotFound)", status, err)
			}
			if containsString(server.commandTypes(), "thread.turn.interrupt") {
				t.Fatalf("dispatched commands = %v, want no interrupt for a %s thread", server.commandTypes(), status)
			}
		})
	}
}

// IsRunning counts a just-started session as running before T3 shows its
// thread, and StopTurn interrupts only a running session. Inside that startup
// grace, Interrupt has no thread to send the cancel to: it sends nothing and
// reports success, not a not-found for a session IsRunning just reported
// running. Once the grace has passed, a session T3 does not show is not found.
func TestSeamBackedInterruptWithoutThreadDuringStartupGrace(t *testing.T) {
	for _, tc := range []struct {
		name        string
		startedAgo  time.Duration
		wantRunning bool
		wantErr     error
	}{
		{name: "inside the grace", wantRunning: true},
		{name: "after the grace", startedAgo: recentStartGracePeriod + time.Second, wantErr: runtime.ErrSessionNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newT3BridgeTestServer(t, map[string]interface{}{"threads": []interface{}{}})
			t.Cleanup(server.Close)
			backed, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
			backed.raw.setRecentStart("agent-a", time.Now().Add(-tc.startedAgo))

			if got := sp.IsRunning("agent-a"); got != tc.wantRunning {
				t.Fatalf("IsRunning = %v, want %v", got, tc.wantRunning)
			}
			err := sp.Interrupt("agent-a")
			if tc.wantErr == nil && err != nil {
				t.Fatalf("Interrupt = %v, want nil", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Interrupt = %v, want errors.Is(%v)", err, tc.wantErr)
			}
			if got := server.commandTypes(); len(got) != 0 {
				t.Fatalf("dispatched commands = %v, want none for a session T3 shows no thread for", got)
			}
		})
	}
}

// Start's reuse path stops a thread whose session is not running and sends
// no turn; the message that woke the session follows as a nudge and must
// reach the thread.
func TestStartReuseThenNudgeDeliversTheTurn(t *testing.T) {
	const name = "agent-a"
	cfg := runtime.Config{
		WorkDir: "/tmp/agent-a",
		Command: "codex",
		Env: map[string]string{
			"GC_CITY_PATH": "/tmp/gc",
			"GC_ALIAS":     name,
			"GC_TEMPLATE":  name,
			"GC_PROVIDER":  "codex",
			"GC_MODEL":     "gpt-5.4",
		},
	}
	server := newT3BridgeTestServer(t, map[string]interface{}{
		"projects": []interface{}{
			map[string]interface{}{"id": "project-1", "workspaceRoot": cfg.WorkDir},
		},
		"threads": []interface{}{
			map[string]interface{}{
				"id":        "thread-1",
				"projectId": "project-1",
				"title":     name + " · " + name,
				"model":     "gpt-5.4",
				"customMetadata": map[string]interface{}{
					"gc.agent":           name,
					"gc.sessionName":     name,
					"gc.startupTemplate": name,
					"gc.startupWorkDir":  cfg.WorkDir,
					"gc.runtimeProvider": "codex",
					"gc.startupModel":    "gpt-5.4",
				},
				"session": map[string]interface{}{"status": "stopped"},
			},
		},
	})
	t.Cleanup(server.Close)
	backed, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
	t.Cleanup(func() { backed.raw.stopEventWatcher(name) })

	if err := sp.Start(context.Background(), name, cfg); err != nil {
		t.Fatalf("Start(reuse): %v", err)
	}
	if got := server.commandPayloads("thread.turn.start"); len(got) != 0 {
		t.Fatalf("Start(reuse) sent %d turn starts, want none", len(got))
	}
	if err := sp.Nudge(name, runtime.TextContent("wake up")); err != nil {
		t.Fatalf("Nudge after reuse = %v, want nil", err)
	}
	turns := server.commandPayloads("thread.turn.start")
	if len(turns) != 1 || turns[0]["threadId"] != "thread-1" {
		t.Fatalf("turn starts after Nudge = %v, want one on thread-1", turns)
	}
}

func TestSeamBackedNudgeAndInterruptLiveThreadDispatch(t *testing.T) {
	server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
	t.Cleanup(server.Close)
	_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())

	if err := sp.Nudge("agent-a", runtime.TextContent("hello")); err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if err := sp.Interrupt("agent-a"); err != nil {
		t.Fatalf("Interrupt: %v", err)
	}
	got := server.commandTypes()
	if !containsString(got, "thread.turn.start") || !containsString(got, "thread.turn.interrupt") {
		t.Fatalf("dispatched commands = %v, want thread.turn.start and thread.turn.interrupt", got)
	}
	for _, typ := range []string{"thread.turn.start", "thread.turn.interrupt"} {
		for _, payload := range server.commandPayloads(typ) {
			if payload["threadId"] != "thread-agent-a" {
				t.Fatalf("%s threadId = %v, want thread-agent-a", typ, payload["threadId"])
			}
		}
	}
}

// A stop that could not reach T3 did not stop anything. It must say so, and
// keep the drain bookkeeping a retried stop needs to archive the thread.
func TestSeamBackedStopSoftUnavailableIsRuntimeUnavailable(t *testing.T) {
	backed, sp := newSeamBackedSnapshotTestProvider(t, unreachableBridgeURL)
	if err := writeMetaValue("agent-a", "drained", "1"); err != nil {
		t.Fatalf("seed drained marker: %v", err)
	}
	err := sp.Stop("agent-a")
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("Stop during bridge outage = %v, want errors.Is(runtime.ErrRuntimeUnavailable)", err)
	}
	if got, _ := backed.raw.GetMeta("agent-a", "drained"); got != "1" {
		t.Fatalf("drained marker after failed stop = %q, want it kept for the retry", got)
	}
}

func TestSeamBackedStopPropagatesSessionStopRejection(t *testing.T) {
	server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
	t.Cleanup(server.Close)
	server.rejectCommand("thread.session.stop", "thread is locked")
	_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())

	err := sp.Stop("agent-a")
	if err == nil {
		t.Fatal("Stop with a rejected thread.session.stop = nil, want the rejection")
	}
	if !containsString(server.commandTypes(), "thread.session.stop") {
		t.Fatalf("dispatched commands = %v, want a thread.session.stop attempt", server.commandTypes())
	}
}

// Stopping a session that has no thread is idempotent: there is nothing to stop.
func TestSeamBackedStopWithoutThreadSucceeds(t *testing.T) {
	server := newT3BridgeTestServer(t, map[string]interface{}{"threads": []interface{}{}})
	t.Cleanup(server.Close)
	_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
	if err := sp.Stop("agent-a"); err != nil {
		t.Fatalf("Stop without a thread = %v, want nil", err)
	}
}

// A drain transition that could not be reflected onto the T3 thread reports
// the outage; the local value (and the drained marker Stop archives on) still
// lands.
func TestSetMetaDrainAckSoftUnavailableIsRuntimeUnavailable(t *testing.T) {
	backed, sp := newSeamBackedSnapshotTestProvider(t, unreachableBridgeURL)
	err := sp.SetMeta("agent-a", "GC_DRAIN_ACK", "1")
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, runtime.ErrMetadataNotMirrored) {
		t.Fatalf("SetMeta(GC_DRAIN_ACK) during bridge outage = %v, want errors.Is(runtime.ErrRuntimeUnavailable) and errors.Is(runtime.ErrMetadataNotMirrored)", err)
	}
	if got, _ := backed.raw.GetMeta("agent-a", "GC_DRAIN_ACK"); got != "1" {
		t.Fatalf("GC_DRAIN_ACK = %q, want the local value persisted", got)
	}
	if got, _ := backed.raw.GetMeta("agent-a", "drained"); got != "1" {
		t.Fatalf("drained = %q, want the local drained marker persisted", got)
	}
}

// The outage error a drain transition reports is a mirror failure and nothing
// else, so callers that need only the stored value can treat it as a warning.
func TestSetMetaDrainOutageIsOnlyAMirrorFailure(t *testing.T) {
	_, sp := newSeamBackedSnapshotTestProvider(t, unreachableBridgeURL)
	for _, key := range []string{"GC_DRAIN", "GC_DRAIN_ACK"} {
		err := sp.SetMeta("agent-a", key, "1")
		if !runtime.OnlyMetadataNotMirrored(err) {
			t.Fatalf("SetMeta(%s) during outage = %v (%T), want runtime.OnlyMetadataNotMirrored", key, err, err)
		}
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("SetMeta(%s) during outage = %v, want errors.Is(runtime.ErrRuntimeUnavailable)", key, err)
		}
	}
}

// A key T3 never mirrors needs no bridge round trip, so an outage cannot fail it.
func TestSetMetaUnreflectedKeyIgnoresBridgeOutage(t *testing.T) {
	backed, sp := newSeamBackedSnapshotTestProvider(t, unreachableBridgeURL)
	if err := sp.SetMeta("agent-a", "GC_DRAIN_ACK_SOURCE", "agent"); err != nil {
		t.Fatalf("SetMeta(unreflected key) during bridge outage = %v, want nil", err)
	}
	if got, _ := backed.raw.GetMeta("agent-a", "GC_DRAIN_ACK_SOURCE"); got != "agent" {
		t.Fatalf("GC_DRAIN_ACK_SOURCE = %q, want agent", got)
	}
}

// Clearing a drain ack clears the local drained marker even when T3 cannot be
// told, mirroring SetMeta: a marker left behind would make a later, unrelated
// Stop archive the live thread.
func TestRemoveMetaDrainAckDuringOutageClearsDrainedMarker(t *testing.T) {
	_, sp := newSeamBackedSnapshotTestProvider(t, unreachableBridgeURL)
	if err := sp.SetMeta("agent-a", "GC_DRAIN_ACK", "1"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("SetMeta(GC_DRAIN_ACK) during outage = %v, want errors.Is(runtime.ErrRuntimeUnavailable)", err)
	}
	if err := sp.RemoveMeta("agent-a", "GC_DRAIN_ACK"); err != nil && !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("RemoveMeta(GC_DRAIN_ACK) during outage = %v, want nil or errors.Is(runtime.ErrRuntimeUnavailable)", err)
	}

	server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
	t.Cleanup(server.Close)
	t.Setenv("T3_WS_URL", server.wsURL())
	resetBridgeAuthCacheForTest(t)
	recovered := NewSeamBacked()
	if got, _ := recovered.GetMeta("agent-a", "drained"); got != "" {
		t.Fatalf("drained after the ack was cleared = %q, want empty", got)
	}
	if err := recovered.Stop("agent-a"); err != nil {
		t.Fatalf("Stop after recovery: %v", err)
	}
	for _, payload := range server.commandPayloads("thread.meta.update") {
		if meta, _ := payload["customMetadata"].(map[string]interface{}); meta["gc.state"] == "archived" {
			t.Fatalf("Stop archived the thread after its drain ack was cleared: %v", payload)
		}
	}
}

// A Stop that fails leaves the session running, so it must keep the session's
// T3 state watcher; a Stop that succeeds tears it down.
func TestStopKeepsEventWatcherUntilTheStopSucceeds(t *testing.T) {
	watching := func(p *Provider) bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		_, ok := p.watchers["agent-a"]
		return ok
	}
	install := func(p *Provider) {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.watchers["agent-a"] = func() {}
	}

	t.Run("outage", func(t *testing.T) {
		backed, sp := newSeamBackedSnapshotTestProvider(t, unreachableBridgeURL)
		install(backed.raw)
		if err := sp.Stop("agent-a"); err == nil {
			t.Fatal("Stop during outage = nil, want an error")
		}
		if !watching(backed.raw) {
			t.Fatal("failed Stop tore down the event watcher")
		}
	})
	t.Run("rejected session stop", func(t *testing.T) {
		server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
		t.Cleanup(server.Close)
		server.rejectCommand("thread.session.stop", "thread is locked")
		backed, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
		install(backed.raw)
		if err := sp.Stop("agent-a"); err == nil {
			t.Fatal("Stop with a rejected thread.session.stop = nil, want an error")
		}
		if !watching(backed.raw) {
			t.Fatal("failed Stop tore down the event watcher")
		}
	})
	t.Run("success", func(t *testing.T) {
		server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
		t.Cleanup(server.Close)
		backed, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
		install(backed.raw)
		if err := sp.Stop("agent-a"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		if watching(backed.raw) {
			t.Fatal("successful Stop left the event watcher running")
		}
	})
}

// RemoveMeta mirrors only drain transitions, so clearing any other key needs no
// T3 round trip and cannot fail on one.
func TestRemoveMetaUnreflectedKeySkipsTheBridge(t *testing.T) {
	server := newT3BridgeTestServer(t, map[string]interface{}{"threads": []interface{}{}})
	t.Cleanup(server.Close)
	server.snapshotFailStatus = http.StatusUnauthorized
	server.snapshotFailBody = "bad token"
	backed, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
	if err := writeMetaValue("agent-a", "GC_DRAIN_ACK_SOURCE", "agent"); err != nil {
		t.Fatal(err)
	}
	if err := sp.RemoveMeta("agent-a", "GC_DRAIN_ACK_SOURCE"); err != nil {
		t.Fatalf("RemoveMeta(unreflected key) with a failing snapshot = %v, want nil", err)
	}
	if got, _ := backed.raw.GetMeta("agent-a", "GC_DRAIN_ACK_SOURCE"); got != "" {
		t.Fatalf("GC_DRAIN_ACK_SOURCE = %q, want it removed", got)
	}
	server.mu.Lock()
	requests := server.snapshotRequests
	server.mu.Unlock()
	if requests != 0 {
		t.Fatalf("snapshot requests = %d, want 0 for a key T3 does not mirror", requests)
	}
}

// A drain clear that landed locally but could not be reflected onto the T3
// thread is a mirror failure, whether T3 is unreachable or refuses the read.
func TestRemoveMetaDrainKeysReportOnlyAMirrorFailure(t *testing.T) {
	failing := newT3BridgeTestServer(t, map[string]interface{}{"threads": []interface{}{}})
	t.Cleanup(failing.Close)
	failing.snapshotFailStatus = http.StatusUnauthorized
	failing.snapshotFailBody = "bad token"
	for _, tc := range []struct {
		name            string
		url             string
		wantUnavailable bool
	}{
		{"unreachable", unreachableBridgeURL, true},
		{"snapshot refused", failing.wsURL(), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backed, sp := newSeamBackedSnapshotTestProvider(t, tc.url)
			for _, key := range []string{"GC_DRAIN", "GC_DRAIN_ACK"} {
				if err := writeMetaValue("agent-a", key, "1"); err != nil {
					t.Fatal(err)
				}
				err := sp.RemoveMeta("agent-a", key)
				if !runtime.OnlyMetadataNotMirrored(err) {
					t.Fatalf("RemoveMeta(%s) = %v (%T), want runtime.OnlyMetadataNotMirrored", key, err, err)
				}
				if got := errors.Is(err, runtime.ErrRuntimeUnavailable); got != tc.wantUnavailable {
					t.Fatalf("RemoveMeta(%s) = %v, errors.Is(ErrRuntimeUnavailable) = %v, want %v", key, err, got, tc.wantUnavailable)
				}
				if got, _ := backed.raw.GetMeta("agent-a", key); got != "" {
					t.Fatalf("%s = %q, want the local value removed", key, got)
				}
			}
		})
	}
}
