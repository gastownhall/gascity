package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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

// A restart that stopped the runtime and could not start it again says so,
// apart from a turn not seen to end: the error wraps ErrRestartNotStarted
// and none of the start's own sentinels, the start's error in its text. The
// runtime stays stopped, for the controller's heal to bring back.
func TestStopTurnWithFailedStartIsRestartNotStarted(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.WaitForIdleErrors[info.SessionName] = errors.New("still busy")
	sp.StartErrors[info.SessionName] = fmt.Errorf("cannot start: %w", runtime.ErrRuntimeUnavailable)
	report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if !errors.Is(err, ErrRestartNotStarted) || report.Restarted {
		t.Fatalf("StopTurnWith(failed start) = %+v, %v; want ErrRestartNotStarted and no restart", report, err)
	}
	for _, sentinel := range []error{runtime.ErrTurnNotEnded, runtime.ErrRuntimeUnavailable, runtime.ErrSessionNotFound, ErrInterruptOutcomeUnknown} {
		if errors.Is(err, sentinel) {
			t.Fatalf("StopTurnWith(failed start) = %v, want ErrRestartNotStarted alone, not %v", err, sentinel)
		}
	}
	if !strings.Contains(err.Error(), "cannot start") {
		t.Fatalf("StopTurnWith(failed start) = %v, want the start's error in its text", err)
	}
	if n := countMethod(sp.SnapshotCalls(), "Stop"); n != 1 || sp.IsRunning(info.SessionName) {
		t.Fatalf("calls = %v, running %v; want the runtime stopped once and not started", callMethods(sp.SnapshotCalls()), sp.IsRunning(info.SessionName))
	}
}

// A restart whose stop fails leaves the runtime as it was: the interrupt
// was sent and its turn not seen to end, and nothing else.
func TestStopTurnWithFailedRestartStopIsTurnNotEnded(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.WaitForIdleErrors[info.SessionName] = errors.New("still busy")
	sp.StopErrors[info.SessionName] = errors.New("cannot stop")
	report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if !errors.Is(err, runtime.ErrTurnNotEnded) || report.Restarted {
		t.Fatalf("StopTurnWith(failed stop) = %+v, %v; want ErrTurnNotEnded and no restart", report, err)
	}
	if errors.Is(err, ErrRestartNotStarted) || errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrSessionNotFound) {
		t.Fatalf("StopTurnWith(failed stop) = %v, want ErrTurnNotEnded alone", err)
	}
	if n := countMethod(sp.SnapshotCalls(), "Start"); n != 0 {
		t.Fatalf("calls = %v, want no start after a failed stop", callMethods(sp.SnapshotCalls()))
	}
}

// A follow-up interrupt its check refused is no ground for a restart: the
// check saw the turn leave Current after the first key, which is the turn's
// end (Settle gets a nil wait for it), or it could not tell. Either way
// whatever runs now may be another turn, the ended one's successor, which a
// restart would kill. When Settle does not see the end either (its reads
// fail), the runtime is left running and the error wraps
// ErrInterruptOutcomeUnknown and runtime.ErrRuntimeUnavailable, not
// runtime.ErrTurnNotEnded, and none of the refusal's own sentinels.
func TestStopTurnWithRefusedFollowUpIsNoRestart(t *testing.T) {
	for _, tc := range []struct {
		name    string
		refusal error
		ended   bool // the refusal is the turn's end: Settle gets a nil wait
	}{
		{"the turn left current", &runtime.TurnNotCurrentError{Session: "s", TurnID: "t1", Current: runtime.TurnState{TurnID: "t2"}}, true},
		{"the check could not answer", fmt.Errorf("checking turn %q of s before interrupting it: %w: %w", "t1", runtime.ErrRuntimeUnavailable, context.DeadlineExceeded), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, sp, info, _ := newFencedStopSession(t, "codex", nil, runtime.TurnInterruptKeystroke)
			checks := 0
			still := func() error {
				if checks++; checks > 1 {
					return tc.refusal
				}
				return nil
			}
			var waits []error
			settle := func(waitErr error) error {
				waits = append(waits, waitErr)
				return TurnNotEndedError("interrupted turn \"t1\"", errors.New("reading the transcript: input/output error"))
			}
			report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Still: still, Settle: settle, Restart: stopTurnRestart(mgr, info, ActorController)})
			if report.Restarted || !errors.Is(err, ErrInterruptOutcomeUnknown) || !errors.Is(err, runtime.ErrRuntimeUnavailable) {
				t.Fatalf("StopTurnWith(follow-up refused) = %+v, %v; want ErrInterruptOutcomeUnknown and ErrRuntimeUnavailable, and no restart", report, err)
			}
			if errors.Is(err, runtime.ErrTurnNotEnded) || errors.Is(err, runtime.ErrTurnNotCurrent) {
				t.Fatalf("StopTurnWith(follow-up refused) = %v, want neither ErrTurnNotEnded nor the refusal's ErrTurnNotCurrent", err)
			}
			if n := countMethod(sp.SnapshotCalls(), "Stop") + countMethod(sp.SnapshotCalls(), "Start"); n != 0 || !sp.IsRunning(info.SessionName) {
				t.Fatalf("calls = %v, running %v; want the runtime left running", callMethods(sp.SnapshotCalls()), sp.IsRunning(info.SessionName))
			}
			if got, want := callMethods(inputCalls(sp.SnapshotCalls(), info.SessionName)), []string{"SendKeys(Escape)"}; !slices.Equal(got, want) {
				t.Fatalf("sent %v, want %v: no fallback after a refusal", got, want)
			}
			if len(waits) != 1 || (waits[0] == nil) != tc.ended {
				t.Fatalf("Settle got %v, want one wait, nil %v", waits, tc.ended)
			}
			if !tc.ended && (!errors.Is(waits[0], runtime.ErrTurnNotEnded) || errors.Is(waits[0], runtime.ErrRuntimeUnavailable)) {
				t.Fatalf("Settle got %v, want ErrTurnNotEnded alone, the refusal in its text", waits[0])
			}
		})
	}
}

