package worker

import (
	"context"
	"errors"
	"slices"
	"sync"
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

func (b inputHookBackend) SendKeysFenced(ctx context.Context, name string, fence runtime.InputFence, keys ...string) error {
	err := b.idleScript.SendKeysFenced(ctx, name, fence, keys...)
	if err == nil && b.onInput != nil {
		b.onInput("SendKeys")
	}
	return err
}

func (b inputHookBackend) InterruptFenced(ctx context.Context, name string, fence runtime.InputFence) error {
	err := b.idleScript.InterruptFenced(ctx, name, fence)
	if err == nil && b.onInput != nil {
		b.onInput("Interrupt")
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
		{name: "the last turn was aborted", lines: []string{codexStarted(codexTurn2, "2026-10-08T02:23:01.007Z", 1791426181), codexAborted(codexTurn2, "2026-10-08T02:23:05.000Z")}, target: codexTurn2, wantErr: runtime.ErrTurnNotCurrent},
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
			s.writeTranscript(t, tc.provider, claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
			fake.Calls = nil
			if err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: claudeTurn1}); !errors.Is(err, tc.wantErr) {
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

// An input fence is a separate axis from the turn: on a transcript source
// every key goes under it, after the turn check, so a stale incarnation's
// fence is a miss with nothing sent and the live one's keys carry it. A
// fence that does not validate is refused before anything is read.
func TestTurnInterruptHonorsTheInputFence(t *testing.T) {
	for _, stale := range []bool{true, false} {
		t.Run(map[bool]string{true: "stale fence", false: "live fence"}[stale], func(t *testing.T) {
			fake := runtime.NewFake()
			backend := inputHookBackend{idleScript: idleScript{FullFake: runtime.FullFake{Fake: fake}, state: &idleScriptState{}}}
			var path string
			backend.onInput = func(string) { appendTranscript(t, path, codexAborted(codexTurn1, "2026-10-08T02:22:40.000Z")+"\n") }
			s := newTurnSession(t, profiled(runtime.ProfileTmux, backend), "codex", "")
			timing, clock := racingTurnTiming(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
			s.handle.turns.timing = timing
			path = s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
			ageTranscript(t, path, clock.now().Add(-time.Hour))
			f, ok := sessionpkg.InputFenceFor(s.info)
			if !ok {
				t.Fatal("session names no incarnation")
			}
			if stale {
				f.Token = "previous-incarnation-token"
			}
			fake.Calls = nil

			err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: codexTurn1, Fence: f})
			sent := sentInputs(fake, s.info.SessionName)
			if stale {
				var miss *runtime.FenceMismatchError
				if !errors.As(err, &miss) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("Interrupt(stale fence) = %v, want *FenceMismatchError, not unknown", err)
				}
				if len(sent) != 0 {
					t.Fatalf("a stale fence sent %v", sent)
				}
				return
			}
			if err != nil {
				t.Fatalf("Interrupt(live fence) = %v, want nil", err)
			}
			if !slices.Equal(sent, []string{"SendKeys(Escape)"}) {
				t.Fatalf("sent %v, want [SendKeys(Escape)]", sent)
			}
			if c := lastCall(fake.SnapshotCalls(), "SendKeys"); c == nil || c.Fence != f {
				t.Fatalf("keys = %+v, want them fenced to %+v", c, f)
			}
		})
	}
	t.Run("invalid fence", func(t *testing.T) {
		fake := runtime.NewFake()
		s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: fake}), "codex", "")
		s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
		fake.Calls = nil
		err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: codexTurn1, Fence: runtime.InputFence{Generation: -1, Token: "tok"}})
		if !errors.Is(err, runtime.ErrInvalidInputFence) || errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) {
			t.Fatalf("Interrupt(invalid fence) = %v, want only ErrInvalidInputFence", err)
		}
		if got := sentInputs(fake, s.info.SessionName); len(got) != 0 {
			t.Fatalf("an invalid fence sent %v", got)
		}
	})
	t.Run("invalid fence/runtime handle", func(t *testing.T) {
		fake := runtime.NewFake()
		h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: fake}))
		fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{"turn-worker": {fullTurnCaps}}
		fake.TurnScripts = map[string][]string{"turn-worker": {"acp-1", ""}}
		fake.Calls = nil
		err := h.Interrupt(context.Background(), InterruptRequest{Turn: "acp-1", Fence: runtime.InputFence{Generation: -1, Token: "tok"}})
		if !errors.Is(err, runtime.ErrInvalidInputFence) || errors.Is(err, runtime.ErrInputFenceUnsupported) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Interrupt(invalid fence) = %v, want only ErrInvalidInputFence", err)
		}
		if got := sentInputs(fake, "turn-worker"); len(got) != 0 {
			t.Fatalf("an invalid fence sent %v", got)
		}
	})
}

