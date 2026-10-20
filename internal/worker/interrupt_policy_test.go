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

// A policy the handle cannot run is refused before anything is sent: an
// unknown on_failure, and a restart with no actor to start the runtime as.
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sp.Calls = nil
			err := tc.call()
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("Interrupt = %v, want refused (%v)", err, tc.want)
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