// The runtime lease is taken before the check and before anything is sent:
// the check runs under it, and a lease held elsewhere refuses the stop as
// unavailable, the check not run and nothing sent.
func TestStopTurnWithRestartTakesTheLeaseBeforeSending(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	refused := errors.New("not the current turn")
	var leaseAtCheck error
	_, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{
		Restart: stopTurnRestart(mgr, info, ActorController),
		Still: func() error {
			_, release, err := mgr.leaseRuntime(context.Background(), testActor(mgr, ActorOperator), info.ID, info.SessionName, 0)
			if err == nil {
				release()
			}
			leaseAtCheck = err
			return refused
		},
	})
	if !errors.Is(err, refused) || leaseAtCheck == nil {
		t.Fatalf("StopTurnWith(restart, refusing check) = %v, the check's own lease attempt = %v; want the refusal, the check run under the stop's lease", err, leaseAtCheck)
	}
	_, release, err := mgr.leaseRuntime(context.Background(), testActor(mgr, ActorOperator), info.ID, info.SessionName, 0)
	if err != nil {
		t.Fatalf("holding the lease after the refused stop released it: %v", err)
	}
	defer release()
	checked := false
	_, err = mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{
		Restart: stopTurnRestart(mgr, info, ActorController),
		Still:   func() error { checked = true; return nil },
	})
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || checked {
		t.Fatalf("StopTurnWith(restart, lease held) = %v, check run %v; want ErrRuntimeUnavailable before the check", err, checked)
	}
	if sent := inputCalls(sp.SnapshotCalls(), info.SessionName); len(sent) != 0 {
		t.Fatalf("a stop without its lease sent %v", callMethods(sent))
	}
}

// An actor that may not start refuses a restart before anything is sent,
// and says nothing was (ErrStopNotSent).
func TestStopTurnWithRestartRefusesAStopSweep(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	_, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorSweep)})
	if !errors.Is(err, ErrSweepStarts) || !errors.Is(err, ErrStopNotSent) {
		t.Fatalf("StopTurnWith(restart by a sweep) = %v, want ErrSweepStarts and ErrStopNotSent", err)
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

// A Settle that saw nothing showing the turn still running
// (TurnEndUnreadError: its reads all failed) is no ground for a restart: the
// runtime is left running, and the error wraps ErrInterruptOutcomeUnknown and
// runtime.ErrRuntimeUnavailable, not runtime.ErrTurnNotEnded. Without a
// restart the stop returns Settle's error: sent, not seen to end, and
// nothing else.
func TestStopTurnWithUnreadSettleIsNoRestart(t *testing.T) {
	unread := func(error) error {
		return TurnEndUnreadError("interrupted turn \"t1\"", fmt.Errorf("reading the transcript: %w", runtime.ErrRuntimeUnavailable))
	}
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Settle: unread, Restart: stopTurnRestart(mgr, info, ActorController)})
	if report.Restarted || !errors.Is(err, ErrInterruptOutcomeUnknown) || !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) {
		t.Fatalf("StopTurnWith(restart, settle read nothing) = %+v, %v; want ErrInterruptOutcomeUnknown and ErrRuntimeUnavailable, and no restart", report, err)
	}
	if n := countMethod(sp.SnapshotCalls(), "Stop") + countMethod(sp.SnapshotCalls(), "Start"); n != 0 || !sp.IsRunning(info.SessionName) {
		t.Fatalf("calls = %v, running %v; want the runtime left running", callMethods(sp.SnapshotCalls()), sp.IsRunning(info.SessionName))
	}
	_, err = mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Settle: unread})
	if !errors.Is(err, runtime.ErrTurnNotEnded) || errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, ErrInterruptOutcomeUnknown) {
		t.Fatalf("StopTurnWith(settle read nothing) = %v, want ErrTurnNotEnded alone", err)
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

// A replacement whose turn does not settle, with a restart on failure, is
// restarted and not prepared: the new runtime has no interrupted turn to
// discard and no input to clear, and nothing is typed into it.
func TestStopTurnWithRestartedReplacementIsNotPrepared(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.WaitForIdleErrors[info.SessionName] = errors.New("still busy")
	report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{ForReplacement: true, Restart: stopTurnRestart(mgr, info, ActorController)})
	if err != nil || !report.Restarted {
		t.Fatalf("StopTurnWith(replacement, restart, not settled) = %+v, %v; want a restart, nil", report, err)
	}
	methods := callMethods(sp.SnapshotCalls())
	if i, j := slices.Index(methods, "Stop"), slices.Index(methods, "Start"); i < 0 || j < i {
		t.Fatalf("calls = %v, want Stop then Start", methods)
	}
	if got, want := callMethods(inputCalls(sp.SnapshotCalls(), info.SessionName)), []string{"Interrupt"}; !slices.Equal(got, want) {
		t.Fatalf("sent %v, want only %v: the restarted runtime is not prepared for the replacement", got, want)
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
// error wraps runtime.ErrRuntimeUnavailable and ErrInterruptOutcomeUnknown,
// not ErrTurnNotEnded: the interrupt was sent, and whether it ended the turn
// is unknown.
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
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, ErrInterruptOutcomeUnknown) || errors.Is(err, runtime.ErrTurnNotEnded) || report.Restarted {
				t.Fatalf("StopTurnWith(ctx ended) = %+v, %v; want ErrRuntimeUnavailable and ErrInterruptOutcomeUnknown, and no restart", report, err)
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

// staleUntilRestart is a cached IsRunning that reads sp's session name not
// running, as the tmux state cache can read a live session, until the
// runtime is started again: a provider's own start refreshes its cache.
func staleUntilRestart(sp *runtime.Fake, name string) func() bool {
	return func() bool { return countMethod(sp.SnapshotCalls(), "Start") > 0 && sp.IsRunning(name) }
}

// Kills a stop with a restart on failure, or for a replacement, that takes a
// cached "not running" for no turn. Unfenced and unchecked, as a
// session-scoped interrupt with no token or on a runtime that cannot fence
// sends it, whether the runtime runs, before the lease and again before the
// keys, is a fresh liveness read's, as a checked stop's is: a stale cache
// would send nothing and report success, so the turn runs on and its
// restart never runs. One that finds the agent up is interrupted whatever
// IsRunning says, then restarted when its turn does not settle, or prepared
// for its replacement; one whose agent exited, its pane up, is sent nothing
// and not restarted; one that cannot answer is runtime.ErrRuntimeUnavailable,
// nothing sent, never success.
func TestStopTurnWithPolicyDecidesTheRuntimeOnAFreshRead(t *testing.T) {
	restart := func(mgr *Manager, info Info) StopTurnOptions {
		return StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)}
	}
	for _, when := range []string{"before the lease", "at the keys"} {
		t.Run("restart, stale cache "+when, func(t *testing.T) {
			mgr, sp, info, cache := newCachedRunningSession(t)
			sp.WaitForIdleErrors[info.SessionName] = errors.New("still busy")
			stale := staleUntilRestart(sp, info.SessionName)
			if when == "before the lease" {
				cache.set(stale, nil)
			} else {
				cache.onMode = func() { cache.set(stale, nil) }
			}
			report, err := mgr.StopTurnWith(context.Background(), info.ID, restart(mgr, info))
			if err != nil || !report.Restarted {
				t.Fatalf("StopTurnWith(restart, stale cache %s) = %+v, %v; want the turn interrupted and the runtime restarted", when, report, err)
			}
			methods := callMethods(sp.SnapshotCalls())
			interrupt, stop, start := slices.Index(methods, "Interrupt"), slices.Index(methods, "Stop"), slices.Index(methods, "Start")
			if interrupt < 0 || stop < interrupt || start < stop {
				t.Fatalf("calls = %v, want Interrupt, then Stop, then Start", methods)
			}
		})
	}
	t.Run("replacement, stale cache", func(t *testing.T) {
		mgr, sp, info, cache := newCachedRunningSession(t)
		cache.set(staleUntilRestart(sp, info.SessionName), nil)
		if _, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{ForReplacement: true}); err != nil {
			t.Fatalf("StopTurnWith(replacement, stale cache) = %v, want nil", err)
		}
		if got, want := callMethods(inputCalls(sp.SnapshotCalls(), info.SessionName)), []string{"Interrupt", "ClearInput"}; !slices.Equal(got, want) {
			t.Fatalf("sent %v, want %v: the turn interrupted and the input box cleared for its replacement", got, want)
		}
	})
	t.Run("restart, agent exited", func(t *testing.T) {
		mgr, sp, info, _ := newCachedRunningSession(t)
		sp.WaitForIdleErrors[info.SessionName] = errors.New("still busy")
		sp.Zombies[info.SessionName] = true // the pane lives on, its agent exited
		report, err := mgr.StopTurnWith(context.Background(), info.ID, restart(mgr, info))
		if err != nil || report.Restarted {
			t.Fatalf("StopTurnWith(restart, agent exited) = %+v, %v; want nil, no turn and no restart", report, err)
		}
		if sent := inputCalls(sp.SnapshotCalls(), info.SessionName); len(sent) != 0 || countMethod(sp.SnapshotCalls(), "Stop") != 0 {
			t.Fatalf("calls = %v, want nothing sent into a pane whose agent exited, and no restart", callMethods(sp.SnapshotCalls()))
		}
	})
	for _, when := range []string{"before the lease", "at the keys"} {
		t.Run("restart, unreadable "+when, func(t *testing.T) {
			mgr, sp, info, cache := newCachedRunningSession(t)
			sp.WaitForIdleErrors[info.SessionName] = errors.New("still busy")
			unreadable := errors.New("tmux: list-panes failed")
			if when == "before the lease" {
				cache.set(nil, unreadable)
			} else {
				cache.onMode = func() { cache.set(nil, unreadable) }
			}
			report, err := mgr.StopTurnWith(context.Background(), info.ID, restart(mgr, info))
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrSessionNotFound) || errors.Is(err, runtime.ErrTurnNotEnded) || report.Restarted {
				t.Fatalf("StopTurnWith(restart, unreadable %s) = %+v, %v; want ErrRuntimeUnavailable alone, no restart", when, report, err)
			}
			if sent := inputCalls(sp.SnapshotCalls(), info.SessionName); len(sent) != 0 || countMethod(sp.SnapshotCalls(), "Stop") != 0 {
				t.Fatalf("calls = %v, want nothing sent on a read that could not answer", callMethods(sp.SnapshotCalls()))
			}
		})
	}
}

