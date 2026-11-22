package session

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

// TestManagerStartRuntimeOnly_RunningRuntimeReportsSessionExists is AC-2 of
// gastownhall/gascity#6417: the reconciler's runtime-only start must not
// answer nil over a runtime that is already up — its only caller is the
// controller's start, whose commit then recorded a launch that never happened
// and left the startup prompt stranded. A running runtime reports the
// collision instead, as a Start that hits an existing session does, and
// starts nothing; convergence is the caller's classification to decide.
func TestManagerStartRuntimeOnly_RunningRuntimeReportsSessionExists(t *testing.T) {
	sp := runtime.NewFake()
	mgr, id, sessName := seedAsleepSession(t, sp)
	if err := sp.Start(context.Background(), sessName, runtime.Config{}); err != nil {
		t.Fatalf("planting a running runtime: %v", err)
	}

	by := runtimeOnlyActor(t, mgr, id)
	inc := mintUnder(t, mgr, by.Lease, id)
	starts := sp.CountCalls("Start", sessName)

	err := mgr.StartRuntimeOnly(context.Background(), by, id, inc, "claude --dangerously", runtime.Config{WorkDir: "/tmp"})
	if err == nil || !errors.Is(err, runtime.ErrSessionExists) {
		t.Fatalf("StartRuntimeOnly over a running runtime: err = %v, want an error wrapping runtime.ErrSessionExists", err)
	}
	if got := sp.CountCalls("Start", sessName); got != starts {
		t.Fatalf("Start calls = %d, want unchanged at %d: a running runtime is left as it is", got, starts)
	}
}
