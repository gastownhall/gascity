package worker

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The turn-scoped interrupt (X11 §5.2, SESSION-RUNTIME-023): a stale target
// sends nothing, a current one sends the family's sequence, the settle is
// observed from the source, and End none refuses.

// inputMethods are the runtime calls that type into or interrupt a session,
// or interrupt one of its turns by protocol.
var inputMethods = map[string]bool{
	"Nudge": true, "NudgeNow": true, "SendKeys": true, "Interrupt": true, "ClearInput": true,
	"InterruptTurn": true, "WaitTurnEnded": true,
}

// sentInputs is the recorder: every input call to name, keys and the
// InterruptTurn outcome included.
func sentInputs(fake *runtime.Fake, name string) []string {
	var out []string
	for _, c := range fake.SnapshotCalls() {
		if c.Name != name || !inputMethods[c.Method] {
			continue
		}
		switch {
		case c.Method == "InterruptTurn":
			out = append(out, c.Method+"("+c.Key+"):"+c.Value)
		case c.Message != "":
			out = append(out, c.Method+"("+c.Message+")")
		default:
			out = append(out, c.Method)
		}
	}
	return out
}

// inputHookBackend is a tmux-shaped backend that runs onInput after each key
// or interrupt it takes: the agent writing its transcript as it stops.
type inputHookBackend struct {
	idleScript
	onInput func(method string)
}

func (b inputHookBackend) Interrupt(name string) error {
	err := b.idleScript.Interrupt(name)
	if b.onInput != nil {
		b.onInput("Interrupt")
	}
	return err
}

func (b inputHookBackend) SendKeys(name string, keys ...string) error {
	err := b.idleScript.SendKeys(name, keys...)
	if b.onInput != nil {
		b.onInput("SendKeys")
	}
	return err
}

// racingTurnTiming is a turn clock whose every wait passes at once, moving
// now by the wait: a settle poll runs to its bound without sleeping.
func racingTurnTiming(start time.Time) (*turnTiming, *manualTurnClock) {
	clock := newManualTurnClock(start)
	return &turnTiming{
		now:      clock.now,
		idleGap:  turnIdleGap,
		noNotify: true,
		after: func(d time.Duration) <-chan time.Time {
			clock.advance(d)
			fire := make(chan time.Time, 1)
			fire <- clock.now()
			return fire
		},
	}, clock
}

func TestTurnInterruptCodexTranscript(t *testing.T) {
	cases := []struct {
		name    string
		lines   []string
		target  string
		abort   bool // the agent records the turn's abort as it stops
		stopped bool // the runtime is not running
		want    []string
		wantErr error
	}{
		{name: "current turn settles", lines: []string{codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142)}, target: codexTurn1, abort: true, want: []string{"SendKeys(Escape)"}},
		{name: "current turn never ends", lines: []string{codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142)}, target: codexTurn1, want: []string{"SendKeys(Escape)"}, wantErr: runtime.ErrTurnNotEnded},
		{
			name: "another turn runs",
			lines: []string{
				codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), codexComplete(codexTurn1, "2026-10-08T02:22:30.500Z"),
				codexStarted(codexTurn2, "2026-10-08T02:23:01.007Z", 1791426181),
			},
			target: codexTurn1, wantErr: runtime.ErrTurnNotCurrent,
		},
		{name: "no turn runs", lines: []string{codexStarted(codexTurn2, "2026-10-08T02:23:01.007Z", 1791426181), codexComplete(codexTurn2, "2026-10-08T02:23:09.250Z")}, target: codexTurn1, wantErr: runtime.ErrTurnNotCurrent},
		{name: "runtime not running", lines: []string{codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142)}, target: codexTurn1, stopped: true, wantErr: runtime.ErrTurnNotCurrent},
		{name: "torn transcript is unknown", lines: []string{codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142), `{"timestamp":"2026-10-08T02:22:30.500Z","type":"event_msg","payload":{"type":"task_comp`}, target: codexTurn1, wantErr: runtime.ErrRuntimeUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := runtime.NewFake()
			backend := inputHookBackend{idleScript: idleScript{FullFake: runtime.FullFake{Fake: fake}, state: &idleScriptState{}}}
			var path string
			if tc.abort {
				backend.onInput = func(string) { appendTranscript(t, path, codexAborted(codexTurn1, "2026-10-08T02:22:40.000Z")+"\n") }
			}
			s := newTurnSession(t, profiled(runtime.ProfileTmux, backend), "codex", "")
			timing, clock := racingTurnTiming(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
			s.handle.turns.timing = timing
			path = s.writeTranscript(t, "codex", tc.lines...)
			ageTranscript(t, path, clock.now().Add(-time.Hour))
			if tc.stopped {
				if err := fake.Stop(s.info.SessionName); err != nil {
					t.Fatal(err)
				}
			}
			fake.Calls = nil

			err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: tc.target})
			if tc.wantErr == nil && err != nil {
				t.Fatalf("Interrupt(%s) = %v, want nil", tc.target, err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("Interrupt(%s) = %v, want %v", tc.target, err, tc.wantErr)
			}
			if got := sentInputs(fake, s.info.SessionName); !slices.Equal(got, tc.want) {
				t.Fatalf("sent %v, want %v", got, tc.want)
			}
			if errors.Is(tc.wantErr, runtime.ErrTurnNotEnded) && clock.now().Sub(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)) < turnInterruptSettle {
				t.Fatalf("gave up after %s, before the %s bound", clock.now().Sub(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)), turnInterruptSettle)
			}
		})
	}
}