// rowLoadStore is a store whose Get answers from load.
type rowLoadStore struct {
	beads.Store
	load func(id string) (beads.Bead, error)
}

func (s rowLoadStore) Get(id string) (beads.Bead, error) { return s.load(id) }

// A stop whose load of the row under the session mutation lock fails sends
// nothing and says so, plain, checked or with a restart on failure alike:
// the error wraps ErrStopNotSent, and the load's own sentinel for a row
// closed, gone or not a session.
func TestStopTurnWithRowLoadFailureIsNotSent(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  func(t *testing.T, mgr *Manager, info Info)
		want error // the load's own sentinel, if any
	}{
		{"closed", func(t *testing.T, mgr *Manager, info Info) {
			if err := mgr.store.Close(info.ID); err != nil {
				t.Fatal(err)
			}
		}, ErrSessionClosed},
		{"gone", func(_ *testing.T, mgr *Manager, _ Info) {
			mgr.store = rowLoadStore{Store: mgr.store, load: func(id string) (beads.Bead, error) {
				return beads.Bead{}, fmt.Errorf("getting bead %q: %w", id, beads.ErrNotFound)
			}}
		}, beads.ErrNotFound},
		{"not a session", func(_ *testing.T, mgr *Manager, _ Info) {
			mgr.store = rowLoadStore{Store: mgr.store, load: func(id string) (beads.Bead, error) {
				return beads.Bead{ID: id, Type: "task", Status: "open"}, nil
			}}
		}, ErrNotSession},
		{"unreadable", func(_ *testing.T, mgr *Manager, _ Info) {
			mgr.store = rowLoadStore{Store: mgr.store, load: func(string) (beads.Bead, error) {
				return beads.Bead{}, errors.New("store: connection reset by peer")
			}}
		}, nil},
	} {
		for _, stop := range []struct {
			name string
			opts func(mgr *Manager, info Info) StopTurnOptions
		}{
			{"plain", func(*Manager, Info) StopTurnOptions { return StopTurnOptions{} }},
			{"checked", func(*Manager, Info) StopTurnOptions { return StopTurnOptions{Still: func() error { return nil }} }},
			{"restart on failure", func(mgr *Manager, info Info) StopTurnOptions {
				return StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)}
			}},
		} {
			t.Run(tc.name+"/"+stop.name, func(t *testing.T) {
				mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
				opts := stop.opts(mgr, info)
				tc.row(t, mgr, info)
				_, err := mgr.StopTurnWith(context.Background(), info.ID, opts)
				if !errors.Is(err, ErrStopNotSent) || (tc.want != nil && !errors.Is(err, tc.want)) {
					t.Fatalf("StopTurnWith(row %s) = %v, want ErrStopNotSent and %v", tc.name, err, tc.want)
				}
				if errors.Is(err, runtime.ErrTurnNotEnded) || errors.Is(err, ErrInterruptOutcomeUnknown) {
					t.Fatalf("StopTurnWith(row %s) = %v, want nothing sent, not sent or unknown", tc.name, err)
				}
				if got := sp.SnapshotCalls(); len(got) != 0 {
					t.Fatalf("a stop whose row would not load made calls %v", callMethods(got))
				}
			})
		}
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
	// The holder lets go on its own well after the deadline, so a stop that
	// ignores the deadline returns late instead of hanging the suite.
	releaser := time.AfterFunc(2*time.Second, func() { close(release) })
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	err := mgr.StopTurnFenced(ctx, info.ID, f)
	took := time.Since(start)
	if releaser.Stop() {
		close(release)
	}
	<-done
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, ErrSessionMutationBusy) {
		t.Fatalf("StopTurnFenced(lock held past the deadline) = %v, want ErrRuntimeUnavailable and ErrSessionMutationBusy", err)
	}
	if took > time.Second {
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

// failingInterruptFake is the fake runtime whose interrupt fails with err,
// by default after it may have reached the agent, as a broken tmux pipe
// does.
type failingInterruptFake struct {
	*runtime.Fake
	err error
}

func (f failingInterruptFake) fail() error {
	if f.err != nil {
		return f.err
	}
	return errors.New("tmux: broken pipe")
}

func (f failingInterruptFake) Interrupt(name string) error {
	_ = f.Fake.Interrupt(name)
	return f.fail()
}

func (f failingInterruptFake) InterruptFenced(ctx context.Context, name string, fence runtime.InputFence) error {
	if err := f.Fake.InterruptFenced(ctx, name, fence); err != nil {
		return err
	}
	return f.fail()
}

// A checked stop whose send fails after its check passed may have reached
// the agent: the error wraps ErrInterruptOutcomeUnknown (and, as before,
// runtime.ErrRuntimeUnavailable). So does a runtime that lost the session
// after its input was applied (runtime.ErrNudgeDeliveryUnconfirmed), though
// it says unavailable too; one that could not tell before its key says
// unavailable alone, nothing sent. A check that refuses sends nothing and
// does not.
func TestStopTurnWithSaysWhenASentInterruptsOutcomeIsUnknown(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       error
		maybeSent bool
	}{
		{"send failed", nil, true},
		{"session lost after the key", fmt.Errorf("%w after input was applied: %w", runtime.ErrNudgeDeliveryUnconfirmed,
			fmt.Errorf("tmux server lost: %w", runtime.ErrRuntimeUnavailable)), true},
		{"unanswered before the key", fmt.Errorf("session %q: fence read: %w", "s", runtime.ErrRuntimeUnavailable), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := runtime.NewFake()
			mgr, info, _ := newFencedStopSessionOn(t, failingInterruptFake{Fake: sp, err: tc.err}, sp, "claude", nil, runtime.TurnInterruptKeystroke)
			_, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Still: func() error { return nil }})
			if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, ErrInterruptOutcomeUnknown) != tc.maybeSent {
				t.Fatalf("StopTurnWith(%s) = %v, want ErrRuntimeUnavailable, ErrInterruptOutcomeUnknown %v", tc.name, err, tc.maybeSent)
			}
		})
	}
	sp := runtime.NewFake()
	mgr, info, _ := newFencedStopSessionOn(t, failingInterruptFake{Fake: sp}, sp, "claude", nil, runtime.TurnInterruptKeystroke)
	refused := errors.New("not the current turn")
	_, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Still: func() error { return refused }})
	if !errors.Is(err, refused) || errors.Is(err, ErrInterruptOutcomeUnknown) {
		t.Fatalf("StopTurnWith(check refuses) = %v, want the refusal and not ErrInterruptOutcomeUnknown", err)
	}
}

