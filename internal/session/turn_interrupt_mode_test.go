package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// turnInterruptFamily is one provider family as the session bead records it.
type turnInterruptFamily struct {
	name     string
	provider string
	meta     map[string]string
}

// turnInterruptFamilies covers every interrupt shape internal/session picks
// by family: Interrupt + input clear (claude), soft Escape + boundary wait
// (codex), soft Escape (kimi), Interrupt + turn reset (gemini), soft Escape
// without idle wait (a wrapped gemini), hard restart (pi), and plain
// Interrupt (any other family).
var turnInterruptFamilies = []turnInterruptFamily{
	{name: "claude", provider: "claude"},
	{name: "codex", provider: "codex"},
	{name: "kimi", provider: "kimi"},
	{name: "gemini", provider: "gemini"},
	{name: "wrapped-gemini", provider: "my-gemini", meta: map[string]string{"builtin_ancestor": "gemini"}},
	{name: "pi", provider: "pi"},
	{name: "opencode", provider: "opencode"},
}

func newTurnInterruptSession(t *testing.T, fam turnInterruptFamily, mode runtime.TurnInterruptMode) (*Manager, *runtime.Fake, Info) {
	t.Helper()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	sp.TurnInterruptModeValue = mode
	mgr := newTestManager(t, store, sp)
	meta := map[string]string{"session_origin": "manual"}
	for k, v := range fam.meta {
		meta[k] = v
	}
	info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: fam.provider, WorkDir: t.TempDir(), Provider: fam.provider, ExtraMeta: meta})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	sp.Calls = nil
	return mgr, sp, info
}

func submitInterruptNow(mgr *Manager, info Info) error {
	_, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "msg", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	return err
}

// A runtime that drives a terminal keeps the exact provider keystroke
// sequence it had before turn-interrupt modes existed: these call sequences
// were recorded from the code before the change.
func TestKeystrokeTurnInterruptKeepsProviderSequences(t *testing.T) {
	golden := map[string]struct{ stop, now string }{
		"claude": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForIdle ClearInput",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle ClearInput IsRunning IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"codex": {
			stop: "IsRunning IsRunning IsRunning SendKeys(Escape) WaitForIdle WaitForInterruptBoundary",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning SendKeys(Escape) WaitForIdle WaitForInterruptBoundary IsRunning IsRunning IsRunning DismissKnownDialogs Pending NudgeNow(msg) DismissKnownDialogs",
		},
		"kimi": {
			stop: "IsRunning IsRunning IsRunning SendKeys(Escape) WaitForIdle",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning SendKeys(Escape) WaitForIdle IsRunning IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"gemini": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForIdle",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle ResetInterruptedTurn SendKeys(C-u) IsRunning IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"wrapped-gemini": {
			stop: "IsRunning IsRunning IsRunning SendKeys(Escape)",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning SendKeys(Escape) ResetInterruptedTurn SendKeys(C-u) IsRunning IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"pi": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning Stop IsRunning IsRunning FindRuntimesBySessionID Start IsRunning NudgeNow(msg)",
		},
		"opencode": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt IsRunning IsRunning IsRunning Pending NudgeNow(msg)",
		},
	}
	// The empty mode is what a runtime without the capability reports.
	for _, mode := range []runtime.TurnInterruptMode{"", runtime.TurnInterruptKeystroke} {
		for _, fam := range turnInterruptFamilies {
			want := golden[fam.name]
			t.Run(string(mode)+"/"+fam.name+"/stop", func(t *testing.T) {
				mgr, sp, info := newTurnInterruptSession(t, fam, mode)
				if err := mgr.StopTurn(info.ID); err != nil {
					t.Fatalf("StopTurn: %v", err)
				}
				if got := strings.Join(callMethods(sp.Calls), " "); got != want.stop {
					t.Fatalf("calls\n got: %s\nwant: %s", got, want.stop)
				}
			})
			t.Run(string(mode)+"/"+fam.name+"/interrupt_now", func(t *testing.T) {
				mgr, sp, info := newTurnInterruptSession(t, fam, mode)
				if err := submitInterruptNow(mgr, info); err != nil {
					t.Fatalf("Submit(interrupt_now): %v", err)
				}
				if got := strings.Join(callMethods(sp.Calls), " "); got != want.now {
					t.Fatalf("calls\n got: %s\nwant: %s", got, want.now)
				}
			})
		}
	}
}

