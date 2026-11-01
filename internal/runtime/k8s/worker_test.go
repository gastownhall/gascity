package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/guard"
)

// fakeWorkerTmux models one tmux server's environment.
func fakeWorkerTmux(alive *bool, env map[string]string) func(...string) (string, error) {
	return func(args ...string) (string, error) {
		if !*alive {
			return "", errors.New("no server running")
		}
		switch args[0] {
		case "has-session":
			return "", nil
		case "set-environment":
			if args[3] == "-u" {
				delete(env, args[4])
				return "", nil
			}
			env[args[3]] = args[4]
			return "", nil
		case "show-environment":
			val, ok := env[args[3]]
			if !ok {
				return "", errors.New("unknown variable")
			}
			return args[3] + "=" + val + "\n", nil
		}
		return "", errors.New("unexpected tmux command: " + strings.Join(args, " "))
	}
}

func newTestWorkerProvider(alive *bool, env map[string]string) *WorkerProvider {
	return &WorkerProvider{Provider: guard.New(), session: "own", tmux: fakeWorkerTmux(alive, env)}
}

func TestWorkerProviderRoundTripsOwnSessionMetadata(t *testing.T) {
	alive := true
	env := map[string]string{}
	p := newTestWorkerProvider(&alive, env)

	if !p.IsRunning("own") {
		t.Fatal("IsRunning(own) = false, want true")
	}
	if err := p.SetMeta("own", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}
	if got := env["GC_DRAIN_ACK"]; got != "1" {
		t.Fatalf("tmux env GC_DRAIN_ACK = %q, want 1", got)
	}
	if got, err := p.GetMeta("own", "GC_DRAIN_ACK"); err != nil || got != "1" {
		t.Fatalf("GetMeta = %q, %v; want 1, nil", got, err)
	}
	if err := p.RemoveMeta("own", "GC_DRAIN_ACK"); err != nil {
		t.Fatalf("RemoveMeta: %v", err)
	}
	if got, err := p.GetMeta("own", "GC_DRAIN_ACK"); err != nil || got != "" {
		t.Fatalf("GetMeta after remove = %q, %v; want empty, nil", got, err)
	}
}

// A gc command run inside the Pod resolves its own session through tmux, so it
// addresses the in-Pod session name rather than the controller-side one.
func TestWorkerProviderAcceptsInPodSessionName(t *testing.T) {
	alive := true
	env := map[string]string{}
	p := newTestWorkerProvider(&alive, env)

	if err := p.SetMeta(tmuxSession, "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("SetMeta(%q): %v", tmuxSession, err)
	}
	if got, err := p.GetMeta(tmuxSession, "GC_DRAIN_ACK"); err != nil || got != "1" {
		t.Fatalf("GetMeta(%q) = %q, %v; want 1, nil", tmuxSession, got, err)
	}
	if !p.IsRunning(tmuxSession) {
		t.Fatalf("IsRunning(%q) = false, want true", tmuxSession)
	}
}

func TestWorkerProviderReportsMissingSessionAsGone(t *testing.T) {
	alive := false
	p := newTestWorkerProvider(&alive, map[string]string{})

	if p.IsRunning("own") {
		t.Fatal("IsRunning with no tmux server = true, want false")
	}
	if err := p.SetMeta("own", "GC_DRAIN_ACK", "1"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("SetMeta error = %v, want ErrSessionNotFound", err)
	}
	if _, err := p.GetMeta("own", "GC_DRAIN_ACK"); !errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("GetMeta error = %v, want ErrSessionNotFound", err)
	}
}

func TestWorkerProviderRefusesOtherSessionsAndLifecycle(t *testing.T) {
	alive := true
	env := map[string]string{}
	p := newTestWorkerProvider(&alive, env)

	if p.IsRunning("other") {
		t.Fatal("IsRunning(other) = true, want false")
	}
	if err := p.SetMeta("other", "GC_DRAIN_ACK", "1"); !errors.Is(err, guard.ErrControllerOwnedRuntime) {
		t.Fatalf("SetMeta(other) error = %v, want ErrControllerOwnedRuntime", err)
	}
	if _, err := p.GetMeta("other", "GC_DRAIN_ACK"); !errors.Is(err, guard.ErrControllerOwnedRuntime) {
		t.Fatalf("GetMeta(other) error = %v, want ErrControllerOwnedRuntime", err)
	}
	if err := p.RemoveMeta("other", "GC_DRAIN_ACK"); !errors.Is(err, guard.ErrControllerOwnedRuntime) {
		t.Fatalf("RemoveMeta(other) error = %v, want ErrControllerOwnedRuntime", err)
	}
	if len(env) != 0 {
		t.Fatalf("refused writes reached tmux: %v", env)
	}
	if err := p.Stop("own"); !errors.Is(err, guard.ErrControllerOwnedRuntime) {
		t.Fatalf("Stop error = %v, want ErrControllerOwnedRuntime", err)
	}
	if err := p.Start(context.Background(), "own", runtime.Config{}); !errors.Is(err, guard.ErrControllerOwnedRuntime) {
		t.Fatalf("Start error = %v, want ErrControllerOwnedRuntime", err)
	}
}

func TestWorkerProviderWithNoSessionIdentityRefuses(t *testing.T) {
	alive := true
	p := &WorkerProvider{Provider: guard.New(), tmux: fakeWorkerTmux(&alive, map[string]string{})}
	if err := p.SetMeta("", "GC_DRAIN_ACK", "1"); !errors.Is(err, guard.ErrControllerOwnedRuntime) {
		t.Fatalf("SetMeta with no identity error = %v, want ErrControllerOwnedRuntime", err)
	}
}