// newKeystrokeTurnFenceSession is a session on a runtime that fences turns
// and interrupts by keystroke mode (ACP: its Interrupt is session/cancel and
// its stop replaces the agent), on rt over sp, scripted by turns.
func newKeystrokeTurnFenceSession(t *testing.T, rt runtime.Provider, sp *runtime.Fake, turns ...string) (*Manager, Info) {
	t.Helper()
	mgr, info, _ := newFencedStopSessionOn(t, rt, sp, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.TurnScripts = map[string][]string{info.SessionName: turns}
	return mgr, info
}

// A protocol turn interrupt with a restart on failure, on a keystroke
// runtime (ACP), interrupts the turn first and replaces the runtime only
// once the turn is not seen to end, as a stop's restart does: the restart is
// reported, nothing is typed into the new runtime, and a turn that ends is
// left alone. A protocol runtime is never restarted: its stop may leave it
// running, so its not-ended error is returned.
func TestInterruptTurnRestartOnFailure(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      runtime.TurnInterruptMode
		turns     []string
		restarted bool
		notEnded  bool
	}{
		{"keystroke runtime, turn not seen to end", runtime.TurnInterruptKeystroke, []string{"t1"}, true, false},
		{"keystroke runtime, turn ends", runtime.TurnInterruptKeystroke, []string{"t1", ""}, false, false},
		{"protocol runtime, turn not seen to end", runtime.TurnInterruptProtocol, []string{"t1"}, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := runtime.NewFake()
			mgr, info, _ := newFencedStopSessionOn(t, runtime.FullFake{Fake: sp}, sp, "claude", nil, tc.mode)
			sp.TurnScripts = map[string][]string{info.SessionName: tc.turns}
			_, report, err := mgr.InterruptTurn(context.Background(), info.ID, "t1", stopTurnRestart(mgr, info, ActorController))
			if report.Restarted != tc.restarted || errors.Is(err, runtime.ErrTurnNotEnded) != tc.notEnded || (err != nil) != tc.notEnded {
				t.Fatalf("InterruptTurn(restart) = %+v, %v; want restarted %v, not ended %v", report, err, tc.restarted, tc.notEnded)
			}
			methods := callMethods(sp.SnapshotCalls())
			interrupt, stop, start := slices.Index(methods, "InterruptTurn"), slices.Index(methods, "Stop"), slices.Index(methods, "Start")
			if interrupt < 0 {
				t.Fatalf("calls = %v, want the turn interrupt sent", methods)
			}
			if !tc.restarted {
				if stop >= 0 || start >= 0 {
					t.Fatalf("calls = %v, want no restart", methods)
				}
				return
			}
			if stop < interrupt || start < stop {
				t.Fatalf("calls = %v, want InterruptTurn, then Stop, then Start", methods)
			}
			for _, m := range []string{"Nudge", "NudgeNow", "SendKeys", "Interrupt"} {
				if slices.Contains(methods, m) {
					t.Fatalf("calls = %v, want nothing but the turn interrupt and the restart", methods)
				}
			}
			if !sp.IsRunning(info.SessionName) {
				t.Fatal("the restarted runtime is not running")
			}
		})
	}
}

