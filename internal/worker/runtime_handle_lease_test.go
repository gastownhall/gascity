package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestRuntimeHandleStopsUnderTheNamesFlock: a runtime-only handle stops only
// under the name's flock, never waiting under the controller's mode.
func TestRuntimeHandleStopsUnderTheNamesFlock(t *testing.T) {
	city := t.TempDir()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "legacy-1", runtime.Config{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	h, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "legacy-1"})
	if err != nil {
		t.Fatal(err)
	}
	cityDir, err := session.NewCityDir(city)
	if err != nil {
		t.Fatal(err)
	}
	controller := session.Actor{Kind: session.ActorController, City: cityDir}
	operator := session.Actor{Kind: session.ActorOperator, City: cityDir}
	held, err := session.TryRuntimeLease(nil, session.RuntimeLeaseRequest{City: city, Name: "legacy-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Kill(context.Background(), controller); !errors.Is(err, session.ErrRuntimeLeaseBusy) || !sp.IsRunning("legacy-1") {
		t.Fatalf("kill under a held flock = %v (running %v), want busy", err, sp.IsRunning("legacy-1"))
	}
	held.Release()
	if err := h.Kill(context.Background(), operator); err != nil || sp.IsRunning("legacy-1") {
		t.Fatalf("kill of a free name = %v (running %v)", err, sp.IsRunning("legacy-1"))
	}
}