// Once the keys are sent, a family wait that finds the session gone (a bare
// runtime.ErrSessionNotFound) leaves the turn not seen to end: the result is
// runtime.ErrTurnNotEnded, never the "nothing sent" *TurnNotCurrentError.
func TestTurnInterruptSentThenRuntimeGoneIsNotEnded(t *testing.T) {
	assertNotEnded := func(t *testing.T, err error, sent, want []string) {
		t.Helper()
		var nc *runtime.TurnNotCurrentError
		if !errors.Is(err, runtime.ErrTurnNotEnded) || errors.As(err, &nc) || errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("Interrupt = %v, want only ErrTurnNotEnded", err)
		}
		if !slices.Equal(sent, want) {
			t.Fatalf("sent %v, want %v", sent, want)
		}
	}
	t.Run("claude idle wait", func(t *testing.T) {
		idle := newIdleScript(idleAnswer{idle: false})
		fake := idle.Fake
		s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "claude", "")
		timing, clock := racingTurnTiming(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
		s.handle.turns.timing = timing
		path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
		ageTranscript(t, path, clock.now().Add(-time.Minute))
		fake.WaitForIdleErrors[s.info.SessionName] = runtime.ErrSessionNotFound
		fake.Calls = nil
		err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: claudeTurn1})
		assertNotEnded(t, err, sentInputs(fake, s.info.SessionName), []string{"Interrupt"})
	})
	t.Run("codex boundary wait", func(t *testing.T) {
		fake := runtime.NewFake()
		s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: fake}), "codex", "")
		timing, clock := racingTurnTiming(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
		s.handle.turns.timing = timing
		path := s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
		ageTranscript(t, path, clock.now().Add(-time.Hour))
		fake.InterruptBoundaryErrors[s.info.SessionName] = runtime.ErrSessionNotFound
		fake.Calls = nil
		err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: codexTurn1})
		assertNotEnded(t, err, sentInputs(fake, s.info.SessionName), []string{"SendKeys(Escape)"})
	})
}

// A runtime that stops after the check reads the turn current, before the
// keys, sent nothing and hosts no turn: a *TurnNotCurrentError.
func TestTurnInterruptRuntimeGoneBeforeTheKeysIsNotCurrent(t *testing.T) {
	idle := newIdleScript(idleAnswer{idle: false})
	fake := idle.Fake
	s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "claude", "")
	timing, clock := racingTurnTiming(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s.handle.turns.timing = timing
	path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
	ageTranscript(t, path, clock.now().Add(-time.Minute))
	idle.state.before = func(call int) {
		if call == 1 {
			if err := fake.Stop(s.info.SessionName); err != nil {
				t.Errorf("Stop: %v", err)
			}
		}
	}
	fake.Calls = nil
	err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: claudeTurn1})
	var nc *runtime.TurnNotCurrentError
	if !errors.As(err, &nc) || errors.Is(err, runtime.ErrTurnNotEnded) {
		t.Fatalf("Interrupt(runtime gone before the keys) = %v, want *TurnNotCurrentError", err)
	}
	if got := sentInputs(fake, s.info.SessionName); len(got) != 0 {
		t.Fatalf("sent %v, want nothing", got)
	}
}