// Claude on tmux has inferred ends: the settle is observed from two idle
// readings with no transcript growth, not from the family's idle wait. The
// check runs under the session mutation lock, and its own reading is busy.
func TestTurnInterruptClaudeSettlesFromIdleReadings(t *testing.T) {
	idle := newIdleScript(idleAnswer{idle: false}, idleAnswer{idle: true}, idleAnswer{idle: true})
	fake := idle.Fake
	s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "claude", "")
	timing, clock := racingTurnTiming(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s.handle.turns.timing = timing
	path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
	ageTranscript(t, path, clock.now().Add(-time.Minute))
	var lockErr error
	idle.state.before = func(call int) {
		if call != 1 {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		lockErr = sessionpkg.WithSessionMutationLockContext(ctx, s.info.ID, func() error { return nil })
		if sent := sentInputs(fake, s.info.SessionName); len(sent) != 0 {
			t.Errorf("the check ran after %v was sent", sent)
		}
	}
	fake.Calls = nil

	if err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: claudeTurn1}); err != nil {
		t.Fatalf("Interrupt(%s) = %v, want nil once the idle readings end it", claudeTurn1, err)
	}
	if !errors.Is(lockErr, sessionpkg.ErrSessionMutationBusy) {
		t.Fatalf("taking the session lock during the check = %v, want ErrSessionMutationBusy", lockErr)
	}
	if got, want := sentInputs(fake, s.info.SessionName), []string{"Interrupt", "ClearInput"}; !slices.Equal(got, want) {
		t.Fatalf("sent %v, want %v", got, want)
	}
	if calls := idle.idleCalls(); calls != 3 {
		t.Fatalf("SnapshotIdle calls = %d, want 3 (the check's, then the settle's two)", calls)
	}
}

// A family wait that returned is no proof: with the idle readings busy, the
// call reports the turn not ended at the bound.
func TestTurnInterruptClaudeBusyIsNotEnded(t *testing.T) {
	idle := newIdleScript(idleAnswer{idle: false})
	fake := idle.Fake
	s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "claude", "")
	timing, clock := racingTurnTiming(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s.handle.turns.timing = timing
	path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
	ageTranscript(t, path, clock.now().Add(-time.Minute))
	fake.WaitForIdleErrors[s.info.SessionName] = nil // the family's idle wait returns at once

	err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: claudeTurn1})
	if !errors.Is(err, runtime.ErrTurnNotEnded) {
		t.Fatalf("Interrupt = %v, want ErrTurnNotEnded", err)
	}
	if lastCall(fake.Calls, "Interrupt") == nil {
		t.Fatalf("calls = %+v, want the interrupt sent", fake.Calls)
	}
}

