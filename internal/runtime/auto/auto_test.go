package auto

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.Provider = (*Provider)(nil)

func TestRouteDefaultAndACP(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

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
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

	p.RouteACP("agent-x")
	if got := p.route("agent-x"); got != acpSP {
		t.Fatal("should route to ACP after registration")
	}

	p.Unroute("agent-x")
	if got := p.route("agent-x"); got != defaultSP {
		t.Fatal("should route to default after unroute")
	}
}

func TestAttachReturnsErrorForACP(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

	p.RouteACP("headless-agent")
	err := p.Attach("headless-agent")
	if err == nil {
		t.Fatal("Attach on ACP session should return error")
	}
	if want := `agent "headless-agent" uses acp transport (no terminal to attach to)`; err.Error() != want {
		t.Errorf("Attach error = %q, want %q", err.Error(), want)
	}

	// Default sessions with an existing session should not error.
	_ = defaultSP.Start(context.Background(), "normal-agent", runtime.Config{})
	if err := p.Attach("normal-agent"); err != nil {
		t.Errorf("Attach on default session should not error: %v", err)
	}
}

func TestListRunningMergesBothBackends(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

	// Start sessions on each backend.
	_ = defaultSP.Start(context.Background(), "default-1", runtime.Config{})
	_ = acpSP.Start(context.Background(), "acp-1", runtime.Config{})

	names, err := p.ListRunning("")
	if err != nil {
		t.Fatalf("ListRunning: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("ListRunning returned %d names, want 2: %v", len(names), names)
	}
	found := map[string]bool{}
	for _, n := range names {
		found[n] = true
	}
	if !found["default-1"] || !found["acp-1"] {
		t.Errorf("ListRunning = %v, want default-1 and acp-1", names)
	}
}

func TestStopPreservesRouteOnBothFail(t *testing.T) {
	defaultSP := runtime.NewFailFake() // both backends fail
	acpSP := runtime.NewFailFake()
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

	p.RouteACP("agent-fail")
	err := p.Stop("agent-fail")
	if err == nil {
		t.Fatal("Stop should return error when both backends fail")
	}

	// Route should be preserved since Stop failed on both.
	if got := p.route("agent-fail"); got != acpSP {
		t.Fatal("route should be preserved when Stop fails on both backends")
	}
}

func TestListRunningPartialError(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFailFake() // ListRunning returns error
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

	_ = defaultSP.Start(context.Background(), "default-1", runtime.Config{})

	names, err := p.ListRunning("")
	if err == nil {
		t.Fatal("ListRunning should return error when one backend fails")
	}
	// Should still return partial results from the working backend.
	if len(names) != 1 || names[0] != "default-1" {
		t.Errorf("ListRunning partial = %v, want [default-1]", names)
	}
}

func TestListRunningBothFail(t *testing.T) {
	defaultSP := runtime.NewFailFake()
	acpSP := runtime.NewFailFake()
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

	names, err := p.ListRunning("")
	if err == nil {
		t.Fatal("ListRunning should return error when both backends fail")
	}
	if names != nil {
		t.Errorf("ListRunning both fail = %v, want nil", names)
	}
}

func TestIsRunningFallsThrough(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

	// Start on default backend but register route as ACP (simulates stale route).
	_ = defaultSP.Start(context.Background(), "stale-agent", runtime.Config{})
	p.RouteACP("stale-agent")

	// ACP says not running -> should fall through to default -> true.
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
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

	// Start on ACP but don't register route (simulates lost route after restart).
	_ = acpSP.Start(context.Background(), "orphan", runtime.Config{})

	// Stop routes to default (no route entry), which fails -> falls through to ACP.
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
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

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
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

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
	p := New(defaultSP, map[string]runtime.Provider{"acp": acpSP})

	p.RouteACP("plain-agent")

	_, err := p.Pending("plain-agent")
	if !errors.Is(err, runtime.ErrInteractionUnsupported) {
		t.Fatalf("Pending error = %v, want ErrInteractionUnsupported", err)
	}
}

// TestNBackendRouting verifies that sessions can be routed to different
// named backends (not just ACP).
func TestNBackendRouting(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	execSP := runtime.NewFake()
	subprocessSP := runtime.NewFake()

	p := New(defaultSP, map[string]runtime.Provider{
		"acp":        acpSP,
		"exec:myscr": execSP,
		"subprocess": subprocessSP,
	})

	// Route different agents to different backends.
	p.Route("agent-acp", "acp")
	p.Route("agent-exec", "exec:myscr")
	p.Route("agent-sub", "subprocess")

	if got := p.route("agent-acp"); got != acpSP {
		t.Fatal("agent-acp should route to ACP backend")
	}
	if got := p.route("agent-exec"); got != execSP {
		t.Fatal("agent-exec should route to exec backend")
	}
	if got := p.route("agent-sub"); got != subprocessSP {
		t.Fatal("agent-sub should route to subprocess backend")
	}
	if got := p.route("agent-default"); got != defaultSP {
		t.Fatal("unregistered agent should route to default")
	}
}

// TestAttachReturnsErrorForNonAttachableBackends verifies that Attach
// returns an error for non-tmux providers like subprocess and exec.
func TestAttachReturnsErrorForNonAttachableBackends(t *testing.T) {
	defaultSP := runtime.NewFake()
	subSP := runtime.NewFake()
	p := New(defaultSP, map[string]runtime.Provider{"subprocess": subSP})

	p.Route("headless", "subprocess")
	err := p.Attach("headless")
	if err == nil {
		t.Fatal("Attach on subprocess session should return error")
	}

	// tmux-routed sessions should be attachable.
	tmuxSP := runtime.NewFake()
	p2 := New(defaultSP, map[string]runtime.Provider{"tmux": tmuxSP})
	_ = tmuxSP.Start(context.Background(), "tmux-agent", runtime.Config{})
	p2.Route("tmux-agent", "tmux")
	if err := p2.Attach("tmux-agent"); err != nil {
		t.Errorf("Attach on tmux-routed session should not error: %v", err)
	}
}

// TestListRunningMergesAllBackends verifies that ListRunning queries
// all backends, not just default and one override.
func TestListRunningMergesAllBackends(t *testing.T) {
	defaultSP := runtime.NewFake()
	acpSP := runtime.NewFake()
	execSP := runtime.NewFake()

	p := New(defaultSP, map[string]runtime.Provider{
		"acp":        acpSP,
		"exec:myscr": execSP,
	})

	_ = defaultSP.Start(context.Background(), "def-1", runtime.Config{})
	_ = acpSP.Start(context.Background(), "acp-1", runtime.Config{})
	_ = execSP.Start(context.Background(), "exec-1", runtime.Config{})

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
	if !found["def-1"] || !found["acp-1"] || !found["exec-1"] {
		t.Errorf("ListRunning = %v, want def-1, acp-1, and exec-1", names)
	}
}

type runtimeNoInteractionProvider struct {
	runtime.Provider
}