// A closed session row answers a turn-scoped interrupt with an error wrapping
// runtime.ErrSessionNotFound, nothing sent, on either turn source.
func TestTurnInterruptClosedRowIsSessionNotFound(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile runtime.Profile
		family  string
		turn    string
		setup   func(*testing.T, *turnSession, *runtime.Fake)
	}{
		{name: "transcript", profile: runtime.ProfileTmux, family: "codex", turn: codexTurn1, setup: func(t *testing.T, s *turnSession, _ *runtime.Fake) {
			s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
		}},
		{name: "protocol", profile: runtime.ProfileACP, family: "claude", turn: "acp-1", setup: func(_ *testing.T, s *turnSession, fake *runtime.Fake) {
			fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{s.info.SessionName: {fullTurnCaps}}
			fake.TurnScripts = map[string][]string{s.info.SessionName: {"acp-1"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := runtime.NewFake()
			s := newTurnSession(t, profiled(tc.profile, runtime.FullFake{Fake: fake}), tc.family, "")
			tc.setup(t, &s, fake)
			if err := s.store.Close(s.info.ID); err != nil {
				t.Fatal(err)
			}
			fake.Calls = nil
			err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: tc.turn})
			if !errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("Interrupt(closed row) = %v, want ErrSessionNotFound", err)
			}
			if got := sentInputs(fake, s.info.SessionName); len(got) != 0 {
				t.Fatalf("sent %v, want nothing", got)
			}
		})
	}
}

// The session mutation lock wait and the check are bounded by ctx: a call
// whose ctx ends before the check can finish sends nothing and reports
// unknown (runtime.ErrRuntimeUnavailable), never a refusal turned send.
func TestTurnInterruptCheckHonorsCtx(t *testing.T) {
	codexSession := func(t *testing.T) (turnSession, *runtime.Fake) {
		fake := runtime.NewFake()
		s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: fake}), "codex", "")
		s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
		fake.Calls = nil
		return s, fake
	}
	// An idle Claude pane: with a live ctx the check infers the end and
	// refuses; a check that cannot take its idle readings must not send.
	idleClaude := func(t *testing.T) (turnSession, *runtime.Fake) {
		idle := newIdleScript(idleAnswer{idle: true})
		s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "claude", "")
		path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
		ageTranscript(t, path, time.Now().Add(-time.Minute))
		idle.Calls = nil
		return s, idle.Fake
	}
	assertUnknownNothingSent := func(t *testing.T, err error, fake *runtime.Fake, name string) {
		t.Helper()
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) || errors.Is(err, runtime.ErrTurnNotCurrent) {
			t.Fatalf("Interrupt = %v, want ErrRuntimeUnavailable alone", err)
		}
		if got := sentInputs(fake, name); len(got) != 0 {
			t.Fatalf("sent %v after ctx ended", got)
		}
	}
	holdLock := func(t *testing.T, id string) (release func()) {
		t.Helper()
		held, done := make(chan struct{}), make(chan struct{})
		go func() {
			_ = sessionpkg.WithSessionMutationLock(id, func() error { close(held); <-done; return nil })
		}()
		<-held
		var once sync.Once
		release = func() { once.Do(func() { close(done) }) }
		t.Cleanup(release)
		return release
	}

	for _, tc := range []struct {
		name  string
		setup func(*testing.T) (turnSession, *runtime.Fake)
		turn  string
	}{{"codex", codexSession, codexTurn1}, {"idle claude", idleClaude, claudeTurn1}} {
		t.Run(tc.name+"/lock held past the deadline", func(t *testing.T) {
			s, fake := tc.setup(t)
			release := holdLock(t, s.info.ID)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- s.handle.Interrupt(ctx, InterruptRequest{Turn: tc.turn}) }()
			select {
			case err := <-done:
				release()
				assertUnknownNothingSent(t, err, fake, s.info.SessionName)
			case <-time.After(5 * time.Second):
				release()
				<-done
				t.Fatal("Interrupt waited for the session lock past its ctx")
			}
		})
		t.Run(tc.name+"/ctx already ended", func(t *testing.T) {
			s, fake := tc.setup(t)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			assertUnknownNothingSent(t, s.handle.Interrupt(ctx, InterruptRequest{Turn: tc.turn}), fake, s.info.SessionName)
		})
	}
	t.Run("idle claude/deadline shorter than the idle readings", func(t *testing.T) {
		s, fake := idleClaude(t)
		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()
		assertUnknownNothingSent(t, s.handle.Interrupt(ctx, InterruptRequest{Turn: claudeTurn1}), fake, s.info.SessionName)
	})
	t.Run("idle claude/deadline long enough refuses", func(t *testing.T) {
		s, fake := idleClaude(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.handle.Interrupt(ctx, InterruptRequest{Turn: claudeTurn1}); !errors.Is(err, runtime.ErrTurnNotCurrent) {
			t.Fatalf("Interrupt = %v, want ErrTurnNotCurrent", err)
		}
		if got := sentInputs(fake, s.info.SessionName); len(got) != 0 {
			t.Fatalf("sent %v", got)
		}
	})
}