// A protocol turn interrupt's restart on failure is refused before anything
// is sent, as a stop's is: an actor that may not start the runtime, a row
// whose drain-ack stop is pending, and a runtime lease held by another.
func TestInterruptTurnRestartRefusalsSendNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kind  ActorKind
		setup func(t *testing.T, mgr *Manager, info Info)
		want  error
	}{
		{"a stop sweep may not start", ActorSweep, nil, ErrSweepStarts},
		{"a drain-ack stop is pending", ActorController, func(t *testing.T, mgr *Manager, info Info) {
			if err := mgr.store.SetMetadataBatch(info.ID, map[string]string{"state": string(StateDraining), "state_reason": DrainAckStopPendingReason}); err != nil {
				t.Fatal(err)
			}
		}, ErrSessionStopping},
		{"the runtime lease is held", ActorController, func(t *testing.T, mgr *Manager, info Info) {
			_, release, err := mgr.leaseRuntime(context.Background(), testActor(mgr, ActorOperator), info.ID, info.SessionName, 0)
			if err != nil {
				t.Fatalf("holding the lease: %v", err)
			}
			t.Cleanup(release)
		}, runtime.ErrRuntimeUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := runtime.NewFake()
			mgr, info := newKeystrokeTurnFenceSession(t, runtime.FullFake{Fake: sp}, sp, "t1")
			if tc.setup != nil {
				tc.setup(t, mgr, info)
			}
			sp.Calls = nil
			_, report, err := mgr.InterruptTurn(context.Background(), info.ID, "t1", stopTurnRestart(mgr, info, tc.kind))
			if !errors.Is(err, tc.want) || errors.Is(err, ErrInterruptOutcomeUnknown) || report.Restarted {
				t.Fatalf("InterruptTurn(restart, %s) = %+v, %v; want %v, nothing sent", tc.name, report, err, tc.want)
			}
			if got := turnFenceCalls(sp); len(got) != 0 || countMethod(sp.SnapshotCalls(), "Stop") != 0 {
				t.Fatalf("a refused restart made calls %v", callMethods(sp.SnapshotCalls()))
			}
		})
	}
}

// cancelingTurnWait is a turn fence whose WaitTurnEnded ends the caller's
// context first, as a city stop or a lease budget ending mid-wait does.
type cancelingTurnWait struct {
	runtime.FullFake
	cancel context.CancelFunc
}

func (p cancelingTurnWait) WaitTurnEnded(ctx context.Context, _, _ string, _ time.Duration) (runtime.TurnState, error) {
	p.cancel()
	return runtime.TurnState{}, ctx.Err()
}

// A caller whose context ended while the interrupted turn settled gets no
// restart, as from a stop: the runtime is left running, and the error wraps
// ErrInterruptOutcomeUnknown and runtime.ErrRuntimeUnavailable, not
// runtime.ErrTurnNotEnded.
func TestInterruptTurnRestartNeverRunsUnderAnEndedContext(t *testing.T) {
	sp := runtime.NewFake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	mgr, info := newKeystrokeTurnFenceSession(t, cancelingTurnWait{FullFake: runtime.FullFake{Fake: sp}, cancel: cancel}, sp, "t1")
	_, report, err := mgr.InterruptTurn(ctx, info.ID, "t1", stopTurnRestart(mgr, info, ActorController))
	if !errors.Is(err, ErrInterruptOutcomeUnknown) || !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) || report.Restarted {
		t.Fatalf("InterruptTurn(restart, ctx ended) = %+v, %v; want ErrInterruptOutcomeUnknown and no restart", report, err)
	}
	if n := countMethod(sp.SnapshotCalls(), "Stop"); n != 0 || !sp.IsRunning(info.SessionName) {
		t.Fatalf("calls = %v, running %v; want the runtime left running", callMethods(sp.SnapshotCalls()), sp.IsRunning(info.SessionName))
	}
}

