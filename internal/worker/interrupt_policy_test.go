package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// callMethodsOf lists the methods of sp's calls on name, in order.
func callMethodsOf(sp *runtime.Fake, name string) []string {
	var out []string
	for _, c := range sp.SnapshotCalls() {
		if c.Name == name {
			out = append(out, c.Method)
		}
	}
	return out
}

// A policy the handle cannot run is refused before anything is sent, and
// says nothing was (ErrInterruptNotSent): an unknown on_failure, a restart
// with no actor to start the runtime as, and an invalid fence.
func TestInterruptPolicyRefusedBeforeSending(t *testing.T) {
	h, sp, name, _ := fencedSessionHandle(t)
	cases := []struct {
		name string
		call func() error
		want error
	}{
		{"unknown on_failure", func() error {
			_, err := h.InterruptAs(context.Background(), handleActor(t, h, sessionpkg.ActorController), InterruptRequest{OnFailure: "retry"})
			return err
		}, nil},
		{"restart without an actor", func() error {
			return h.Interrupt(context.Background(), InterruptRequest{OnFailure: InterruptOnFailureRestart})
		}, sessionpkg.ErrNoActor},
		{"restart with a zero actor", func() error {
			_, err := h.InterruptAs(context.Background(), sessionpkg.Actor{}, InterruptRequest{OnFailure: InterruptOnFailureRestart})
			return err
		}, sessionpkg.ErrNoActor},
		{"invalid fence", func() error {
			return h.Interrupt(context.Background(), InterruptRequest{Fence: runtime.InputFence{Token: "tok"}})
		}, runtime.ErrInvalidInputFence},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp.Calls = nil
			err := tc.call()
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("Interrupt = %v, want refused (%v)", err, tc.want)
			}
			if !errors.Is(err, ErrInterruptNotSent) {
				t.Fatalf("Interrupt = %v, want ErrInterruptNotSent: nothing was sent", err)
			}
			if got := callMethodsOf(sp, name); len(got) != 0 {
				t.Fatalf("a refused interrupt made calls %v", got)
			}
		})
	}
}

// A session-scoped interrupt whose turn does not settle restarts the runtime
// with on_failure restart and reports it; with fail it returns the not-ended
// error and leaves the runtime alone.
func TestSessionInterruptOnFailure(t *testing.T) {
	for _, tc := range []struct {
		onFailure InterruptOnFailure
		restarted bool
	}{
		{InterruptOnFailureRestart, true},
		{InterruptOnFailureFail, false},
		{"", false},
	} {
		t.Run(string(tc.onFailure), func(t *testing.T) {
			h, sp, name, f := fencedSessionHandle(t)
			sp.WaitForIdleErrors[name] = errors.New("still busy")
			res, err := h.InterruptAs(context.Background(), handleActor(t, h, sessionpkg.ActorController), InterruptRequest{Fence: f, OnFailure: tc.onFailure})
			if res.Restarted != tc.restarted {
				t.Fatalf("Restarted = %v, want %v", res.Restarted, tc.restarted)
			}
			methods := callMethodsOf(sp, name)
			if tc.restarted {
				if err != nil {
					t.Fatalf("InterruptAs(restart) = %v, want nil", err)
				}
				if i, j := slices.Index(methods, "Stop"), slices.Index(methods, "Start"); i < 0 || j < i {
					t.Fatalf("calls = %v, want Stop then Start", methods)
				}
				return
			}
			if !errors.Is(err, runtime.ErrTurnNotEnded) {
				t.Fatalf("InterruptAs(%q) = %v, want ErrTurnNotEnded", tc.onFailure, err)
			}
			if slices.Contains(methods, "Stop") || slices.Contains(methods, "Start") {
				t.Fatalf("calls = %v, want no restart", methods)
			}
		})
	}
}

