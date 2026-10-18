package session

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/auto"
	"github.com/gastownhall/gascity/internal/runtime/hybrid"
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
// runtime runs is the fresh read's. pi's restart reads the runtime fresh
// once more under its lease before it mints (I-MINT): a start never mints
// over a live runtime.)
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
			now:  "IsRunning IsRunning IsRunning Pending IsRunning Stop IsRunning IsRunning IsRunning FindRuntimesBySessionID Start IsRunning NudgeNow(msg)",
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

// keystrokeComposites route a keystroke Fake through each composite cmd/gc
// wraps the base runtime in: auto whenever a city has ACP targets, hybrid for
// a local/remote split (its other leg a t3bridge runtime, on either side).
// Every composite answers runtime.TurnFenceProvider, whatever it routes to.
var keystrokeComposites = map[string]func(*runtime.Fake) runtime.Provider{
	"auto": func(f *runtime.Fake) runtime.Provider {
		return auto.New(f, runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: runtime.NewFake()}))
	},
	"hybrid-local": func(f *runtime.Fake) runtime.Provider {
		return hybrid.New(f, runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: runtime.NewFake()}), func(string) bool { return false })
	},
	"hybrid-remote": func(f *runtime.Fake) runtime.Provider {
		return hybrid.New(runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: runtime.NewFake()}), f, func(string) bool { return true })
	},
}

// interruptNowEffects runs "deliver now" for fam on a keystroke Fake wrapped
// by wrap and returns its turn-affecting calls, after checking that it took
// the unfenced path: no turn report, nothing queued.
func interruptNowEffects(t *testing.T, fam turnInterruptFamily, mode runtime.TurnInterruptMode, wrap func(*runtime.Fake) runtime.Provider) string {
	t.Helper()
	f := runtime.NewFake()
	f.TurnInterruptModeValue = mode
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
	out, err := mgr.Submit(context.Background(), testActor(mgr, ActorOperator), info.ID, "msg", BuildResumeCommand(info), runtime.Config{WorkDir: info.WorkDir}, SubmitIntentInterruptNow)
	if err != nil {
		t.Fatalf("Submit(interrupt_now): %v", err)
	}
	if out.Interrupt != nil || out.Queued || out.Deferred {
		t.Fatalf("outcome %+v (report %+v), want the unfenced path: no report, nothing queued", out, out.Interrupt)
	}
	state, err := nudgequeue.LoadState(mgr.cityPath)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if len(state.Pending) != 0 {
		t.Fatalf("pending %+v, want nothing queued", state.Pending)
	}
	var effects []string
	for _, c := range turnEffectCalls(f.Calls) {
		// A process-table scan is a read; composites answer it from their
		// own legs.
		if c != "FindRuntimesBySessionID" {
			effects = append(effects, c)
		}
	}
	return strings.Join(effects, " ")
}

// A keystroke runtime behind auto or hybrid, which always answer
// runtime.TurnFenceProvider, keeps its bare provider sequence for "deliver
// now": the turn fence is for protocol-mode sessions only
// (SESSION-RUNTIME-020), so the composite's capability must not turn
// the interrupt into a turn read that the keystroke backend cannot answer.
func TestKeystrokeTurnInterruptBehindCompositesKeepsProviderSequences(t *testing.T) {
	bare := func(f *runtime.Fake) runtime.Provider { return f }
	for _, mode := range []runtime.TurnInterruptMode{"", runtime.TurnInterruptKeystroke} {
		for _, fam := range turnInterruptFamilies {
			if fam.refusesInterruptNow {
				continue
			}
			for cname, wrap := range keystrokeComposites {
				t.Run(string(mode)+"/"+fam.name+"/"+cname, func(t *testing.T) {
					want := interruptNowEffects(t, fam, mode, bare)
					if !strings.HasSuffix(want, " NudgeNow(msg)") {
						t.Fatalf("bare effects %q are not an interrupt then the message", want)
					}
					if got := interruptNowEffects(t, fam, mode, wrap); got != want {
						t.Fatalf("turn-affecting calls\n got: %s\nwant: %s (bare)", got, want)
					}
				})
			}
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

// The production t3bridge runtime, bare and behind auto or hybrid as cmd/gc
// wires it, fences "deliver now" to the turn it arrived during for every
// family that takes it (SESSION-RUNTIME-020): an idle session gets the
// message alone; a running turn gets one InterruptTurn naming it, then the
// message once it ended. No keys, no untargeted Interrupt, no hard restart.
func TestT3BridgeProfileTurnInterruptSequence(t *testing.T) {
	for pname, wrap := range t3bridgeComposites {
		for _, fam := range turnInterruptFamilies {
			if fam.refusesInterruptNow {
				continue
			}
			for _, tc := range []struct {
				name   string
				script []string
				want   string
			}{
				{"idle", nil, "Nudge(msg)"},
				{"running", []string{"t1", "t1", ""}, "InterruptTurn Nudge(msg)"},
			} {
				t.Run(pname+"/"+fam.name+"/"+tc.name, func(t *testing.T) {
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
					f.TurnScripts = map[string][]string{info.SessionName: tc.script}
					if err := submitInterruptNow(mgr, info); err != nil {
						t.Fatalf("Submit(interrupt_now): %v", err)
					}
					if got := strings.Join(turnEffectCalls(f.Calls), " "); got != tc.want {
						t.Fatalf("turn-affecting calls = %q, want %q; all calls = %v", got, tc.want, callMethods(f.Calls))
					}
				})
			}
		}
	}
}

// t3bridgeComposites wrap a FullFake as the t3bridge runtime, bare and behind
// each composite cmd/gc routes it through.
var t3bridgeComposites = map[string]func(*runtime.Fake) runtime.Provider{
	"t3bridge": func(f *runtime.Fake) runtime.Provider {
		return runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: f})
	},
	"auto": func(f *runtime.Fake) runtime.Provider {
		return auto.New(
			runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: f}),
			runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: runtime.NewFake()}),
		)
	},
	"hybrid": func(f *runtime.Fake) runtime.Provider {
		return hybrid.New(
			runtime.NewFake(),
			runtime.NewFakeProfile(runtime.ProfileT3Bridge, runtime.FullFake{Fake: f}),
			func(string) bool { return true },
		)
	},
}

// turnEffectCalls keeps the calls that act on the agent's turn or runtime,
// dropping read-only probes.
func turnEffectCalls(calls []runtime.Call) []string {
	var out []string
	for i, m := range callMethods(calls) {
		switch calls[i].Method {
		case "IsRunning", "Pending", "ProcessAlive", "GetMeta", "SetMeta", "ObserveLivenessWithError", "Capabilities", "DismissKnownDialogs",
			"ObserveTurn", "WaitTurnEnded":
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