// A protocol turn interrupt that fails before it sends anything for a cause
// of its own says so, as a stop does (ErrStopNotSent), with the cause's own
// sentinels: its row, loaded under the session mutation lock, closed, gone,
// not a session or unreadable, with or without a restart on failure, and an
// actor that may not start the runtime refused a restart.
func TestInterruptTurnRowLoadFailureIsNotSent(t *testing.T) {
	for _, tc := range []struct {
		name string
		row  func(t *testing.T, mgr *Manager, info Info)
		want error // the load's own sentinel, if any
	}{
		{"closed", func(t *testing.T, mgr *Manager, info Info) {
			if err := mgr.store.Close(info.ID); err != nil {
				t.Fatal(err)
			}
		}, ErrSessionClosed},
		{"gone", func(_ *testing.T, mgr *Manager, _ Info) {
			mgr.store = rowLoadStore{Store: mgr.store, load: func(id string) (beads.Bead, error) {
				return beads.Bead{}, fmt.Errorf("getting bead %q: %w", id, beads.ErrNotFound)
			}}
		}, beads.ErrNotFound},
		{"not a session", func(_ *testing.T, mgr *Manager, _ Info) {
			mgr.store = rowLoadStore{Store: mgr.store, load: func(id string) (beads.Bead, error) {
				return beads.Bead{ID: id, Type: "task", Status: "open"}, nil
			}}
		}, ErrNotSession},
		{"unreadable", func(_ *testing.T, mgr *Manager, _ Info) {
			mgr.store = rowLoadStore{Store: mgr.store, load: func(string) (beads.Bead, error) {
				return beads.Bead{}, errors.New("store: connection reset by peer")
			}}
		}, nil},
	} {
		for _, restarts := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/restart %v", tc.name, restarts), func(t *testing.T) {
				sp := runtime.NewFake()
				mgr, info := newKeystrokeTurnFenceSession(t, runtime.FullFake{Fake: sp}, sp, "t1")
				var restart *StopTurnRestart
				if restarts {
					restart = stopTurnRestart(mgr, info, ActorController)
				}
				tc.row(t, mgr, info)
				sp.Calls = nil
				_, _, err := mgr.InterruptTurn(context.Background(), info.ID, "t1", restart)
				if !errors.Is(err, ErrStopNotSent) || (tc.want != nil && !errors.Is(err, tc.want)) {
					t.Fatalf("InterruptTurn(row %s) = %v, want ErrStopNotSent and %v", tc.name, err, tc.want)
				}
				if errors.Is(err, runtime.ErrTurnNotEnded) || errors.Is(err, ErrInterruptOutcomeUnknown) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("InterruptTurn(row %s) = %v, want nothing sent, not sent, unknown or unavailable", tc.name, err)
				}
				if got := sp.SnapshotCalls(); len(got) != 0 {
					t.Fatalf("an interrupt whose row would not load made calls %v", callMethods(got))
				}
			})
		}
	}
	sp := runtime.NewFake()
	mgr, info := newKeystrokeTurnFenceSession(t, runtime.FullFake{Fake: sp}, sp, "t1")
	sp.Calls = nil
	if _, _, err := mgr.InterruptTurn(context.Background(), info.ID, "t1", stopTurnRestart(mgr, info, ActorSweep)); !errors.Is(err, ErrStopNotSent) || !errors.Is(err, ErrSweepStarts) {
		t.Fatalf("InterruptTurn(restart by a sweep) = %v, want ErrStopNotSent and ErrSweepStarts", err)
	}
	if got := sp.SnapshotCalls(); len(got) != 0 {
		t.Fatalf("a refused restart made calls %v", callMethods(got))
	}
}

// unreadTurnWait is a turn fence whose WaitTurnEnded reads nothing, as an ACP
// owner or a bridge that stopped answering leaves it: it returns err.
type unreadTurnWait struct {
	runtime.FullFake
	err error
}

func (p unreadTurnWait) WaitTurnEnded(context.Context, string, string, time.Duration) (runtime.TurnState, error) {
	return runtime.TurnState{}, p.err
}

// A protocol turn interrupt whose wait read nothing (no read answered within
// its bound, or the runtime cannot answer the wait at all) saw nothing
// showing the turn still running, so a restart on failure is withheld: the
// runtime is left running, and the error wraps ErrInterruptOutcomeUnknown
// and runtime.ErrRuntimeUnavailable, not runtime.ErrTurnNotEnded. Without a
// restart the error is sent and not seen to end, and nothing else.
func TestInterruptTurnUnreadWaitIsNoRestart(t *testing.T) {
	unanswered := fmt.Errorf("%w: the owner did not answer", runtime.ErrRuntimeUnavailable)
	for _, tc := range []struct {
		name string
		wait error
	}{
		{"no read answered within the bound", fmt.Errorf("%w: session %q turn %q after %s: last read: %w", runtime.ErrTurnNotEnded, "s", "t1", turnSettleTimeout, unanswered)},
		{"the runtime cannot answer the wait", unanswered},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := runtime.NewFake()
			mgr, info := newKeystrokeTurnFenceSession(t, unreadTurnWait{FullFake: runtime.FullFake{Fake: sp}, err: tc.wait}, sp, "t1")
			_, report, err := mgr.InterruptTurn(context.Background(), info.ID, "t1", stopTurnRestart(mgr, info, ActorController))
			if report.Restarted || !errors.Is(err, ErrInterruptOutcomeUnknown) || !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) {
				t.Fatalf("InterruptTurn(restart, wait read nothing) = %+v, %v; want ErrInterruptOutcomeUnknown and ErrRuntimeUnavailable, and no restart", report, err)
			}
			if n := countMethod(sp.SnapshotCalls(), "Stop") + countMethod(sp.SnapshotCalls(), "Start"); n != 0 || !sp.IsRunning(info.SessionName) {
				t.Fatalf("calls = %v, running %v; want the runtime left running", callMethods(sp.SnapshotCalls()), sp.IsRunning(info.SessionName))
			}
			_, _, err = mgr.InterruptTurn(context.Background(), info.ID, "t1", nil)
			if !errors.Is(err, runtime.ErrTurnNotEnded) || errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, ErrInterruptOutcomeUnknown) {
				t.Fatalf("InterruptTurn(wait read nothing) = %v, want ErrTurnNotEnded alone", err)
			}
		})
	}
}

// A protocol turn interrupt's restart that stopped the runtime and could not
// start it again says so, as a stop's does: ErrRestartNotStarted and none of
// the start's own sentinels.
func TestInterruptTurnFailedRestartStartIsRestartNotStarted(t *testing.T) {
	sp := runtime.NewFake()
	mgr, info := newKeystrokeTurnFenceSession(t, runtime.FullFake{Fake: sp}, sp, "t1")
	sp.StartErrors[info.SessionName] = fmt.Errorf("cannot start: %w", runtime.ErrRuntimeUnavailable)
	_, report, err := mgr.InterruptTurn(context.Background(), info.ID, "t1", stopTurnRestart(mgr, info, ActorController))
	if !errors.Is(err, ErrRestartNotStarted) || report.Restarted {
		t.Fatalf("InterruptTurn(restart, failed start) = %+v, %v; want ErrRestartNotStarted and no restart", report, err)
	}
	for _, sentinel := range []error{runtime.ErrTurnNotEnded, runtime.ErrRuntimeUnavailable, runtime.ErrSessionNotFound, ErrInterruptOutcomeUnknown} {
		if errors.Is(err, sentinel) {
			t.Fatalf("InterruptTurn(restart, failed start) = %v, want ErrRestartNotStarted alone, not %v", err, sentinel)
		}
	}
	if n := countMethod(sp.SnapshotCalls(), "Stop"); n != 1 || sp.IsRunning(info.SessionName) {
		t.Fatalf("calls = %v, running %v; want the runtime stopped once and not started", callMethods(sp.SnapshotCalls()), sp.IsRunning(info.SessionName))
	}
}