// A turn-scoped interrupt whose turn is never seen to end restarts the
// runtime, under the session mutation lock, after the settle bound.
func TestTurnInterruptOnFailureRestart(t *testing.T) {
	fake := runtime.NewFake()
	backend := inputHookBackend{idleScript: idleScript{FullFake: runtime.FullFake{Fake: fake}, state: &idleScriptState{}}}
	s := newTurnSession(t, profiled(runtime.ProfileTmux, backend), "codex", "")
	timing, clock := racingTurnTiming(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	s.handle.turns.timing = timing
	path := s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
	ageTranscript(t, path, clock.now().Add(-time.Hour))
	fake.Calls = nil

	res, err := s.handle.InterruptAs(context.Background(), handleActor(t, s.handle, sessionpkg.ActorController),
		InterruptRequest{Turn: codexTurn1, OnFailure: InterruptOnFailureRestart})
	if err != nil || !res.Restarted {
		t.Fatalf("InterruptAs(turn, restart) = %+v, %v; want a restart, nil", res, err)
	}
	methods := callMethodsOf(fake, s.info.SessionName)
	if i, j := slices.Index(methods, "SendKeys"), slices.Index(methods, "Stop"); i < 0 || j < i {
		t.Fatalf("calls = %v, want the keys, then the restart's Stop", methods)
	}
	if !slices.Contains(methods, "Start") {
		t.Fatalf("calls = %v, want the restart's Start", methods)
	}
	if clock.now().Sub(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC)) < turnInterruptSettle {
		t.Fatal("restarted before the settle bound")
	}
}

// A turn-scoped restart on failure needs something showing the turn still
// running after the keys: a settle read naming it current
// (TestTurnInterruptOnFailureRestart), or a family wait that failed for a
// cause other than a runtime that could not answer. A settle whose every
// read fails (a torn transcript record here) after family waits that passed,
// or that could not answer either, cannot tell whether the interrupt ended
// the turn: the runtime is left running, and the error wraps
// session.ErrInterruptOutcomeUnknown and runtime.ErrRuntimeUnavailable, not
// runtime.ErrTurnNotEnded.
func TestTurnInterruptRestartNeedsTheTurnSeenRunning(t *testing.T) {
	for _, tc := range []struct {
		name      string
		boundary  error // the Codex family's boundary wait after the keys
		restarted bool
	}{
		{"the family waits passed", nil, false},
		{"the boundary wait could not answer", fmt.Errorf("reading the rollout: %w", runtime.ErrRuntimeUnavailable), false},
		{"the boundary wait saw no boundary", errors.New("no turn_aborted marker yet"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, fake, _, _, _ := codexSettleSession(t, func(path string, _ *manualTurnClock) {
				appendTranscript(t, path, `{"timestamp":"2026-10-08T02:22:30.500Z","type":"event_msg","payload":{"type":"turn_abo`)
			})
			if tc.boundary != nil {
				fake.InterruptBoundaryErrors[s.info.SessionName] = tc.boundary
			}
			res, err := s.handle.InterruptAs(context.Background(), handleActor(t, s.handle, sessionpkg.ActorController),
				InterruptRequest{Turn: codexTurn1, OnFailure: InterruptOnFailureRestart})
			methods := callMethodsOf(fake, s.info.SessionName)
			if tc.restarted {
				if err != nil || !res.Restarted {
					t.Fatalf("InterruptAs(turn, restart) = %+v, %v; want a restart, nil", res, err)
				}
				if i, j := slices.Index(methods, "Stop"), slices.Index(methods, "Start"); i < 0 || j < i {
					t.Fatalf("calls = %v, want the restart's Stop, then its Start", methods)
				}
				return
			}
			if res.Restarted || !errors.Is(err, sessionpkg.ErrInterruptOutcomeUnknown) || !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) {
				t.Fatalf("InterruptAs(turn, restart, settle read nothing) = %+v, %v; want ErrInterruptOutcomeUnknown and ErrRuntimeUnavailable, and no restart", res, err)
			}
			if slices.Contains(methods, "Stop") || slices.Contains(methods, "Start") {
				t.Fatalf("calls = %v, want the runtime left running", methods)
			}
			if got := sentInputs(fake, s.info.SessionName); !slices.Equal(got, []string{"SendKeys(Escape)"}) {
				t.Fatalf("sent %v, want [SendKeys(Escape)]", got)
			}
		})
	}
}

