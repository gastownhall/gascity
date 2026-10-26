package t3bridge

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestSessionBindingReportsTheBoundThread(t *testing.T) {
	server := newT3BridgeTestServer(t, boundThreadSnapshot("ready"))
	t.Cleanup(server.Close)
	_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
	bp, ok := sp.(runtime.SessionBindingProvider)
	if !ok {
		t.Fatalf("%T is not a runtime.SessionBindingProvider", sp)
	}
	got, err := bp.SessionBinding("agent-a")
	if err != nil {
		t.Fatalf("SessionBinding: %v", err)
	}
	if want := (runtime.SessionBinding{ThreadID: "thread-agent-a", ProjectID: "project-1"}); got != want {
		t.Fatalf("SessionBinding = %+v, want %+v", got, want)
	}
}

func TestSessionBindingWithoutThreadIsSessionNotFound(t *testing.T) {
	server := newT3BridgeTestServer(t, map[string]interface{}{"threads": []interface{}{}})
	t.Cleanup(server.Close)
	_, sp := newSeamBackedSnapshotTestProvider(t, server.wsURL())
	if _, err := sp.(runtime.SessionBindingProvider).SessionBinding("agent-a"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("SessionBinding = %v, want errors.Is(runtime.ErrSessionNotFound)", err)
	}
}

func TestSessionBindingSoftUnavailableIsRuntimeUnavailable(t *testing.T) {
	_, sp := newSeamBackedSnapshotTestProvider(t, unreachableBridgeURL)
	if _, err := sp.(runtime.SessionBindingProvider).SessionBinding("agent-a"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("SessionBinding = %v, want errors.Is(runtime.ErrRuntimeUnavailable)", err)
	}
}
