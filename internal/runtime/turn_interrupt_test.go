package runtime_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
)

// keysOnlyProvider is a Provider with no turn-interrupt capability.
type keysOnlyProvider struct{ runtime.Provider }

// Kills a default that is not keystroke: a runtime that never declared a mode
// (tmux, and every runtime before modes existed) must keep its keystroke
// interrupts, and an empty answer reads the same.
func TestTurnInterruptModeForDefaultsToKeystroke(t *testing.T) {
	if got := runtime.TurnInterruptModeFor(keysOnlyProvider{runtime.NewFake()}, "s"); got != runtime.TurnInterruptKeystroke {
		t.Fatalf("undeclared runtime mode = %q, want keystroke", got)
	}
	f := runtime.NewFake()
	if got := runtime.TurnInterruptModeFor(f, "s"); got != runtime.TurnInterruptKeystroke {
		t.Fatalf("empty declared mode = %q, want keystroke", got)
	}
	for _, mode := range []runtime.TurnInterruptMode{runtime.TurnInterruptProtocol, runtime.TurnInterruptUnsupported} {
		f.TurnInterruptModeValue = mode
		if got := runtime.TurnInterruptModeFor(f, "s"); got != mode {
			t.Fatalf("declared mode = %q, want %q", got, mode)
		}
	}
	// A mode this build does not know reads as the default, so every caller
	// classifies it the same way instead of each picking its own fallback.
	f.TurnInterruptModeValue = "telepathy"
	if got := runtime.TurnInterruptModeFor(f, "s"); got != runtime.TurnInterruptKeystroke {
		t.Fatalf("unknown declared mode = %q, want keystroke", got)
	}
}

// Kills a composite that answers for itself instead of the session's routed
// backend: a protocol-interrupt backend behind auto or hybrid must still be
// interrupted by protocol, and a keystroke backend beside it by keys.
func TestCompositesRouteTurnInterruptMode(t *testing.T) {
	keys := keysOnlyProvider{runtime.NewFake()}
	protocol := runtime.NewFake()
	protocol.TurnInterruptModeValue = runtime.TurnInterruptProtocol

	a := auto.New(keys, protocol)
	a.RouteACP("routed")
	h := hybrid.New(keys, protocol, func(name string) bool { return name == "routed" })
	for name, c := range map[string]runtime.Provider{"auto": a, "hybrid": h} {
		if got := runtime.TurnInterruptModeFor(c, "routed"); got != runtime.TurnInterruptProtocol {
			t.Errorf("%s routed session mode = %q, want protocol", name, got)
		}
		if got := runtime.TurnInterruptModeFor(c, "other"); got != runtime.TurnInterruptKeystroke {
			t.Errorf("%s default session mode = %q, want keystroke", name, got)
		}
	}
}