// A turn-scoped restart that stopped the runtime and could not start it
// again reaches the caller as that (session.ErrRestartNotStarted): never as
// a row that would not load (runtime.ErrRuntimeUnavailable, nothing sent),
// nor as a turn not seen to end.
func TestTurnInterruptFailedRestartStartIsNotStarted(t *testing.T) {
	fake := runtime.NewFake()
	backend := inputHookBackend{idleScript: idleScript{FullFake: runtime.FullFake{Fake: fake}, state: &idleScriptState{}}}
	s := newTurnSession(t, profiled(runtime.ProfileTmux, backend), "codex", "")
	timing, clock := racingTurnTiming(time.Date(2026, 10, 9, 0, 0, 0, 0, time.UTC))
	s.handle.turns.timing = timing
	path := s.writeTranscript(t, "codex", codexStarted(codexTurn1, "2026-10-08T02:22:22.191Z", 1791426142))
	ageTranscript(t, path, clock.now().Add(-time.Hour))
	fake.StartErrors[s.info.SessionName] = errors.New("cannot start")
	fake.Calls = nil

	res, err := s.handle.InterruptAs(context.Background(), handleActor(t, s.handle, sessionpkg.ActorController),
		InterruptRequest{Turn: codexTurn1, OnFailure: InterruptOnFailureRestart})
	if res.Restarted || !errors.Is(err, sessionpkg.ErrRestartNotStarted) {
		t.Fatalf("InterruptAs(turn, restart, start fails) = %+v, %v; want ErrRestartNotStarted and no restart", res, err)
	}
	for _, sentinel := range []error{runtime.ErrRuntimeUnavailable, runtime.ErrTurnNotEnded, runtime.ErrSessionNotFound} {
		if errors.Is(err, sentinel) {
			t.Fatalf("InterruptAs(turn, restart, start fails) = %v, want ErrRestartNotStarted alone, not %v", err, sentinel)
		}
	}
	if methods := callMethodsOf(fake, s.info.SessionName); !slices.Contains(methods, "Stop") {
		t.Fatalf("calls = %v, want the restart's Stop", methods)
	}
}

// A replacement is refused, nothing sent, where an interrupt does not
// replace the session's turn: a Pi session restarts instead, and a bare
// runtime has no deliver-now.
func TestInterruptForReplacementRefusal(t *testing.T) {
	fake := runtime.NewFake()
	s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: fake}), "pi", "")
	fake.Calls = nil
	err := s.handle.Interrupt(context.Background(), InterruptRequest{ForReplacement: true})
	if !errors.Is(err, sessionpkg.ErrTurnInterruptUnsupported) {
		t.Fatalf("Interrupt(pi, replacement) = %v, want ErrTurnInterruptUnsupported", err)
	}
	if got := sentInputs(fake, s.info.SessionName); len(got) != 0 {
		t.Fatalf("a refused replacement sent %v", got)
	}

	rh, sp, _ := fencedRuntimeHandle(t)
	if err := rh.Interrupt(context.Background(), InterruptRequest{ForReplacement: true}); !errors.Is(err, sessionpkg.ErrTurnInterruptUnsupported) {
		t.Fatalf("RuntimeHandle.Interrupt(replacement) = %v, want ErrTurnInterruptUnsupported", err)
	}
	if got := callMethodsOf(sp, "rt"); len(got) != 0 {
		t.Fatalf("a refused replacement made calls %v", got)
	}
}

// A replacement on a session that an interrupt replaces prepares it after
// the turn settled: Claude's input box is cleared for the message.
func TestInterruptForReplacementPreparesTheSession(t *testing.T) {
	h, sp, name, f := fencedSessionHandle(t)
	if err := h.Interrupt(context.Background(), InterruptRequest{Fence: f, ForReplacement: true}); err != nil {
		t.Fatalf("Interrupt(replacement): %v", err)
	}
	methods := callMethodsOf(sp, name)
	if i, j := slices.Index(methods, "Interrupt"), slices.Index(methods, "ClearInput"); i < 0 || j < i {
		t.Fatalf("calls = %v, want the interrupt, then the input cleared", methods)
	}
}

