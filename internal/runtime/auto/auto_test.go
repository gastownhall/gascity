package auto

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.Provider = (*Provider)(nil)

func TestRouteDefaultAndACP(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP)
	p.AddBackend("acp", acpSP)

	// Unregistered session routes to default.
	if got := p.route("agent-a"); got != defaultSP {
		t.Fatal("unregistered session should route to default")
	}

	// Register as ACP.
	p.RouteACP("agent-b")
	if got := p.route("agent-b"); got != acpSP {
		t.Fatal("registered session should route to ACP")
	}
	if got := p.route("agent-a"); got != defaultSP {
		t.Fatal("other session should still route to default")
	}
}

func TestUnroute(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP)
	p.AddBackend("acp", acpSP)

	p.RouteACP("agent-x")
	if got := p.route("agent-x"); got != acpSP {
		t.Fatal("should route to ACP after registration")
	}

	p.Unroute("agent-x")
	if got := p.route("agent-x"); got != defaultSP {
		t.Fatal("should route to default after unroute")
	}
}

func TestRouteToExecProvider(t *testing.T) {
	defaultSP := runtime.NewFake()
	headlessSP := runtime.NewFake()
	p := New(defaultSP)
	p.AddBackend("exec:scripts/gc-session-docker-headless", headlessSP)

	p.Route("worker-1", "exec:scripts/gc-session-docker-headless")
	if got := p.route("worker-1"); got != headlessSP {
		t.Fatal("worker-1 should route to headless provider")
	}
	if got := p.route("mayor"); got != defaultSP {
		t.Fatal("mayor should route to default provider")
	}
}

func TestMultipleBackends(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	headlessSP := runtime.NewFake()
	p := New(defaultSP)
	p.AddBackend("acp", acpSP)
	p.AddBackend("exec:headless", headlessSP)

	p.RouteACP("acp-agent")
	p.Route("headless-agent", "exec:headless")

	if got := p.route("acp-agent"); got != acpSP {
		t.Fatal("acp-agent should route to ACP")
	}
	if got := p.route("headless-agent"); got != headlessSP {
		t.Fatal("headless-agent should route to headless")
	}
	if got := p.route("default-agent"); got != defaultSP {
		t.Fatal("default-agent should route to default")
	}
}

func TestAttachDelegatesToRoutedBackend(t *testing.T) {
	defaultSP := runtime.NewFake()
	headlessSP := runtime.NewFake()
	p := New(defaultSP)
	p.AddBackend("exec:headless", headlessSP)

	p.Route("headless-agent", "exec:headless")

	// Default sessions with an existing session should not error.
	_ = defaultSP.Start(context.Background(), "normal-agent", runtime.Config{})
	if err := p.Attach("normal-agent"); err != nil {
		t.Errorf("Attach on default session should not error: %v", err)
	}
}

func TestListRunningMergesAllBackends(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	headlessSP := runtime.NewFake()
	p := New(defaultSP)
	p.AddBackend("acp", acpSP)
	p.AddBackend("exec:headless", headlessSP)

	// Start sessions on each backend.
	_ = defaultSP.Start(context.Background(), "default-1", runtime.Config{})
	_ = acpSP.Start(context.Background(), "acp-1", runtime.Config{})
	_ = headlessSP.Start(context.Background(), "headless-1", runtime.Config{})

	names, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("ListRunning returned %d names, want 3: %v", len(names), names)
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["default-1"] || !found["acp-1"] || !found["headless-1"] {
		t.Errorf("ListRunning = %v, want default-1, acp-1, and headless-1", names)
	}
}

func TestStopPreservesRouteOnAllFail(t *testing.T) {
	defaultSP := runtime.NewFailFake()
	acpSP := runtime.NewFailFake()
	p := New(defaultSP)
	p.AddBackend("acp", acpSP)

	p.RouteACP("agent-fail")
	err := p.Stop("agent-fail")
	if err == nil {
		t.Fatal("Stop should return error when all backends fail")
	}

	// Route should be preserved since Stop failed on all.
	if got := p.route("agent-fail"); got != acpSP {
		t.Fatal("route should be preserved when Stop fails on all backends")
	}
}

