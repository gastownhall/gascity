package session

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// stopTurnRestart is a restart on failure by kind, with the session's own
// start command.
func stopTurnRestart(mgr *Manager, info Info, kind ActorKind) *StopTurnRestart {
	return &StopTurnRestart{By: testActor(mgr, kind), ResumeCommand: BuildResumeCommand(info), Hints: runtime.Config{WorkDir: info.WorkDir}}
}

// An interrupt whose idle wait fails, with a restart on failure, stops the
// runtime and starts it again in its place, and reports it; nothing is typed
// into the new runtime.
func TestStopTurnWithRestartReplacesTheRuntimeWhenTheTurnDoesNotEnd(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.WaitForIdleErrors[info.SessionName] = errors.New("still busy")
	report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if err != nil {
		t.Fatalf("StopTurnWith(restart) = %v, want nil", err)
	}
	if !report.Restarted {
		t.Fatal("report.Restarted = false, want the restart reported")
	}
	methods := callMethods(sp.SnapshotCalls())
	interrupt, stop, start := slices.Index(methods, "Interrupt"), slices.Index(methods, "Stop"), slices.Index(methods, "Start")
	if interrupt < 0 || stop < interrupt || start < stop {
		t.Fatalf("calls = %v, want Interrupt, then Stop, then Start", methods)
	}
	for _, m := range []string{"Nudge", "NudgeNow"} {
		if slices.Contains(methods, m) {
			t.Fatalf("calls = %v, want nothing typed into the restarted runtime", methods)
		}
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatal("the restarted runtime is not running")
	}
}

// A turn that settles is not restarted.
func TestStopTurnWithRestartLeavesASettledTurn(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if err != nil || report.Restarted {
		t.Fatalf("StopTurnWith(restart, settled) = %+v, %v; want no restart, nil", report, err)
	}
	if n := countMethod(sp.SnapshotCalls(), "Stop"); n != 0 {
		t.Fatalf("calls = %v, want no Stop", callMethods(sp.SnapshotCalls()))
	}
}

// A protocol runtime is never restarted: its stop may leave it running. Its
// not-ended error is returned as without a restart.
func TestStopTurnWithRestartSkipsAProtocolRuntime(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptProtocol)
	sp.WaitForIdleErrors[info.SessionName] = errors.New("still busy")
	report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if !errors.Is(err, runtime.ErrTurnNotEnded) || report.Restarted {
		t.Fatalf("StopTurnWith(restart, protocol) = %+v, %v; want ErrTurnNotEnded and no restart", report, err)
	}
	assertProtocolInterruptOnly(t, sp)
}

// A restart that fails is sent but not seen to end, and nothing else.
func TestStopTurnWithFailedRestartIsTurnNotEnded(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.WaitForIdleErrors[info.SessionName] = errors.New("still busy")
	sp.StartErrors[info.SessionName] = errors.New("cannot start")
	report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if !errors.Is(err, runtime.ErrTurnNotEnded) || report.Restarted {
		t.Fatalf("StopTurnWith(failed restart) = %+v, %v; want ErrTurnNotEnded and no restart", report, err)
	}
	if errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("StopTurnWith(failed restart) = %v, want ErrTurnNotEnded alone", err)
	}
}

// The runtime lease is taken before anything is sent: a lease held elsewhere
// refuses the stop as unavailable, nothing sent.
func TestStopTurnWithRestartTakesTheLeaseBeforeSending(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	_, release, err := mgr.leaseRuntime(context.Background(), testActor(mgr, ActorOperator), info.ID, info.SessionName, 0)
	if err != nil {
		t.Fatalf("holding the lease: %v", err)
	}
	defer release()
	_, err = mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("StopTurnWith(restart, lease held) = %v, want ErrRuntimeUnavailable", err)
	}
	if sent := inputCalls(sp.SnapshotCalls(), info.SessionName); len(sent) != 0 {
		t.Fatalf("a stop without its lease sent %v", callMethods(sent))
	}
}

// An actor that may not start refuses a restart before anything is sent.
func TestStopTurnWithRestartRefusesAStopSweep(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	_, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorSweep)})
	if !errors.Is(err, ErrSweepStarts) {
		t.Fatalf("StopTurnWith(restart by a sweep) = %v, want ErrSweepStarts", err)
	}
	if len(sp.SnapshotCalls()) != 0 {
		t.Fatalf("a refused restart made calls %v", callMethods(sp.SnapshotCalls()))
	}
}