// A bare runtime cannot restart: on_failure restart is refused, nothing
// sent, rather than run as fail.
func TestRuntimeHandleRefusesARestartOnFailure(t *testing.T) {
	rh, sp, f := fencedRuntimeHandle(t)
	by := sessionpkg.Actor{Kind: sessionpkg.ActorController}
	_, err := rh.InterruptAs(context.Background(), by, InterruptRequest{Fence: f, OnFailure: InterruptOnFailureRestart})
	if !errors.Is(err, ErrInterruptRestartUnsupported) {
		t.Fatalf("RuntimeHandle.InterruptAs(restart) = %v, want ErrInterruptRestartUnsupported", err)
	}
	if got := callMethodsOf(sp, "rt"); len(got) != 0 {
		t.Fatalf("a refused restart made calls %v", got)
	}
	if _, err := rh.InterruptAs(context.Background(), by, InterruptRequest{Fence: f, OnFailure: InterruptOnFailureFail}); err != nil {
		t.Fatalf("RuntimeHandle.InterruptAs(fail) = %v, want nil", err)
	}
}

// AllowUnfenced lets a fenced interrupt go out where it cannot carry the
// fence, reporting Unfenced; without it the same request is refused,
// nothing sent. A path that carries the fence reports nothing.
func TestInterruptAllowUnfenced(t *testing.T) {
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	t.Run("protocol turn interrupt on a session handle", func(t *testing.T) {
		fake := runtime.NewFake()
		s := newTurnSession(t, profiled(runtime.ProfileACP, runtime.FullFake{Fake: fake}), "claude", "")
		fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{s.info.SessionName: {fullTurnCaps}}
		fake.TurnScripts = map[string][]string{s.info.SessionName: {"acp-1", ""}}
		fake.Calls = nil
		res, err := s.handle.InterruptAs(context.Background(), handleActor(t, s.handle, sessionpkg.ActorController),
			InterruptRequest{Turn: "acp-1", Fence: f, AllowUnfenced: true})
		if err != nil || !res.Unfenced {
			t.Fatalf("InterruptAs(turn, fence, allow unfenced) = %+v, %v; want sent unfenced", res, err)
		}
		if got := sentInputs(fake, s.info.SessionName); !slices.Contains(got, "InterruptTurn(acp-1):sent") {
			t.Fatalf("sent %v, want the turn interrupt", got)
		}
	})
	t.Run("protocol turn interrupt on a runtime handle", func(t *testing.T) {
		fake := runtime.NewFake()
		h := newTurnRuntimeHandle(t, runtime.NewFakeProfile(runtime.ProfileACP, runtime.FullFake{Fake: fake}))
		fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{"turn-worker": {fullTurnCaps}}
		fake.TurnScripts = map[string][]string{"turn-worker": {"acp-1", ""}}
		fake.Calls = nil
		res, err := h.InterruptAs(context.Background(), sessionpkg.Actor{Kind: sessionpkg.ActorController},
			InterruptRequest{Turn: "acp-1", Fence: f, AllowUnfenced: true})
		if err != nil || !res.Unfenced {
			t.Fatalf("InterruptAs(turn, fence, allow unfenced) = %+v, %v; want sent unfenced", res, err)
		}
	})
	t.Run("unfenceable runtime on a session handle", func(t *testing.T) {
		h, sp, name, sf := fencedSessionHandle(t)
		sp.InputFencingValue = runtime.InputFencingNone
		res, err := h.InterruptAs(context.Background(), handleActor(t, h, sessionpkg.ActorController), InterruptRequest{Fence: sf, AllowUnfenced: true})
		if err != nil || !res.Unfenced {
			t.Fatalf("InterruptAs(unfenceable, allow unfenced) = %+v, %v; want sent unfenced", res, err)
		}
		for _, c := range sp.SnapshotCalls() {
			if c.Name == name && !c.Fence.IsZero() {
				t.Fatalf("%s carries %v on an unfenceable runtime", c.Method, c.Fence)
			}
		}
		if !slices.Contains(callMethodsOf(sp, name), "Interrupt") {
			t.Fatalf("calls = %v, want the interrupt", callMethodsOf(sp, name))
		}
	})
	t.Run("unfenceable runtime on a runtime handle", func(t *testing.T) {
		rh, sp, rf := fencedRuntimeHandle(t)
		sp.InputFencingValue = runtime.InputFencingNone
		res, err := rh.InterruptAs(context.Background(), sessionpkg.Actor{Kind: sessionpkg.ActorController}, InterruptRequest{Fence: rf, AllowUnfenced: true})
		if err != nil || !res.Unfenced {
			t.Fatalf("InterruptAs(unfenceable, allow unfenced) = %+v, %v; want sent unfenced", res, err)
		}
	})
	t.Run("a fenceable runtime keeps the fence", func(t *testing.T) {
		h, sp, name, sf := fencedSessionHandle(t)
		res, err := h.InterruptAs(context.Background(), handleActor(t, h, sessionpkg.ActorController), InterruptRequest{Fence: sf, AllowUnfenced: true})
		if err != nil || res.Unfenced {
			t.Fatalf("InterruptAs(fenceable, allow unfenced) = %+v, %v; want sent fenced", res, err)
		}
		for _, c := range sp.SnapshotCalls() {
			if c.Name == name && inputMethods[c.Method] && c.Fence != sf {
				t.Fatalf("%s carries %v, want the fence", c.Method, c.Fence)
			}
		}
	})
}