// codexSettleSession is a tmux Codex session whose turn codexTurn1 is open,
// on a racing clock, with onInput run after each key.
func codexSettleSession(t *testing.T, onInput func(path string, clock *manualTurnClock)) (turnSession, *runtime.Fake, *turnTiming, *manualTurnClock, string) {
	t.Helper()
	fake := runtime.NewFake()
	backend := inputHookBackend{idleScript: idleScript{FullFake: runtime.FullFake{Fake: fake}, state: &idleScriptState{}}}
	var path string
	timing, clock := racingTurnTiming(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	if onInput != nil {
		backend.onInput = func(string) { onInput(path, clock) }
	}
	s := newTurnSession(t, profiled(runtime.ProfileTmux, backend), "codex", "")
	s.handle.turns.timing = timing
	path = s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
	ageTranscript(t, path, clock.now().Add(-time.Hour))
	fake.Calls = nil
	return s, fake, timing, clock, path
}

// The settle after the keys: what ends it, what does not, and the error it
// gives when nothing does.
func TestTurnInterruptSettle(t *testing.T) {
	t.Run("a torn record that never completes is not the end", func(t *testing.T) {
		// X11 I8: an unreadable transcript after the keys is never the
		// turn's end. The error says sent (ErrTurnNotEnded) and nothing
		// else, though the last read was unavailable.
		s, fake, _, _, _ := codexSettleSession(t, func(path string, _ *manualTurnClock) {
			appendTranscript(t, path, `{"timestamp":"2026-10-08T02:22:30.500Z","type":"event_msg","payload":{"type":"turn_abo`)
		})
		err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: codexTurn1})
		if !errors.Is(err, runtime.ErrTurnNotEnded) {
			t.Fatalf("Interrupt = %v, want ErrTurnNotEnded", err)
		}
		if errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("Interrupt = %v also wraps ErrRuntimeUnavailable: a sent interrupt would read as unknown", err)
		}
		if got := sentInputs(fake, s.info.SessionName); !slices.Equal(got, []string{"SendKeys(Escape)"}) {
			t.Fatalf("sent %v", got)
		}
	})
	t.Run("a successor turn settles the target", func(t *testing.T) {
		s, _, _, _, _ := codexSettleSession(t, func(path string, _ *manualTurnClock) {
			appendTranscript(t, path, codexAborted(codexTurn1, "2026-10-08T02:22:40.000Z")+"\n"+codexStarted(codexTurn2, "2026-10-08T02:22:41.000Z", 1791426161)+"\n")
		})
		if err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: codexTurn1}); err != nil {
			t.Fatalf("Interrupt = %v, want nil: the target left Current", err)
		}
	})
	t.Run("a failed family wait is re-read from the transcript", func(t *testing.T) {
		s, fake, _, _, _ := codexSettleSession(t, func(path string, _ *manualTurnClock) {
			appendTranscript(t, path, codexAborted(codexTurn1, "2026-10-08T02:22:40.000Z")+"\n")
		})
		fake.WaitForIdleErrors[s.info.SessionName] = errors.New("still busy")
		if err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: codexTurn1}); err != nil {
			t.Fatalf("Interrupt = %v, want nil: the transcript shows the end", err)
		}
	})
	t.Run("past the deadline the transcript is still read once", func(t *testing.T) {
		var once sync.Once
		s, _, _, _, _ := codexSettleSession(t, func(path string, clock *manualTurnClock) {
			once.Do(func() {
				clock.advance(2 * turnInterruptSettle)
				appendTranscript(t, path, codexAborted(codexTurn1, "2026-10-08T02:22:40.000Z")+"\n")
			})
		})
		if err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: codexTurn1}); err != nil {
			t.Fatalf("Interrupt = %v, want nil from the read past the deadline", err)
		}
	})
	t.Run("a session closed after the keys has ended its turn", func(t *testing.T) {
		var s turnSession
		s, _, _, _, _ = codexSettleSession(t, func(string, *manualTurnClock) {
			if err := s.store.Close(s.info.ID); err != nil {
				t.Errorf("closing the session: %v", err)
			}
		})
		if err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: codexTurn1}); err != nil {
			t.Fatalf("Interrupt = %v, want nil: a gone session's turn ended (WaitTurnEnded's rule)", err)
		}
	})
	t.Run("the bound starts when the check passes", func(t *testing.T) {
		// Time spent before the check passes (the lock wait, the check's
		// own reads) spends the clock past 15 s: the settle still gets its
		// full bound after the check, and sees the end on a later poll. The
		// clock's first reading is taken, then jumps.
		s, _, timing, clock, path := codexSettleSession(t, nil)
		polls := 0
		after := timing.after
		timing.after = func(d time.Duration) <-chan time.Time {
			if polls++; polls == 3 {
				appendTranscript(t, path, codexAborted(codexTurn1, "2026-10-08T02:22:40.000Z")+"\n")
			}
			return after(d)
		}
		jumped := false
		timing.now = func() time.Time {
			now := clock.now()
			if !jumped {
				jumped = true
				clock.advance(2 * turnInterruptSettle)
			}
			return now
		}
		if err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: codexTurn1}); err != nil {
			t.Fatalf("Interrupt = %v, want nil: the end came within the bound after the check", err)
		}
		if polls < 3 {
			t.Fatalf("settled after %d polls, want the end seen on the third", polls)
		}
	})
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
}