// A runtime whose turns are protocol-driven drops keystrokes, so every family
// must be interrupted through Interrupt and no keys may be sent: a dropped
// Escape would leave the turn running while the caller is told it stopped.
func TestProtocolTurnInterruptUsesInterruptForEveryFamily(t *testing.T) {
	golden := map[string]struct{ stop, now string }{
		"claude": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForIdle",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle IsRunning IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"codex": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForIdle WaitForInterruptBoundary",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle WaitForInterruptBoundary IsRunning IsRunning IsRunning DismissKnownDialogs Pending NudgeNow(msg) DismissKnownDialogs",
		},
		"kimi": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForIdle",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle IsRunning IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"gemini": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForIdle",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle ResetInterruptedTurn IsRunning IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"wrapped-gemini": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt ResetInterruptedTurn IsRunning IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"pi": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
			// A protocol runtime cancels pi's turn itself: no hard restart,
			// whose Stop may leave a protocol-hosted runtime running.
			now: "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt IsRunning IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"opencode": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt IsRunning IsRunning IsRunning Pending NudgeNow(msg)",
		},
	}
	for _, fam := range turnInterruptFamilies {
		want := golden[fam.name]
		t.Run(fam.name+"/stop", func(t *testing.T) {
			mgr, sp, info := newTurnInterruptSession(t, fam, runtime.TurnInterruptProtocol)
			if err := mgr.StopTurn(info.ID); err != nil {
				t.Fatalf("StopTurn: %v", err)
			}
			if got := strings.Join(callMethods(sp.Calls), " "); got != want.stop {
				t.Fatalf("calls\n got: %s\nwant: %s", got, want.stop)
			}
		})
		t.Run(fam.name+"/interrupt_now", func(t *testing.T) {
			mgr, sp, info := newTurnInterruptSession(t, fam, runtime.TurnInterruptProtocol)
			if err := submitInterruptNow(mgr, info); err != nil {
				t.Fatalf("Submit(interrupt_now): %v", err)
			}
			if got := strings.Join(callMethods(sp.Calls), " "); got != want.now {
				t.Fatalf("calls\n got: %s\nwant: %s", got, want.now)
			}
		})
	}
}

// A runtime that can neither deliver keys nor interrupt a turn by protocol
// must refuse with a typed error, not report a stop it never sent, and must
// not deliver the "deliver now" message as if the turn had been interrupted.
func TestUnsupportedTurnInterruptReturnsTypedError(t *testing.T) {
	for _, fam := range turnInterruptFamilies {
		t.Run(fam.name+"/stop", func(t *testing.T) {
			mgr, sp, info := newTurnInterruptSession(t, fam, runtime.TurnInterruptUnsupported)
			err := mgr.StopTurn(info.ID)
			if !errors.Is(err, ErrTurnInterruptUnsupported) {
				t.Fatalf("StopTurn error = %v, want ErrTurnInterruptUnsupported", err)
			}
			assertNoInterruptSent(t, sp)
		})
		t.Run(fam.name+"/interrupt_now", func(t *testing.T) {
			mgr, sp, info := newTurnInterruptSession(t, fam, runtime.TurnInterruptUnsupported)
			err := submitInterruptNow(mgr, info)
			if fam.name == "pi" {
				// pi replaces the turn by a runtime restart, which needs
				// neither keys nor a protocol interrupt.
				if err != nil {
					t.Fatalf("Submit(interrupt_now): %v", err)
				}
				assertNoInterruptSent(t, sp)
				return
			}
			if !errors.Is(err, ErrTurnInterruptUnsupported) {
				t.Fatalf("Submit(interrupt_now) error = %v, want ErrTurnInterruptUnsupported", err)
			}
			assertNoInterruptSent(t, sp)
			if i := callIndex(sp.Calls, "NudgeNow", info.SessionName); i >= 0 {
				t.Fatalf("calls = %v, did not want the message delivered", callMethods(sp.Calls))
			}
		})
	}
}

// A session with no live runtime has no turn to interrupt, whatever the
// runtime's mode: StopTurn stays a no-op success.
func TestUnsupportedTurnInterruptIgnoresStoppedSession(t *testing.T) {
	mgr, sp, info := newTurnInterruptSession(t, turnInterruptFamilies[1], runtime.TurnInterruptUnsupported)
	if err := sp.Stop(info.SessionName); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := mgr.StopTurn(info.ID); err != nil {
		t.Fatalf("StopTurn on a stopped session: %v", err)
	}
}

func assertNoInterruptSent(t *testing.T, sp *runtime.Fake) {
	t.Helper()
	for _, call := range sp.Calls {
		if call.Method == "Interrupt" || call.Method == "SendKeys" || call.Method == "ClearInput" {
			t.Fatalf("calls = %v, want no interrupt, keys or input clear", callMethods(sp.Calls))
		}
	}
}