// A session-scoped restart or replacement carries the fence on every input
// up to the restart's stop.
func TestSessionInterruptPolicyFencesEveryInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  func(runtime.InputFence) InterruptRequest
		busy bool
	}{
		{"restart", func(f runtime.InputFence) InterruptRequest {
			return InterruptRequest{Fence: f, OnFailure: InterruptOnFailureRestart}
		}, true},
		{"replacement", func(f runtime.InputFence) InterruptRequest { return InterruptRequest{Fence: f, ForReplacement: true} }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sp, name, f := fencedSessionHandle(t)
			if tc.busy {
				sp.WaitForIdleErrors[name] = errors.New("still busy")
			}
			if _, err := h.InterruptAs(context.Background(), handleActor(t, h, sessionpkg.ActorController), tc.req(f)); err != nil {
				t.Fatalf("InterruptAs(%s): %v", tc.name, err)
			}
			var sent int
			for _, c := range sp.SnapshotCalls() {
				if c.Name != name {
					continue
				}
				if c.Method == "Stop" {
					break
				}
				if inputMethods[c.Method] {
					sent++
					if c.Fence != f {
						t.Errorf("%s(%s) carries %v, want the fence", c.Method, c.Message, c.Fence)
					}
				}
			}
			if sent == 0 {
				t.Fatal("no input sent")
			}
		})
	}
}

// A turn-scoped replacement on a checked transcript source prepares the
// session: a clear that fails is the replacement's error, where a plain
// stop only logs it.
func TestTurnInterruptForReplacementPreparesTheSession(t *testing.T) {
	for _, tc := range []struct {
		name        string
		replacement bool
	}{{"replacement", true}, {"plain", false}} {
		t.Run(tc.name, func(t *testing.T) {
			idle := newIdleScript(idleAnswer{idle: false}, idleAnswer{idle: true}, idleAnswer{idle: true})
			fake := idle.Fake
			s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "claude", "")
			timing, clock := racingTurnTiming(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
			s.handle.turns.timing = timing
			path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
			ageTranscript(t, path, clock.now().Add(-time.Minute))
			fake.ClearInputErrors[s.info.SessionName] = errors.New("input box will not clear")
			fake.Calls = nil
			err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: claudeTurn1, ForReplacement: tc.replacement})
			if tc.replacement != errors.Is(err, sessionpkg.ErrReplacementNotPrepared) {
				t.Fatalf("Interrupt(%s, replacement %v) = %v, want ErrReplacementNotPrepared exactly for a replacement", claudeTurn1, tc.replacement, err)
			}
			if !tc.replacement && err != nil {
				t.Fatalf("Interrupt(%s) = %v, want nil", claudeTurn1, err)
			}
		})
	}
}

