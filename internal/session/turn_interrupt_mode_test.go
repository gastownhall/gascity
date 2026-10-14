package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
)

// turnInterruptFamily is one provider family as the session bead records it.
type turnInterruptFamily struct {
	name     string
	provider string
	meta     map[string]string
	// refusesInterruptNow marks a family that cannot take "deliver now" in
	// any runtime mode: Submit refuses it with ErrInteractionUnsupported.
	refusesInterruptNow bool
}

// turnInterruptFamilies covers every interrupt shape internal/session picks
// by family: Interrupt + input clear (claude), soft Escape + boundary wait
// (codex), soft Escape + boundary wait without idle wait (a wrapped codex),
// soft Escape (kimi), Interrupt + turn reset (gemini), soft Escape without
// idle wait (a wrapped gemini), hard restart (pi), plain Interrupt (any other
// family), and no "deliver now" at all (antigravity).
var turnInterruptFamilies = []turnInterruptFamily{
	{name: "claude", provider: "claude"},
	{name: "codex", provider: "codex"},
	{name: "wrapped-codex", provider: "my-codex", meta: map[string]string{"builtin_ancestor": "codex"}},
	{name: "kimi", provider: "kimi"},
	{name: "gemini", provider: "gemini"},
	{name: "wrapped-gemini", provider: "my-gemini", meta: map[string]string{"builtin_ancestor": "gemini"}},
	{name: "pi", provider: "pi"},
	{name: "opencode", provider: "opencode"},
	{name: "antigravity", provider: "antigravity", refusesInterruptNow: true},
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

// checkInterruptNow submits "deliver now" and compares the runtime calls with
// want; a family that refuses "deliver now" must refuse it without sending
// anything.
func checkInterruptNow(t *testing.T, fam turnInterruptFamily, mgr *Manager, sp *runtime.Fake, info Info, want string) {
	t.Helper()
	err := submitInterruptNow(mgr, info)
	if fam.refusesInterruptNow {
		if !errors.Is(err, ErrInteractionUnsupported) {
			t.Fatalf("Submit(interrupt_now) error = %v, want ErrInteractionUnsupported", err)
		}
		assertNoInterruptSent(t, sp)
		return
	}
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if got := strings.Join(callMethods(sp.Calls), " "); got != want {
		t.Fatalf("calls\n got: %s\nwant: %s", got, want)
	}
}

// A runtime that drives a terminal keeps the exact provider keystroke
// sequence it had before turn-interrupt modes existed: these call sequences
// were recorded from the code before the change. (A delivery's two cached
// IsRunning reads after its fresh read went with mc-ir8qp: whether the
// runtime runs is the fresh read's.)
func TestKeystrokeTurnInterruptKeepsProviderSequences(t *testing.T) {
	golden := map[string]struct{ stop, now string }{
		"claude": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForIdle ClearInput",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle ClearInput IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"codex": {
			stop: "IsRunning IsRunning IsRunning SendKeys(Escape) WaitForIdle WaitForInterruptBoundary",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning SendKeys(Escape) WaitForIdle WaitForInterruptBoundary IsRunning IsRunning DismissKnownDialogs Pending NudgeNow(msg) DismissKnownDialogs",
		},
		"wrapped-codex": {
			stop: "IsRunning IsRunning IsRunning SendKeys(Escape) WaitForInterruptBoundary",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning SendKeys(Escape) WaitForInterruptBoundary IsRunning IsRunning DismissKnownDialogs Pending NudgeNow(msg) DismissKnownDialogs",
		},
		"kimi": {
			stop: "IsRunning IsRunning IsRunning SendKeys(Escape) WaitForIdle",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning SendKeys(Escape) WaitForIdle IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"gemini": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForIdle",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle ResetInterruptedTurn SendKeys(C-u) IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"wrapped-gemini": {
			stop: "IsRunning IsRunning IsRunning SendKeys(Escape)",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning SendKeys(Escape) ResetInterruptedTurn SendKeys(C-u) IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"pi": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning Stop IsRunning IsRunning FindRuntimesBySessionID Start IsRunning NudgeNow(msg)",
		},
		"opencode": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"antigravity": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
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
				checkInterruptNow(t, fam, mgr, sp, info, want.now)
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
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"codex": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForIdle WaitForInterruptBoundary",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle WaitForInterruptBoundary IsRunning IsRunning DismissKnownDialogs Pending NudgeNow(msg) DismissKnownDialogs",
		},
		"wrapped-codex": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForInterruptBoundary",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForInterruptBoundary IsRunning IsRunning DismissKnownDialogs Pending NudgeNow(msg) DismissKnownDialogs",
		},
		"kimi": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForIdle",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"gemini": {
			stop: "IsRunning IsRunning IsRunning Interrupt WaitForIdle",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt WaitForIdle ResetInterruptedTurn IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"wrapped-gemini": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt ResetInterruptedTurn IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"pi": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
			// A protocol runtime cancels pi's turn itself: no hard restart,
			// whose Stop may leave a protocol-hosted runtime running.
			now: "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"opencode": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
			now:  "IsRunning IsRunning IsRunning Pending IsRunning IsRunning Interrupt IsRunning IsRunning Pending NudgeNow(msg)",
		},
		"antigravity": {
			stop: "IsRunning IsRunning IsRunning Interrupt",
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
			checkInterruptNow(t, fam, mgr, sp, info, want.now)
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
			if fam.refusesInterruptNow {
				if !errors.Is(err, ErrInteractionUnsupported) {
					t.Fatalf("Submit(interrupt_now) error = %v, want ErrInteractionUnsupported", err)
				}
				assertNoInterruptSent(t, sp)
				return
			}
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

// A protocol interrupt is sent once: when the turn does not go idle, there is
// no keystroke Ctrl-C fallback to retry with, and a second protocol interrupt
// could cancel a turn that started in between. StopTurn reports the idle wait
// failure; interrupt_now falls back to the hard restart.
func TestProtocolTurnInterruptSendsOneInterruptWhenIdleWaitFails(t *testing.T) {
	for _, fam := range turnInterruptFamilies {
		if fam.name != "codex" && fam.name != "kimi" {
			continue
		}
		t.Run(fam.name+"/stop", func(t *testing.T) {
			mgr, sp, info := newTurnInterruptSession(t, fam, runtime.TurnInterruptProtocol)
			sp.WaitForIdleSequence[info.SessionName] = []error{errors.New("not idle"), nil}
			err := mgr.StopTurn(info.ID)
			if err == nil || !strings.Contains(err.Error(), "waiting for stopped session to become idle") {
				t.Fatalf("StopTurn error = %v, want the idle wait failure", err)
			}
			if got := countMethod(sp.Calls, "Interrupt"); got != 1 {
				t.Fatalf("Interrupt calls = %d, want 1; calls = %v", got, callMethods(sp.Calls))
			}
		})
		t.Run(fam.name+"/interrupt_now", func(t *testing.T) {
			mgr, sp, info := newTurnInterruptSession(t, fam, runtime.TurnInterruptProtocol)
			sp.WaitForIdleSequence[info.SessionName] = []error{errors.New("not idle"), nil}
			err := submitInterruptNow(mgr, info)
			if err == nil || !strings.Contains(err.Error(), "waiting for interrupted session to become idle") {
				t.Fatalf("Submit(interrupt_now) error = %v, want the idle wait failure", err)
			}
			assertProtocolInterruptOnly(t, sp)
		})
	}
}

// A protocol runtime's Stop may leave the runtime running (t3bridge
// persistent threads), so when the turn does not settle after the protocol
// Interrupt, "deliver now" reports it instead of a stop-and-restart that
// would interrupt nothing and nudge the message into the running turn.
func TestProtocolInterruptNowReportsUnsettledTurnWithoutRestart(t *testing.T) {
	codex := turnInterruptFamilies[1]
	mgr, sp, info := newTurnInterruptSession(t, codex, runtime.TurnInterruptProtocol)
	sp.InterruptBoundaryErrors[info.SessionName] = errors.New("no boundary")
	err := submitInterruptNow(mgr, info)
	if err == nil || !strings.Contains(err.Error(), "waiting for interrupt boundary") {
		t.Fatalf("Submit(interrupt_now) error = %v, want the boundary wait failure", err)
	}
	assertProtocolInterruptOnly(t, sp)
}

// assertProtocolInterruptOnly checks that exactly one Interrupt reached the
// runtime and that it was neither stopped, restarted nor sent the message.
func assertProtocolInterruptOnly(t *testing.T, sp *runtime.Fake) {
	t.Helper()
	if got := countMethod(sp.Calls, "Interrupt"); got != 1 {
		t.Fatalf("Interrupt calls = %d, want 1; calls = %v", got, callMethods(sp.Calls))
	}
	for _, method := range []string{"Stop", "Start", "Nudge", "NudgeNow", "SendKeys"} {
		if n := countMethod(sp.Calls, method); n != 0 {
			t.Fatalf("calls = %v, want no %s", callMethods(sp.Calls), method)
		}
	}
}

func countMethod(calls []runtime.Call, method string) int {
	n := 0
	for _, call := range calls {
		if call.Method == method {
			n++
		}
	}
	return n
}

// The production t3bridge runtime, bare and behind auto as cmd/gc wires it,
// interrupts every family that takes "deliver now" with one Interrupt and
// then delivers the message: no keys, no waits it does not implement, no
// hard restart.
func TestT3BridgeProfileTurnInterruptSequence(t *testing.T) {
	providers := map[string]func(*runtime.Fake) runtime.Provider{
		"t3bridge": func(f *runtime.Fake) runtime.Provider {
			return runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: f})
		},
		"auto": func(f *runtime.Fake) runtime.Provider {
			return auto.New(
				runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: f}),
				runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: runtime.NewFake()}),
			)
		},
	}
	for pname, wrap := range providers {
		for _, fam := range turnInterruptFamilies {
			if fam.refusesInterruptNow {
				continue
			}
			t.Run(pname+"/"+fam.name, func(t *testing.T) {
				f := runtime.NewFake()
				mgr := newTestManager(t, beads.NewMemStore(), wrap(f))
				meta := map[string]string{"session_origin": "manual"}
				for k, v := range fam.meta {
					meta[k] = v
				}
				info, err := mgr.CreateSession(context.Background(), CreateOptions{Template: "helper", Command: fam.provider, WorkDir: t.TempDir(), Provider: fam.provider, ExtraMeta: meta})
				if err != nil {
					t.Fatalf("Create: %v", err)
				}
				f.Calls = nil
				if err := submitInterruptNow(mgr, info); err != nil {
					t.Fatalf("Submit(interrupt_now): %v", err)
				}
				if got := strings.Join(turnEffectCalls(f.Calls), " "); got != "Interrupt Nudge(msg)" {
					t.Fatalf("turn-affecting calls = %q, want %q; all calls = %v", got, "Interrupt Nudge(msg)", callMethods(f.Calls))
				}
			})
		}
	}
}