// End none (subprocess and herdr Claude) and no source at all refuse before
// anything is sent: no read could show the turn end.
func TestTurnInterruptRefusesWithoutAnObservableEnd(t *testing.T) {
	cases := []struct {
		name     string
		profile  runtime.Profile
		provider string
		wantErr  error
	}{
		{name: "subprocess claude", profile: runtime.ProfileSubprocess, provider: "claude", wantErr: runtime.ErrTurnIdentityUnsupported},
		{name: "herdr claude", profile: runtime.ProfileHerdr, provider: "claude", wantErr: runtime.ErrTurnIdentityUnsupported},
		{name: "tmux gemini", profile: runtime.ProfileTmux, provider: "gemini", wantErr: runtime.ErrTurnIdentityUnsupported},
		{name: "exec claude", profile: runtime.ProfileExec, provider: "claude", wantErr: runtime.ErrTurnIdentityUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := runtime.NewFake()
			s := newTurnSession(t, profiled(tc.profile, runtime.FullFake{Fake: fake}), tc.provider, "")
			turn := claudeTurn1
			if tc.provider == "codex" {
				turn = codexTurn1
				s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
			} else {
				s.writeTranscript(t, tc.provider, claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
			}
			fake.Calls = nil
			if err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: turn}); !errors.Is(err, tc.wantErr) {
				t.Fatalf("Interrupt = %v, want %v", err, tc.wantErr)
			}
			if got := sentInputs(fake, s.info.SessionName); len(got) != 0 {
				t.Fatalf("a refused interrupt sent %v", got)
			}
		})
	}
}

// The refusal's floors (X11 §3.3): Start recorded, Fence checked, End
// inferred; a runtime that cannot interrupt at all says so instead.
func TestTurnInterruptRefusal(t *testing.T) {
	checked := TurnCapabilities{
		TurnCapabilities: runtime.TurnCapabilities{Start: runtime.TurnStartRecorded, End: runtime.TurnEndInferred, Fence: runtime.TurnFenceChecked},
		Interrupt:        runtime.TurnInterruptKeystroke,
	}
	with := func(edit func(*TurnCapabilities)) TurnCapabilities {
		c := checked
		edit(&c)
		return c
	}
	cases := []struct {
		name    string
		caps    TurnCapabilities
		wantErr error
	}{
		{name: "checked", caps: checked},
		{name: "full", caps: with(func(c *TurnCapabilities) { c.TurnCapabilities = fullTurnCaps })},
		{name: "end none", caps: with(func(c *TurnCapabilities) { c.End = runtime.TurnEndNone }), wantErr: runtime.ErrTurnIdentityUnsupported},
		{name: "end boundary", caps: with(func(c *TurnCapabilities) { c.End = runtime.TurnEndBoundary }), wantErr: runtime.ErrTurnIdentityUnsupported},
		{name: "start boundary", caps: with(func(c *TurnCapabilities) { c.Start = runtime.TurnStartBoundary }), wantErr: runtime.ErrTurnIdentityUnsupported},
		{name: "fence none", caps: with(func(c *TurnCapabilities) { c.Fence = runtime.TurnFenceNone }), wantErr: runtime.ErrTurnIdentityUnsupported},
		{name: "cannot interrupt", caps: with(func(c *TurnCapabilities) {
			c.Interrupt = runtime.TurnInterruptUnsupported
			c.Fence = runtime.TurnFenceNone
		}), wantErr: sessionpkg.ErrTurnInterruptUnsupported},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := turnInterruptRefusal(tc.caps, "s", "t1")
			if tc.wantErr == nil && err != nil {
				t.Fatalf("refusal = %v, want none", err)
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("refusal = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestTurnInterruptRefusesAnInputFence(t *testing.T) {
	fake := runtime.NewFake()
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: fake}), "codex", "")
	s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
	f, ok := sessionpkg.InputFenceFor(s.info)
	if !ok {
		t.Fatal("session names no incarnation")
	}
	fake.Calls = nil
	if err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: codexTurn1, Fence: f}); !errors.Is(err, ErrTurnInterruptFenced) {
		t.Fatalf("Interrupt(turn and fence) = %v, want ErrTurnInterruptFenced", err)
	}
	if got := sentInputs(fake, s.info.SessionName); len(got) != 0 {
		t.Fatalf("a refused interrupt sent %v", got)
	}
}