// ACP interrupts by keystroke but reads its turns by protocol: a turn-scoped
// interrupt with on_failure restart there interrupts the turn by protocol
// first, and only once the turn is not seen to end stops the runtime and
// starts it again, as the session-scoped stop and deliver-now restart it. A
// turn that ends is left alone. A protocol runtime (T3) is never restarted,
// its stop may leave it running: the turn not seen to end is returned.
func TestProtocolTurnInterruptRestartOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		profile   runtime.Profile
		turns     []string
		restarted bool
		notEnded  bool
	}{
		{"keystroke runtime, turn not seen to end", runtime.ProfileACP, []string{"acp-1"}, true, false},
		{"keystroke runtime, turn ends", runtime.ProfileACP, []string{"acp-1", ""}, false, false},
		{"protocol runtime, turn not seen to end", runtime.ProfileT3Bridge, []string{"acp-1"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := runtime.NewFake()
			s := newTurnSession(t, profiled(tc.profile, runtime.FullFake{Fake: fake}), "claude", "")
			fake.TurnCapabilityScripts = map[string][]runtime.TurnCapabilities{s.info.SessionName: {fullTurnCaps}}
			fake.TurnScripts = map[string][]string{s.info.SessionName: tc.turns}
			fake.Calls = nil
			res, err := s.handle.InterruptAs(context.Background(), handleActor(t, s.handle, sessionpkg.ActorController),
				InterruptRequest{Turn: "acp-1", OnFailure: InterruptOnFailureRestart})
			if res.Restarted != tc.restarted || errors.Is(err, runtime.ErrTurnNotEnded) != tc.notEnded || (err != nil) != tc.notEnded {
				t.Fatalf("InterruptAs(protocol turn, restart) = %+v, %v; want restarted %v, not ended %v", res, err, tc.restarted, tc.notEnded)
			}
			methods := callMethodsOf(fake, s.info.SessionName)
			interrupt, stop, start := slices.Index(methods, "InterruptTurn"), slices.Index(methods, "Stop"), slices.Index(methods, "Start")
			switch {
			case interrupt < 0:
				t.Fatalf("calls = %v, want the turn interrupt sent", methods)
			case tc.restarted && (stop < interrupt || start < stop):
				t.Fatalf("calls = %v, want the turn interrupt, then the restart's Stop and Start", methods)
			case !tc.restarted && (stop >= 0 || start >= 0):
				t.Fatalf("calls = %v, want no restart", methods)
			}
		})
	}
}

// An interrupt that fails before anything is sent says so: the error wraps
// ErrInterruptNotSent (here the restart's start command, whose row read
// fails).
func TestInterruptPreSendFailureIsNotSent(t *testing.T) {
	sp := runtime.NewFake()
	store := beads.NewMemStore()
	manager := sessionpkg.NewManagerWithOptions(store, sp, sessionpkg.WithCityPath(t.TempDir()))
	h, err := NewSessionHandle(SessionHandleConfig{
		Manager: manager,
		Session: SessionSpec{Template: "probe", Title: "Probe", Command: "claude", WorkDir: t.TempDir(), Provider: "claude"},
	})
	if err != nil {
		t.Fatalf("NewSessionHandle: %v", err)
	}
	if err := h.Start(context.Background(), handleActor(t, h, sessionpkg.ActorAgent)); err != nil {
		t.Fatalf("Start: %v", err)
	}
	by := handleActor(t, h, sessionpkg.ActorController)
	h.manager = sessionpkg.NewManagerWithOptions(failingGetStore{Store: store, err: errors.New("store boom")}, sp, sessionpkg.WithCityPath(t.TempDir()))
	sp.Calls = nil
	_, err = h.InterruptAs(context.Background(), by, InterruptRequest{OnFailure: InterruptOnFailureRestart})
	if !errors.Is(err, ErrInterruptNotSent) {
		t.Fatalf("InterruptAs(row read fails) = %v, want ErrInterruptNotSent", err)
	}
	if got := sp.SnapshotCalls(); len(got) != 0 {
		t.Fatalf("an interrupt that failed before sending made calls %+v", got)
	}
}