// turnEffectCalls keeps the calls that act on the agent's turn or runtime,
// dropping read-only probes.
func turnEffectCalls(calls []runtime.Call) []string {
	var out []string
	for i, m := range callMethods(calls) {
		switch calls[i].Method {
		case "IsRunning", "Pending", "ProcessAlive", "GetMeta", "SetMeta", "ObserveLivenessWithError", "Capabilities", "DismissKnownDialogs":
			continue
		}
		out = append(out, m)
	}
	return out
}

// An unsupported runtime refuses "deliver now" before it takes the runtime
// lease: a busy lease must not hide the real reason behind ErrSessionStarting.
func TestUnsupportedTurnInterruptRefusesBeforeRuntimeLease(t *testing.T) {
	mgr, sp, info := newTurnInterruptSession(t, turnInterruptFamilies[1], runtime.TurnInterruptUnsupported)
	held, err := TryRuntimeLease(nil, RuntimeLeaseRequest{City: mgr.cityPath, Name: info.SessionName})
	if err != nil {
		t.Fatalf("holding the runtime lease: %v", err)
	}
	defer held.Release()
	err = submitInterruptNow(mgr, info)
	if !errors.Is(err, ErrTurnInterruptUnsupported) {
		t.Fatalf("Submit(interrupt_now) under a held lease = %v, want ErrTurnInterruptUnsupported", err)
	}
	assertNoInterruptSent(t, sp)
}