func TestIsRunningFallsThrough(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP)
	p.AddBackend("acp", acpSP)

	// Start on default backend but register route as ACP (simulates stale route).
	_ = defaultSP.Start(context.Background(), "stale-agent", runtime.Config{})
	p.RouteACP("stale-agent")

	// ACP says not running → should fall through to default → true.
	if !p.IsRunning("stale-agent") {
		t.Fatal("IsRunning should fall through to default when ACP reports not running")
	}

	// Reverse: start on ACP, don't register route (simulates lost route).
	_ = acpSP.Start(context.Background(), "lost-route", runtime.Config{})
	if !p.IsRunning("lost-route") {
		t.Fatal("IsRunning should fall through to ACP when default reports not running")
	}
}

func TestStopFallsThrough(t *testing.T) {
	defaultSP := runtime.NewFailFake() // Stop always fails (simulates "not found")
	acpSP := runtime.NewFake()
	p := New(defaultSP)
	p.AddBackend("acp", acpSP)

	// Start on ACP but don't register route (simulates lost route after restart).
	_ = acpSP.Start(context.Background(), "orphan", runtime.Config{})

	// Stop routes to default (no route entry), which fails → falls through to ACP.
	if err := p.Stop("orphan"); err != nil {
		t.Fatalf("Stop should fall through to ACP backend: %v", err)
	}
	if acpSP.IsRunning("orphan") {
		t.Fatal("session should be stopped on ACP backend after fallthrough")
	}
}

func TestStopCleansUpRoute(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP)
	p.AddBackend("acp", acpSP)

	p.RouteACP("agent-z")
	_ = acpSP.Start(context.Background(), "agent-z", runtime.Config{})

	if err := p.Stop("agent-z"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	// After stop, route entry should be cleaned up.
	if got := p.route("agent-z"); got != defaultSP {
		t.Fatal("route should fall back to default after Stop")
	}
}

func TestPendingAndRespondDelegateToRoutedBackend(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP)
	p.AddBackend("acp", acpSP)

	p.RouteACP("interactive-agent")
	_ = acpSP.Start(context.Background(), "interactive-agent", runtime.Config{})
	acpSP.SetPendingInteraction("interactive-agent", &runtime.PendingInteraction{RequestID: "req-1"})

	pending, err := p.Pending("interactive-agent")
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending == nil || pending.RequestID != "req-1" {
		t.Fatalf("Pending = %#v, want req-1", pending)
	}
	if err := p.Respond("interactive-agent", runtime.InteractionResponse{RequestID: "req-1", Action: "approve"}); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if got := acpSP.Responses["interactive-agent"]; len(got) != 1 || got[0].Action != "approve" {
		t.Fatalf("Responses = %#v, want single approve", got)
	}
}

func TestPendingUnsupportedWhenBackendLacksInteractionSupport(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := &runtimeNoInteractionProvider{Provider: runtime.NewFake()}
	p := New(defaultSP)
	p.AddBackend("acp", acpSP)

	p.RouteACP("plain-agent")

	_, err := p.Pending("plain-agent")
	if err != runtime.ErrInteractionUnsupported {
		t.Fatalf("Pending error = %v, want ErrInteractionUnsupported", err)
	}
}

func TestUnregisteredBackendRoutesToDefault(t *testing.T) {
	defaultSP := runtime.NewFake()
	p := New(defaultSP)

	// Route to a key that has no registered backend.
	p.Route("orphan-agent", "nonexistent-provider")

	if got := p.route("orphan-agent"); got != defaultSP {
		t.Fatal("unregistered backend key should fall back to default")
	}
}

type runtimeNoInteractionProvider struct {
	runtime.Provider
}