// A session-scoped interrupt whose stop loads the row again under the
// session mutation lock and finds it closed or gone is not found
// (runtime.ErrSessionNotFound), and one whose row will not load is
// ErrInterruptNotSent, nothing sent either way: with or without a deadline,
// and with a restart on failure. Neither is an error in no class, which a
// caller must take as maybe sent.
func TestSessionInterruptRowLoadUnderTheLock(t *testing.T) {
	for _, tc := range []struct {
		name     string
		onLocked func(s turnSession) error
		want     error
		not      []error
	}{
		{
			name:     "closed",
			onLocked: func(s turnSession) error { return s.store.Close(s.info.ID) },
			want:     runtime.ErrSessionNotFound,
			not:      []error{ErrInterruptNotSent, runtime.ErrRuntimeUnavailable},
		},
		{
			name: "gone",
			onLocked: func(s turnSession) error {
				return fmt.Errorf("getting bead %q: %w", s.info.ID, beads.ErrNotFound)
			},
			want: runtime.ErrSessionNotFound,
			not:  []error{ErrInterruptNotSent, runtime.ErrRuntimeUnavailable},
		},
		{
			name:     "unreadable",
			onLocked: func(turnSession) error { return errors.New("store: connection reset by peer") },
			want:     ErrInterruptNotSent,
			not:      []error{runtime.ErrSessionNotFound, runtime.ErrRuntimeUnavailable},
		},
	} {
		for _, call := range []struct {
			name     string
			deadline bool
			req      InterruptRequest
		}{
			{"no deadline", false, InterruptRequest{}},
			{"deadline", true, InterruptRequest{}},
			{"restart on failure", true, InterruptRequest{OnFailure: InterruptOnFailureRestart}},
		} {
			t.Run(tc.name+"/"+call.name, func(t *testing.T) {
				fake := runtime.NewFake()
				s := newTurnSession(t, profiled(runtime.ProfileTmux, runtime.FullFake{Fake: fake}), "claude", "")
				s.onLockedLoad(t, func() error { return tc.onLocked(s) })
				ctx := context.Background()
				if call.deadline {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
					defer cancel()
				}
				fake.Calls = nil
				_, err := s.handle.InterruptAs(ctx, handleActor(t, s.handle, sessionpkg.ActorController), call.req)
				if !errors.Is(err, tc.want) {
					t.Fatalf("InterruptAs(row %s) = %v, want %v", tc.name, err, tc.want)
				}
				for _, other := range append(tc.not, runtime.ErrTurnNotEnded, sessionpkg.ErrInterruptOutcomeUnknown) {
					if errors.Is(err, other) {
						t.Fatalf("InterruptAs(row %s) = %v, want %v, not %v too", tc.name, err, tc.want, other)
					}
				}
				if got := sentInputs(fake, s.info.SessionName); len(got) != 0 {
					t.Fatalf("sent %v, want nothing", got)
				}
			})
		}
	}
}

// A turn-scoped interrupt on a transcript source whose runtime cannot fence
// the session (herdr Codex: turns read from its transcript, no input
// fencing) goes out without its fence where the request allows it, and
// reports Unfenced; without AllowUnfenced it is refused, nothing sent.
func TestTranscriptTurnInterruptOnAnUnfenceableRuntime(t *testing.T) {
	f := runtime.InputFence{Generation: 1, Epoch: 1, Token: "tok"}
	for _, allow := range []bool{true, false} {
		t.Run(map[bool]string{true: "allow unfenced", false: "fenced only"}[allow], func(t *testing.T) {
			idle := newIdleScript(idleAnswer{idle: false}, idleAnswer{idle: true}, idleAnswer{idle: true})
			fake := idle.Fake
			s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "claude", "")
			timing, clock := racingTurnTiming(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
			s.handle.turns.timing = timing
			path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
			ageTranscript(t, path, clock.now().Add(-time.Minute))
			fake.InputFencingValue = runtime.InputFencingNone
			fake.Calls = nil
			res, err := s.handle.InterruptAs(context.Background(), handleActor(t, s.handle, sessionpkg.ActorController),
				InterruptRequest{Turn: claudeTurn1, Fence: f, AllowUnfenced: allow})
			if !allow {
				if !errors.Is(err, runtime.ErrInputFenceUnsupported) {
					t.Fatalf("InterruptAs(turn, fence, unfenceable) = %+v, %v; want ErrInputFenceUnsupported", res, err)
				}
				if got := sentInputs(fake, s.info.SessionName); len(got) != 0 {
					t.Fatalf("a refused interrupt sent %v", got)
				}
				return
			}
			if err != nil || !res.Unfenced {
				t.Fatalf("InterruptAs(turn, fence, allow unfenced, unfenceable) = %+v, %v; want sent unfenced", res, err)
			}
			for _, c := range fake.SnapshotCalls() {
				if c.Name == s.info.SessionName && !c.Fence.IsZero() {
					t.Fatalf("%s carries %v on an unfenceable runtime", c.Method, c.Fence)
				}
			}
			if got := sentInputs(fake, s.info.SessionName); len(got) == 0 {
				t.Fatal("the interrupt sent nothing")
			}
		})
	}
}

