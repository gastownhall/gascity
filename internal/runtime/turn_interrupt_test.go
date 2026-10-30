package runtime_test

import (
	"errors"
	"fmt"
	"strings"
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

// Kills a not-current error that callers cannot classify or read: it must
// match ErrTurnNotCurrent through any wrapping, be reachable with errors.As,
// and name the session, the turn asked for and the turn read instead.
func TestTurnNotCurrentErrorIsAndAs(t *testing.T) {
	var err error = &runtime.TurnNotCurrentError{Session: "sess-a", TurnID: "thread/t1", Current: runtime.TurnState{TurnID: "thread/t2"}}
	wrapped := fmt.Errorf("interrupting: %w", err)
	if !errors.Is(wrapped, runtime.ErrTurnNotCurrent) {
		t.Fatalf("errors.Is(%v, ErrTurnNotCurrent) = false", wrapped)
	}
	if errors.Is(wrapped, runtime.ErrTurnNotEnded) {
		t.Fatalf("a not-current error matches ErrTurnNotEnded")
	}
	var nc *runtime.TurnNotCurrentError
	if !errors.As(wrapped, &nc) || nc.Current.TurnID != "thread/t2" {
		t.Fatalf("errors.As = %+v, want the error carrying the current turn", nc)
	}
	for _, want := range []string{"sess-a", "thread/t1", "thread/t2"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Error() = %q, want it to name %q", err.Error(), want)
		}
	}
	idle := &runtime.TurnNotCurrentError{Session: "sess-a", TurnID: "thread/t1"}
	if msg := idle.Error(); !strings.Contains(msg, "sess-a") || !strings.Contains(msg, "thread/t1") || !strings.Contains(msg, "no turn") {
		t.Errorf("idle Error() = %q, want the session, the turn and that no turn runs", msg)
	}
}

// Kills an ErrTurnNotEnded that collides with another sentinel, and an Active
// that reads an empty turn as running.
func TestTurnNotEndedAndActive(t *testing.T) {
	if errors.Is(runtime.ErrTurnNotEnded, runtime.ErrTurnNotCurrent) || errors.Is(runtime.ErrTurnNotEnded, runtime.ErrRuntimeUnavailable) {
		t.Fatal("ErrTurnNotEnded matches another sentinel")
	}
	joined := errors.Join(runtime.ErrTurnNotEnded, runtime.ErrRuntimeUnavailable)
	if !errors.Is(joined, runtime.ErrTurnNotEnded) {
		t.Fatal("a joined ErrTurnNotEnded is not matched")
	}
	if (runtime.TurnState{}).Active() {
		t.Fatal("an empty turn state is active")
	}
	if !(runtime.TurnState{TurnID: "thread/t1"}).Active() {
		t.Fatal("a turn state naming a turn is not active")
	}
}