// On a protocol source the runtime fences the interrupt to the turn and
// reports its end; no keys are sent.
func TestTurnInterruptProtocolSource(t *testing.T) {
	cases := []struct {
		name    string
		turns   []string
		want    []string
		wantErr error
	}{
		{name: "current turn settles", turns: []string{"acp-1", ""}, want: []string{"InterruptTurn(acp-1):sent", "WaitTurnEnded"}},
		{name: "stale turn", turns: []string{"acp-2"}, want: []string{"InterruptTurn(acp-1):not_current"}, wantErr: runtime.ErrTurnNotCurrent},
		{name: "never ends", turns: []string{"acp-1"}, want: []string{"InterruptTurn(acp-1):sent", "WaitTurnEnded"}, wantErr: runtime.ErrTurnNotEnded},
	}
	for _, tc := range cases {
		t.Run("session handle/"+tc.name, func(t *testing.T) {
			fake := runtime.NewFake()
			s := newTurnSession(t, profiled(runtime.ProfileACP, runtime.FullFake{Fake: fake}), "claude", "")
			fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{s.info.SessionName: {fullTurnCaps}}
			fake.TurnScripts = map[string][]string{s.info.SessionName: tc.turns}
			fake.Calls = nil
			assertTurnInterrupt(t, s.handle, "acp-1", tc.wantErr)
			if got := sentInputs(fake, s.info.SessionName); !slices.Equal(got, tc.want) {
				t.Fatalf("sent %v, want %v", got, tc.want)
			}
		})
		t.Run("runtime handle/"+tc.name, func(t *testing.T) {
			fake := runtime.NewFake()
			h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: fake}))
			fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{"turn-worker": {fullTurnCaps}}
			fake.TurnScripts = map[string][]string{"turn-worker": tc.turns}
			fake.Calls = nil
			assertTurnInterrupt(t, h, "acp-1", tc.wantErr)
			if got := sentInputs(fake, "turn-worker"); !slices.Equal(got, tc.want) {
				t.Fatalf("sent %v, want %v", got, tc.want)
			}
		})
	}
}

// A runtime handle reads no transcript, so on tmux it refuses a named turn
// and sends nothing, while its session-scoped interrupt still runs.
func TestRuntimeHandleTurnInterruptRefusesWithoutAProtocolSource(t *testing.T) {
	fake := runtime.NewFake()
	h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileTmux, runtime.FullFake{Fake: fake}))
	fake.Calls = nil
	assertTurnInterrupt(t, h, codexTurn1, runtime.ErrTurnIdentityUnsupported)
	if got := sentInputs(fake, "turn-worker"); len(got) != 0 {
		t.Fatalf("a refused interrupt sent %v", got)
	}
	if err := h.Interrupt(context.Background(), InterruptRequest{}); err != nil {
		t.Fatalf("session-scoped Interrupt: %v", err)
	}
	if got := sentInputs(fake, "turn-worker"); !slices.Equal(got, []string{"Interrupt"}) {
		t.Fatalf("session-scoped interrupt sent %v, want [Interrupt]", got)
	}
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	if err := h.Interrupt(context.Background(), InterruptRequest{Turn: codexTurn1, Fence: f}); !errors.Is(err, ErrTurnInterruptFenced) {
		t.Fatalf("Interrupt(turn and fence) = %v, want ErrTurnInterruptFenced", err)
	}
}

func assertTurnInterrupt(t *testing.T, h Handle, turn string, wantErr error) {
	t.Helper()
	err := h.Interrupt(context.Background(), InterruptRequest{Turn: turn})
	if wantErr == nil && err != nil {
		t.Fatalf("Interrupt(%s) = %v, want nil", turn, err)
	}
	if wantErr != nil && !errors.Is(err, wantErr) {
		t.Fatalf("Interrupt(%s) = %v, want %v", turn, err, wantErr)
	}
}