// A session advertises "deliver now" only when its runtime can carry it out:
// an unsupported runtime would accept the intent and then fail it, except for
// pi, which replaces the turn by restarting the runtime.
func TestSubmissionCapabilitiesFollowTurnInterruptMode(t *testing.T) {
	for _, tc := range []struct {
		fam  turnInterruptFamily
		mode runtime.TurnInterruptMode
		want bool
	}{
		{turnInterruptFamilies[1], runtime.TurnInterruptKeystroke, true},
		{turnInterruptFamilies[1], runtime.TurnInterruptProtocol, true},
		{turnInterruptFamilies[1], runtime.TurnInterruptUnsupported, false},
		{turnInterruptFamily{name: "pi", provider: "pi"}, runtime.TurnInterruptUnsupported, true},
	} {
		t.Run(tc.fam.name+"/"+string(tc.mode), func(t *testing.T) {
			mgr, _, info := newTurnInterruptSession(t, tc.fam, tc.mode)
			caps, err := mgr.SubmissionCapabilities(info.ID)
			if err != nil {
				t.Fatalf("SubmissionCapabilities: %v", err)
			}
			if caps.SupportsInterruptNow != tc.want {
				t.Fatalf("SupportsInterruptNow = %v, want %v", caps.SupportsInterruptNow, tc.want)
			}
		})
	}
}