// A protocol source's runtime takes no input fence on its turn interrupt:
// a fenced one is refused, nothing sent, on both handles.
func TestProtocolTurnInterruptRefusesAnInputFence(t *testing.T) {
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	t.Run("session handle", func(t *testing.T) {
		fake := runtime.NewFake()
		s := newTurnSession(t, profiled(runtime.ProfileACP, runtime.FullFake{Fake: fake}), "claude", "")
		fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{s.info.SessionName: {fullTurnCaps}}
		fake.TurnScripts = map[string][]string{s.info.SessionName: {"acp-1", ""}}
		fake.Calls = nil
		if err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: "acp-1", Fence: f}); !errors.Is(err, runtime.ErrInputFenceUnsupported) {
			t.Fatalf("Interrupt(turn and fence) = %v, want ErrInputFenceUnsupported", err)
		}
		if got := sentInputs(fake, s.info.SessionName); len(got) != 0 {
			t.Fatalf("a refused interrupt sent %v", got)
		}
	})
	t.Run("runtime handle", func(t *testing.T) {
		fake := runtime.NewFake()
		h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: fake}))
		fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{"turn-worker": {fullTurnCaps}}
		fake.TurnScripts = map[string][]string{"turn-worker": {"acp-1", ""}}
		fake.Calls = nil
		if err := h.Interrupt(context.Background(), InterruptRequest{Turn: "acp-1", Fence: f}); !errors.Is(err, runtime.ErrInputFenceUnsupported) {
			t.Fatalf("Interrupt(turn and fence) = %v, want ErrInputFenceUnsupported", err)
		}
		if got := sentInputs(fake, "turn-worker"); len(got) != 0 {
			t.Fatalf("a refused interrupt sent %v", got)
		}
	})
}

// On a protocol source a runtime that no longer hosts the session (a bare
// ErrSessionNotFound from InterruptTurn's read) has no turn: nothing sent,
// a *TurnNotCurrentError, as on a transcript source.
func TestProtocolTurnInterruptGoneRuntimeIsNotCurrent(t *testing.T) {
	fake := runtime.NewFake()
	s := newTurnSession(t, profiled(runtime.ProfileACP, runtime.FullFake{Fake: fake}), "claude", "")
	fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{s.info.SessionName: {fullTurnCaps}}
	fake.TurnScripts = map[string][]string{s.info.SessionName: {"acp-1"}}
	fake.TurnReadErrors = map[string][]error{s.info.SessionName: {runtime.ErrSessionNotFound}}
	fake.Calls = nil
	err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: "acp-1"})
	var nc *runtime.TurnNotCurrentError
	if !errors.As(err, &nc) || errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("Interrupt(runtime gone) = %v, want *TurnNotCurrentError", err)
	}
	if got := sentInputs(fake, s.info.SessionName); !slices.Equal(got, []string{"InterruptTurn(acp-1):error"}) {
		t.Fatalf("sent %v, want only the refused InterruptTurn", got)
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
