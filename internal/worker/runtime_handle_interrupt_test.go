package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

// TestRuntimeHandleInterruptFollowsTurnInterruptMode: a runtime-only handle
// (no session bead, e.g. the shutdown sweep's fallback by name) interrupts
// keystroke and protocol runtimes through Interrupt, and refuses a runtime
// that cannot interrupt the turn with the typed error, sending nothing,
// rather than reporting success.
func TestRuntimeHandleInterruptFollowsTurnInterruptMode(t *testing.T) {
	for _, tc := range []struct {
		mode          runtime.TurnInterruptMode
		wantErr       error
		wantInterrupt bool
	}{
		{mode: runtime.TurnInterruptKeystroke, wantInterrupt: true},
		{mode: runtime.TurnInterruptProtocol, wantInterrupt: true},
		{mode: runtime.TurnInterruptUnsupported, wantErr: session.ErrTurnInterruptUnsupported},
	} {
		t.Run(string(tc.mode), func(t *testing.T) {
			sp := runtime.NewFake()
			sp.TurnInterruptModeValue = tc.mode
			if err := sp.Start(context.Background(), "legacy-3", runtime.Config{Command: "x"}); err != nil {
				t.Fatal(err)
			}
			sp.Calls = nil
			h, err := NewRuntimeHandle(RuntimeHandleConfig{Provider: sp, SessionName: "legacy-3"})
			if err != nil {
				t.Fatal(err)
			}
			err = h.Interrupt(context.Background(), InterruptRequest{})
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("Interrupt = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("Interrupt: %v", err)
			}
			var sawInterrupt bool
			for _, call := range sp.Calls {
				sawInterrupt = sawInterrupt || call.Method == "Interrupt"
			}
			if sawInterrupt != tc.wantInterrupt {
				t.Fatalf("calls = %#v, want Interrupt=%v", sp.Calls, tc.wantInterrupt)
			}
		})
	}
}
