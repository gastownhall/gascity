package t3bridge

import (
	"errors"
	"testing"

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

func TestSeamBackedInterruptSoftUnavailableIsRuntimeUnavailable(t *testing.T) {
	_, sp := newSeamBackedSnapshotTestProvider(t, unreachableBridgeURL)
	err := sp.Interrupt("agent-a")
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("Interrupt during bridge outage = %v, want errors.Is(runtime.ErrRuntimeUnavailable)", err)
	}
}

// With T3 reachable, a session that has no bound thread, or whose thread's
// session is not live, cannot take a turn: report it as not found, and never
// dispatch a turn that would resurrect a stopped thread.
func TestSeamBackedNudgeAndInterruptWithoutLiveThreadAreSessionNotFound(t *testing.T) {
	for _, tc := range []struct {
		name     string
		snapshot map[string]interface{}
	}{
		{name: "no thread", snapshot: map[string]interface{}{"threads": []interface{}{}}},
		{name: "stopped thread", snapshot: boundThreadSnapshot("stopped")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := newT3BridgeTestServer(t, tc.snapshot)
			t.Cleanup(server.Close)
			_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())

			if err := sp.Nudge("agent-a", runtime.TextContent("hello")); !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("Nudge = %v, want errors.Is(runtime.ErrSessionNotFound)", err)
			}
			if err := sp.Interrupt("agent-a"); !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("Interrupt = %v, want errors.Is(runtime.ErrSessionNotFound)", err)
			}
			if got := server.commandTypes(); len(got) != 0 {
				t.Fatalf("dispatched commands = %v, want none for a session without a live thread", got)
			}
		})
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
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("SetMeta(GC_DRAIN_ACK) during bridge outage = %v, want errors.Is(runtime.ErrRuntimeUnavailable)", err)
	}
	if got, _ := backed.raw.GetMeta("agent-a", "GC_DRAIN_ACK"); got != "1" {
		t.Fatalf("GC_DRAIN_ACK = %q, want the local value persisted", got)
	}
	if got, _ := backed.raw.GetMeta("agent-a", "drained"); got != "1" {
		t.Fatalf("drained = %q, want the local drained marker persisted", got)
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
