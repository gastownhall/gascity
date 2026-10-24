package runtime_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// Kills a default that steers: a runtime that never declared a steer mode
// (subprocess, ACP, every runtime before modes existed) must not be typed
// into mid-turn, and an unknown answer reads the same.
func TestSteerModeForDefaultsToNone(t *testing.T) {
	if got := runtime.SteerModeFor(keysOnlyProvider{runtime.NewFake()}, "s"); got != runtime.SteerNone {
		t.Fatalf("undeclared runtime steer mode = %q, want none", got)
	}
	if got := runtime.SteerModeFor(nil, "s"); got != runtime.SteerNone {
		t.Fatalf("nil runtime steer mode = %q, want none", got)
	}
	f := runtime.NewFake()
	if got := runtime.SteerModeFor(f, "s"); got != runtime.SteerKeystroke {
		t.Fatalf("fake's empty steer mode = %q, want keystroke (the fake types as tmux does)", got)
	}
	for _, mode := range []runtime.SteerMode{runtime.SteerNone, runtime.SteerKeystroke, runtime.SteerProtocol} {
		f.SteerModeValue = mode
		if got := runtime.SteerModeFor(f, "s"); got != mode {
			t.Fatalf("declared steer mode = %q, want %q", got, mode)
		}
	}
	f.SteerModeValue = "telepathy"
	if got := runtime.SteerModeFor(f, "s"); got != runtime.SteerNone {
		t.Fatalf("unknown declared steer mode = %q, want none", got)
	}
}

// Kills a composite that answers for itself instead of the session's routed
// backend: a backend that cannot steer behind auto or hybrid must not be
// steered because the other backend can.
func TestCompositesRouteSteerMode(t *testing.T) {
	keys := runtime.NewFake()
	none := keysOnlyProvider{runtime.NewFake()}

	a := auto.New(keys, none)
	a.RouteACP("routed")
	h := hybrid.New(keys, none, func(name string) bool { return name == "routed" })
	for name, c := range map[string]runtime.Provider{"auto": a, "hybrid": h} {
		if got := runtime.SteerModeFor(c, "routed"); got != runtime.SteerNone {
			t.Errorf("%s routed session steer mode = %q, want none", name, got)
		}
		if got := runtime.SteerModeFor(c, "other"); got != runtime.SteerKeystroke {
			t.Errorf("%s default session steer mode = %q, want keystroke", name, got)
		}
	}
}