// Settle decides the end after the stop's own waits, under the lock, and a
// not-ended answer is restarted on failure.
func TestStopTurnWithSettleDecidesTheEnd(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	var got []error
	settle := func(waitErr error) error {
		got = append(got, waitErr)
		ctx, cancel := context.WithTimeout(context.Background(), 1)
		defer cancel()
		if err := WithSessionMutationLockContext(ctx, info.ID, func() error { return nil }); !errors.Is(err, ErrSessionMutationBusy) {
			t.Errorf("taking the lock in Settle = %v, want ErrSessionMutationBusy", err)
		}
		return TurnNotEndedError("the turn", nil)
	}
	report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Settle: settle, Restart: stopTurnRestart(mgr, info, ActorController)})
	if err != nil || !report.Restarted {
		t.Fatalf("StopTurnWith(settle not ended, restart) = %+v, %v; want a restart, nil", report, err)
	}
	if len(got) != 1 || got[0] != nil {
		t.Fatalf("Settle got %v, want one call with the waits' nil", got)
	}
	if n := countMethod(sp.SnapshotCalls(), "Stop"); n != 1 {
		t.Fatalf("calls = %v, want one Stop", callMethods(sp.SnapshotCalls()))
	}

	nmgr, _, ninfo, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	_, err = nmgr.StopTurnWith(context.Background(), ninfo.ID, StopTurnOptions{Settle: func(error) error { return TurnNotEndedError("the turn", nil) }})
	if !errors.Is(err, runtime.ErrTurnNotEnded) {
		t.Fatalf("StopTurnWith(settle not ended) = %v, want ErrTurnNotEnded", err)
	}
}

// A replacement prepares the session as interrupt_now does between its
// interrupt and its message: Gemini's turn is reset and its input cleared,
// Claude's input box cleared; a plain stop resets nothing.
func TestStopTurnWithForReplacementPreparesTheSession(t *testing.T) {
	for _, tc := range []struct {
		provider string
		want     []string
	}{
		{"gemini", []string{"ResetInterruptedTurn", "SendKeys(C-u)"}},
		{"claude", []string{"ClearInput"}},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			mgr, sp, info, _ := newFencedStopSession(t, tc.provider, nil, runtime.TurnInterruptKeystroke)
			if _, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{ForReplacement: true}); err != nil {
				t.Fatalf("StopTurnWith(replacement): %v", err)
			}
			methods := callMethods(sp.SnapshotCalls())
			last := slices.Index(methods, "WaitForIdle")
			for _, m := range tc.want {
				if i := slices.Index(methods[last+1:], m); i < 0 {
					t.Fatalf("calls = %v, want %s after the idle wait", methods, m)
				}
			}
			pmgr, psp, pinfo, _ := newFencedStopSession(t, tc.provider, nil, runtime.TurnInterruptKeystroke)
			if err := pmgr.StopTurn(pinfo.ID); err != nil {
				t.Fatalf("StopTurn: %v", err)
			}
			if slices.Contains(callMethods(psp.SnapshotCalls()), "ResetInterruptedTurn") {
				t.Fatalf("a plain stop reset the turn: %v", callMethods(psp.SnapshotCalls()))
			}
		})
	}
}

// A replacement whose preparation fails reports it.
func TestStopTurnWithFailedReplacementIsAnError(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "gemini", nil, runtime.TurnInterruptKeystroke)
	sp.ResetTurnErrors[info.SessionName] = errors.New("reset refused")
	_, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{ForReplacement: true})
	if err == nil || errors.Is(err, runtime.ErrTurnNotEnded) {
		t.Fatalf("StopTurnWith(replacement, reset fails) = %v, want the reset's error", err)
	}
}

// ctxStartFake is the fake runtime with a Start that honors its context, as
// tmux's does, and a Stop that runs onStop first.
type ctxStartFake struct {
	*runtime.Fake
	onStop func()
}

func (f ctxStartFake) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.Fake.Start(ctx, name, cfg)
}

func (f ctxStartFake) Stop(name string) error {
	if f.onStop != nil {
		f.onStop()
	}
	return f.Fake.Stop(name)
}

// A caller whose context ended while the interrupted turn settled (a city
// stop, a lease budget) gets no restart: the runtime is not stopped, and the
// error wraps runtime.ErrRuntimeUnavailable, not ErrTurnNotEnded, so the
// caller releases the op rather than closing it failed.
func TestStopTurnWithRestartNeverRunsUnderAnEndedContext(t *testing.T) {
	for _, tc := range []struct {
		name string
		mk   func() (context.Context, context.CancelFunc)
	}{
		{"canceled", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), time.Hour)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := runtime.NewFake()
			mgr, info, _ := newFencedStopSessionOn(t, ctxStartFake{Fake: sp}, sp, "claude", nil, runtime.TurnInterruptKeystroke)
			ctx, cancel := tc.mk()
			defer cancel()
			settle := func(error) error {
				cancel()
				return TurnNotEndedError("interrupted turn", ctx.Err())
			}
			report, err := mgr.StopTurnWith(ctx, info.ID, StopTurnOptions{Settle: settle, Restart: stopTurnRestart(mgr, info, ActorController)})
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) || report.Restarted {
				t.Fatalf("StopTurnWith(ctx ended) = %+v, %v; want ErrRuntimeUnavailable alone and no restart", report, err)
			}
			if n := countMethod(sp.SnapshotCalls(), "Stop"); n != 0 || !sp.IsRunning(info.SessionName) {
				t.Fatalf("calls = %v, running %v; want the runtime left running", callMethods(sp.SnapshotCalls()), sp.IsRunning(info.SessionName))
			}
		})
	}
}