// A session-scoped stop whose waits read nothing showing the session busy
// (every read failed or was indeterminate: runtime.ErrWaitUnobserved, or a
// runtime that could not answer) is no ground for a restart: it has no turn
// identity to check, so only a busy reading can show the turn still running
// (SESSION-RUNTIME-026). The runtime is left running and the error wraps
// ErrInterruptOutcomeUnknown and runtime.ErrRuntimeUnavailable, not
// runtime.ErrTurnNotEnded.
func TestStopTurnWithRestartWithheldWhenTheWaitsReadNothing(t *testing.T) {
	for name, waitErr := range map[string]error{
		"the idle wait read nothing":     fmt.Errorf("agent not idle before timeout: %w", runtime.ErrWaitUnobserved),
		"the idle wait could not answer": fmt.Errorf("capturing the pane: %w", runtime.ErrRuntimeUnavailable),
	} {
		t.Run(name, func(t *testing.T) {
			mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
			sp.WaitForIdleErrors[info.SessionName] = waitErr
			report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
			if report.Restarted || !errors.Is(err, ErrInterruptOutcomeUnknown) || !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) {
				t.Fatalf("StopTurnWith(restart, waits read nothing) = %+v, %v; want ErrInterruptOutcomeUnknown and ErrRuntimeUnavailable, and no restart", report, err)
			}
			if n := countMethod(sp.SnapshotCalls(), "Stop") + countMethod(sp.SnapshotCalls(), "Start"); n != 0 || !sp.IsRunning(info.SessionName) {
				t.Fatalf("calls = %v, running %v; want the runtime left running", callMethods(sp.SnapshotCalls()), sp.IsRunning(info.SessionName))
			}
		})
	}
}

// A wait that saw the session busy and timed out (the bare timeout, no
// runtime.ErrWaitUnobserved) is the evidence a session-scoped restart needs.
func TestStopTurnWithRestartsWhenTheWaitSawABusySession(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.WaitForIdleErrors[info.SessionName] = errors.New("agent not idle before timeout")
	report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if err != nil || !report.Restarted {
		t.Fatalf("StopTurnWith(restart, busy at the timeout) = %+v, %v; want a restart, nil", report, err)
	}
}

// A soft-escape family's Ctrl-C fallback that fails to send with nothing
// before it that read the session busy says nothing about whether the
// interrupted turn still runs: the fallback wait read nothing or could not
// answer, or the send found another incarnation, of which the fallback's
// busy reading says nothing. A session-scoped stop with a restart on failure
// withholds the restart, leaving the runtime running, with
// ErrInterruptOutcomeUnknown and runtime.ErrRuntimeUnavailable, not
// runtime.ErrTurnNotEnded (SESSION-RUNTIME-026).
func TestStopTurnWithRestartWithheldWhenTheFollowUpInterruptFails(t *testing.T) {
	sendFailed := errors.New("send-keys C-c: exit status 1")
	for _, tc := range []struct {
		name, provider string
		fallback       error // the fallback wait's result
		send           error // the Ctrl-C's send
	}{
		{"codex fallback after a wait that read nothing", "codex", fmt.Errorf("agent not idle before timeout: %w", runtime.ErrWaitUnobserved), sendFailed},
		{"kimi fallback after a wait that read nothing", "kimi", fmt.Errorf("agent not idle before timeout: %w", runtime.ErrWaitUnobserved), sendFailed},
		{"codex fallback after a wait that could not answer", "codex", fmt.Errorf("capturing the pane: %w", runtime.ErrRuntimeUnavailable), sendFailed},
		{"codex fallback into another incarnation", "codex", errors.New("not idle yet"), fmt.Errorf("fenced send-keys C-c: %w", runtime.ErrFenceMismatch)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, sp, info, _ := newFencedStopSession(t, tc.provider, nil, runtime.TurnInterruptKeystroke)
			sp.WaitForIdleSequence[info.SessionName] = []error{tc.fallback, nil}
			sp.InterruptErrors[info.SessionName] = tc.send
			report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
			if report.Restarted || !errors.Is(err, ErrInterruptOutcomeUnknown) || !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) {
				t.Fatalf("StopTurnWith(restart, follow-up send fails) = %+v, %v; want ErrInterruptOutcomeUnknown and ErrRuntimeUnavailable, and no restart", report, err)
			}
			calls := sp.SnapshotCalls()
			if countMethod(calls, "Interrupt") != 1 {
				t.Fatalf("calls = %v, want the one follow-up interrupt tried", callMethods(calls))
			}
			if n := countMethod(calls, "Stop") + countMethod(calls, "Start"); n != 0 || !sp.IsRunning(info.SessionName) {
				t.Fatalf("calls = %v, running %v; want the runtime left running", callMethods(calls), sp.IsRunning(info.SessionName))
			}
		})
	}
}