// A session-scoped interrupt with no fence to carry (a row with no token,
// or a runtime that cannot fence, AllowUnfenced) waits for the session
// mutation lock only until a ctx deadline, as a fenced one does: a lock
// still held then is an error wrapping runtime.ErrRuntimeUnavailable,
// nothing sent.
func TestUnfencedSessionInterruptBoundsTheLockWaitByTheDeadline(t *testing.T) {
	for _, tc := range []struct {
		name    string
		fencing runtime.InputFencing
		fenced  bool
	}{
		{"fenced", "", true},
		{"unfenceable runtime, AllowUnfenced", runtime.InputFencingNone, true},
		{"no token", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, sp, name, sf := fencedSessionHandle(t)
			if tc.fencing != "" {
				sp.InputFencingValue = tc.fencing
			}
			req := InterruptRequest{}
			if tc.fenced {
				req = InterruptRequest{Fence: sf, AllowUnfenced: true}
			}
			id := h.currentSessionID()
			held, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
			go func() {
				defer close(done)
				_ = sessionpkg.WithSessionMutationLock(id, func() error { close(held); <-release; return nil })
			}()
			<-held
			releaser := time.AfterFunc(2*time.Second, func() { close(release) })
			sp.Calls = nil
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			start := time.Now()
			_, err := h.InterruptAs(ctx, handleActor(t, h, sessionpkg.ActorController), req)
			took := time.Since(start)
			if releaser.Stop() {
				close(release)
			}
			<-done
			if took > time.Second {
				t.Fatalf("the interrupt waited %s for the session lock past its 100ms deadline", took)
			}
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, sessionpkg.ErrSessionMutationBusy) {
				t.Fatalf("InterruptAs(lock held past the deadline) = %v, want ErrRuntimeUnavailable and ErrSessionMutationBusy", err)
			}
			for _, m := range callMethodsOf(sp, name) {
				if inputMethods[m] {
					t.Fatalf("an interrupt that never took the lock sent %v", callMethodsOf(sp, name))
				}
			}
		})
	}
}

// A turn-scoped replacement whose preparation finds the runtime gone after
// the turn settled (an unfenced input clear on a runtime that stopped) says
// the replacement was not prepared: its interrupt was sent, so it is never
// read as a turn no longer current, nothing sent.
func TestTurnReplacementClearOnAGoneRuntimeIsNotPrepared(t *testing.T) {
	idle := newIdleScript(idleAnswer{idle: false}, idleAnswer{idle: true}, idleAnswer{idle: true})
	fake := idle.Fake
	s := newTurnSession(t, profiled(runtime.ProfileTmux, idle), "claude", "")
	timing, clock := racingTurnTiming(time.Date(2026, 10, 13, 6, 0, 0, 0, time.UTC))
	s.handle.turns.timing = timing
	path := s.writeTranscript(t, "claude", claudePrompt(claudeTurn1, "2026-10-13T05:59:50.123Z"))
	ageTranscript(t, path, clock.now().Add(-time.Minute))
	fake.ClearInputErrors[s.info.SessionName] = fmt.Errorf("tmux: %w", runtime.ErrSessionNotFound)
	fake.Calls = nil
	err := s.handle.Interrupt(context.Background(), InterruptRequest{Turn: claudeTurn1, ForReplacement: true})
	if !errors.Is(err, sessionpkg.ErrReplacementNotPrepared) || errors.Is(err, runtime.ErrTurnNotCurrent) {
		t.Fatalf("Interrupt(%s, replacement, clear finds the runtime gone) = %v, want ErrReplacementNotPrepared, not ErrTurnNotCurrent", claudeTurn1, err)
	}
}