// Once the restart is decided it runs to the end: a caller context that ends
// after the runtime was stopped does not leave it stopped.
func TestStopTurnWithRestartOutlivesTheCallerContext(t *testing.T) {
	sp := runtime.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, info, _ := newFencedStopSessionOn(t, ctxStartFake{Fake: sp, onStop: cancel}, sp, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.WaitForIdleErrors[info.SessionName] = errors.New("still busy")
	report, err := mgr.StopTurnWith(ctx, info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if err != nil || !report.Restarted {
		t.Fatalf("StopTurnWith(ctx canceled at the restart's stop) = %+v, %v; want a restart, nil", report, err)
	}
	if !sp.IsRunning(info.SessionName) {
		t.Fatalf("calls = %v; the restarted runtime is not running", callMethods(sp.SnapshotCalls()))
	}
}

// A row whose drain-ack stop is pending belongs to the controller's stop: a
// restart on failure is refused before anything is sent, as interrupt_now
// refuses it.
func TestStopTurnWithRestartRefusesADrainAckStopPendingRow(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	if err := mgr.store.SetMetadataBatch(info.ID, map[string]string{"state": string(StateDraining), "state_reason": DrainAckStopPendingReason}); err != nil {
		t.Fatal(err)
	}
	sp.Calls = nil
	_, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if !errors.Is(err, ErrSessionStopping) {
		t.Fatalf("StopTurnWith(restart, drain-ack stop pending) = %v, want ErrSessionStopping", err)
	}
	if sent := inputCalls(sp.SnapshotCalls(), info.SessionName); len(sent) != 0 || countMethod(sp.SnapshotCalls(), "Stop") != 0 {
		t.Fatalf("a refused restart made calls %v", callMethods(sp.SnapshotCalls()))
	}
}

// A stop with a deadline waits for the session mutation lock only until it:
// a lock still held then is an error wrapping runtime.ErrRuntimeUnavailable,
// nothing sent. A stop with no deadline waits as before.
func TestStopTurnWithBoundsTheLockWaitByTheDeadline(t *testing.T) {
	mgr, sp, info, f := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	held, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = withSessionMutationLock(info.ID, func() error { close(held); <-release; return nil })
	}()
	<-held
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := mgr.StopTurnFenced(ctx, info.ID, f)
	took := time.Since(start)
	close(release)
	<-done
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, ErrSessionMutationBusy) {
		t.Fatalf("StopTurnFenced(lock held past the deadline) = %v, want ErrRuntimeUnavailable and ErrSessionMutationBusy", err)
	}
	if took > 5*time.Second {
		t.Fatalf("the stop waited %s for the lock past its 50ms deadline", took)
	}
	if sent := inputCalls(sp.SnapshotCalls(), info.SessionName); len(sent) != 0 {
		t.Fatalf("a stop that never took the lock sent %v", callMethods(sent))
	}
}

// A fenced replacement carries the fence on every input, the preparation's
// reset and input clear included.
func TestStopTurnWithForReplacementFencesEveryInput(t *testing.T) {
	for _, provider := range []string{"gemini", "claude"} {
		t.Run(provider, func(t *testing.T) {
			mgr, sp, info, f := newFencedStopSession(t, provider, nil, runtime.TurnInterruptKeystroke)
			if _, err := mgr.StopTurnWith(runtime.WithInputFence(context.Background(), f), info.ID, StopTurnOptions{ForReplacement: true}); err != nil {
				t.Fatalf("StopTurnWith(fenced replacement): %v", err)
			}
			sent := inputCalls(sp.SnapshotCalls(), info.SessionName)
			methods := callMethods(sent)
			if !slices.Contains(methods, "ClearInput") && !slices.Contains(methods, "SendKeys(C-u)") {
				t.Fatalf("inputs %v, want the preparation's input clear", methods)
			}
			for _, c := range sent {
				if c.Fence != f {
					t.Errorf("%s(%s) carries %v, want the fence", c.Method, c.Message, c.Fence)
				}
			}
		})
	}
}

// A preparation that fails after the turn settled says so apart from the
// interrupt's own errors: a fence miss on the clear is not "nothing sent".
func TestStopTurnWithFailedReplacementSaysTheTurnSettled(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "gemini", nil, runtime.TurnInterruptKeystroke)
	sp.ResetTurnErrors[info.SessionName] = &runtime.FenceMismatchError{Session: info.SessionName, Found: runtime.FenceFoundAbsent}
	_, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{ForReplacement: true})
	if !errors.Is(err, ErrReplacementNotPrepared) {
		t.Fatalf("StopTurnWith(replacement, reset misses) = %v, want ErrReplacementNotPrepared", err)
	}
}