// A follow-up interrupt whose send fails after the fallback wait saw the
// session busy leaves that busy reading as the stop's last: nothing more was
// sent, so the turn the Escape did not stop is seen still running, and a
// session-scoped stop with a restart on failure restarts the runtime
// (SESSION-RUNTIME-026).
func TestStopTurnWithRestartsWhenTheFallbackSawABusySession(t *testing.T) {
	for _, provider := range []string{"codex", "kimi"} {
		t.Run(provider, func(t *testing.T) {
			// newFencedStopSession's fallback wait reads busy: "not idle yet",
			// a bare error.
			mgr, sp, info, _ := newFencedStopSession(t, provider, nil, runtime.TurnInterruptKeystroke)
			sp.InterruptErrors[info.SessionName] = errors.New("send-keys C-c: exit status 1")
			report, err := mgr.StopTurnWith(context.Background(), info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
			if err != nil || !report.Restarted {
				t.Fatalf("StopTurnWith(restart, busy fallback, follow-up send fails) = %+v, %v; want a restart, nil", report, err)
			}
			methods := callMethods(sp.SnapshotCalls())
			interrupt, stop, start := slices.Index(methods, "Interrupt"), slices.Index(methods, "Stop"), slices.Index(methods, "Start")
			if interrupt < 0 || stop < interrupt || start < stop {
				t.Fatalf("calls = %v, want the failed follow-up Interrupt, then Stop, then Start", methods)
			}
		})
	}
}

// A stop whose idle wait read nothing showing the session busy (every pane
// capture indeterminate: runtime.ErrWaitUnobserved) leaves the turn to its
// family's boundary wait, which reads it apart from the pane (Codex's
// rollout). A boundary wait that passes is the turn's end: the stop
// succeeds and nothing restarts. One that timed out after reading the turn
// running (the bare timeout) is the turn not ended, and a session-scoped
// stop with a restart on failure restarts the runtime. One that read nothing
// either or could not answer, or a runtime with no boundary wait, leaves the
// idle wait's error: the restart is withheld, the runtime left running
// (SESSION-RUNTIME-026).
func TestStopTurnWithBoundaryDecidesAnUnobservedIdleWait(t *testing.T) {
	unobserved := fmt.Errorf("agent not idle before timeout: %w", runtime.ErrWaitUnobserved)
	for _, tc := range []struct {
		name      string
		boundary  error // the boundary wait's result; nil passes
		restarted bool  // the turn read running: the restart runs
		withheld  bool  // nothing read the turn: the restart is withheld
	}{
		{"the rollout recorded the turn's end", nil, false, false},
		{"the rollout recorded the turn running", errors.New("agent not idle before timeout"), true, false},
		{"the rollout read nothing", fmt.Errorf("agent not idle before timeout: %w", runtime.ErrWaitUnobserved), false, true},
		{"the rollout could not be read", fmt.Errorf("reading the rollout: %w", runtime.ErrRuntimeUnavailable), false, true},
		{"the runtime has no boundary wait", runtime.ErrInteractionUnsupported, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stop := func(opts func(*Manager, Info) StopTurnOptions) (*runtime.Fake, StopTurnReport, error) {
				t.Helper()
				mgr, sp, info, _ := newFencedStopSession(t, "codex", nil, runtime.TurnInterruptKeystroke)
				// Every capture indeterminate: the fallback wait and the idle
				// wait after the Ctrl-C both read nothing.
				sp.WaitForIdleSequence[info.SessionName] = []error{unobserved, unobserved}
				if tc.boundary != nil {
					sp.InterruptBoundaryErrors[info.SessionName] = tc.boundary
				}
				report, err := mgr.StopTurnWith(context.Background(), info.ID, opts(mgr, info))
				if n := countMethod(sp.SnapshotCalls(), "WaitForInterruptBoundary"); n != 1 {
					t.Fatalf("calls = %v, want the boundary wait once, after the idle wait", callMethods(sp.SnapshotCalls()))
				}
				return sp, report, err
			}

			sp, report, err := stop(func(mgr *Manager, info Info) StopTurnOptions {
				return StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)}
			})
			restarts := countMethod(sp.SnapshotCalls(), "Stop") + countMethod(sp.SnapshotCalls(), "Start")
			switch {
			case tc.restarted:
				if err != nil || !report.Restarted || restarts == 0 {
					t.Fatalf("StopTurnWith(restart) = %+v, %v, calls %v; want a restart, nil", report, err, callMethods(sp.SnapshotCalls()))
				}
			case tc.withheld:
				if report.Restarted || restarts != 0 || !errors.Is(err, ErrInterruptOutcomeUnknown) || !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) {
					t.Fatalf("StopTurnWith(restart) = %+v, %v, calls %v; want ErrInterruptOutcomeUnknown and ErrRuntimeUnavailable, and no restart", report, err, callMethods(sp.SnapshotCalls()))
				}
			default:
				if err != nil || report.Restarted || restarts != 0 {
					t.Fatalf("StopTurnWith(restart) = %+v, %v, calls %v; want nil and no restart: the turn ended", report, err, callMethods(sp.SnapshotCalls()))
				}
			}

			// Without a restart the stop returns what its waits decided.
			_, _, err = stop(func(*Manager, Info) StopTurnOptions { return StopTurnOptions{} })
			switch {
			case tc.restarted:
				if !errors.Is(err, runtime.ErrTurnNotEnded) || errors.Is(err, runtime.ErrWaitUnobserved) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("StopTurnWith = %v; want the boundary wait's ErrTurnNotEnded alone", err)
				}
			case tc.withheld:
				if !errors.Is(err, runtime.ErrTurnNotEnded) || !errors.Is(err, runtime.ErrWaitUnobserved) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
					t.Fatalf("StopTurnWith = %v; want the idle wait's ErrTurnNotEnded and ErrWaitUnobserved", err)
				}
			default:
				if err != nil {
					t.Fatalf("StopTurnWith = %v; want nil: the turn ended", err)
				}
			}
		})
	}
}

// A restart decided when the caller's deadline leaves less than the restart
// may take (stopTurnRestartBound) is withheld: it runs apart from ctx and
// would outlive the caller's lease, leaving a stopped runtime under a lease
// another host may take. The runtime is left running, and the error wraps
// ErrInterruptOutcomeUnknown and runtime.ErrRuntimeUnavailable.
func TestStopTurnWithRestartWithheldPastItsDeadline(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.WaitForIdleErrors[info.SessionName] = errors.New("agent not idle before timeout")
	ctx, cancel := context.WithTimeout(context.Background(), stopTurnRestartBound-time.Second)
	defer cancel()
	report, err := mgr.StopTurnWith(ctx, info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if report.Restarted || !errors.Is(err, ErrInterruptOutcomeUnknown) || !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) {
		t.Fatalf("StopTurnWith(restart, short deadline) = %+v, %v; want ErrInterruptOutcomeUnknown and ErrRuntimeUnavailable, and no restart", report, err)
	}
	if n := countMethod(sp.SnapshotCalls(), "Stop") + countMethod(sp.SnapshotCalls(), "Start"); n != 0 || !sp.IsRunning(info.SessionName) {
		t.Fatalf("calls = %v, running %v; want the runtime left running", callMethods(sp.SnapshotCalls()), sp.IsRunning(info.SessionName))
	}
}

// A deadline that covers the restart's bound lets the restart run.
func TestStopTurnWithRestartRunsWithinACoveringDeadline(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.WaitForIdleErrors[info.SessionName] = errors.New("agent not idle before timeout")
	ctx, cancel := context.WithTimeout(context.Background(), stopTurnRestartBound+time.Minute)
	defer cancel()
	report, err := mgr.StopTurnWith(ctx, info.ID, StopTurnOptions{Restart: stopTurnRestart(mgr, info, ActorController)})
	if err != nil || !report.Restarted {
		t.Fatalf("StopTurnWith(restart, covering deadline) = %+v, %v; want a restart, nil", report, err)
	}
}
