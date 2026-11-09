package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestRuntimeHandleAttachNeverStartsRuntime pins the contract gc session
// attach relies on to keep a viewer from owning a runtime's lifetime: a
// runtime-only handle attaches to a running runtime, and refuses a stopped
// one with ErrSessionInactive without starting it.
func TestRuntimeHandleAttachNeverStartsRuntime(t *testing.T) {
	sp := runtime.NewFake()
	h, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "viewer-1"})
	if err != nil {
		t.Fatal(err)
	}
	operator := session.Actor{Kind: session.ActorOperator}

	if err := h.Attach(context.Background(), operator); !errors.Is(err, session.ErrSessionInactive) {
		t.Fatalf("Attach(stopped) = %v, want ErrSessionInactive", err)
	}
	if n := sp.CountCalls("Start", "viewer-1"); n != 0 || sp.IsRunning("viewer-1") {
		t.Fatalf("Attach(stopped) started the runtime: Start calls = %d, running = %v", n, sp.IsRunning("viewer-1"))
	}
	if n := sp.CountCalls("Attach", "viewer-1"); n != 0 {
		t.Fatalf("Attach(stopped) reached the provider's Attach %d times, want 0", n)
	}

	if err := sp.Start(context.Background(), "viewer-1", runtime.Config{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := h.Attach(context.Background(), operator); err != nil {
		t.Fatalf("Attach(running) = %v", err)
	}
	if n := sp.CountCalls("Start", "viewer-1"); n != 1 {
		t.Fatalf("Start calls = %d, want only the test's own start", n)
	}
	if n := sp.CountCalls("Attach", "viewer-1"); n != 1 {
		t.Fatalf("provider Attach calls = %d, want 1", n)
	}
}
