package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

// Kills a check that runs after the keys, or whose refusal still sends: for
// every family and both interrupt modes, a refused check sends nothing and
// returns its error, and a passed one sends exactly StopTurn's sequence.
func TestStopTurnIfRefusalSendsNothing(t *testing.T) {
	stale := &runtime.TurnNotCurrentError{Session: "s", TurnID: "t1"}
	for _, fam := range stopFamilies {
		for _, mode := range []runtime.TurnInterruptMode{runtime.TurnInterruptKeystroke, runtime.TurnInterruptProtocol} {
			t.Run(fam.name+"/"+string(mode), func(t *testing.T) {
				mgr, sp, info, _ := newFencedStopSession(t, fam.provider, fam.meta, mode)
				calls := 0
				err := mgr.StopTurnIf(context.Background(), info.ID, func() error { calls++; return stale })
				if !errors.Is(err, runtime.ErrTurnNotCurrent) {
					t.Fatalf("StopTurnIf(stale) = %v, want ErrTurnNotCurrent", err)
				}
				var nc *runtime.TurnNotCurrentError
				if !errors.As(err, &nc) || nc.TurnID != "t1" {
					t.Fatalf("StopTurnIf(stale) = %v, want the check's *TurnNotCurrentError", err)
				}
				if calls != 1 {
					t.Fatalf("check ran %d times, want 1", calls)
				}
				if sent := inputCalls(sp.SnapshotCalls(), info.SessionName); len(sent) != 0 {
					t.Fatalf("a refused check sent %v", callMethods(sent))
				}

				cmgr, csp, cinfo, _ := newFencedStopSession(t, fam.provider, fam.meta, mode)
				if err := cmgr.StopTurnIf(context.Background(), cinfo.ID, func() error { return nil }); err != nil {
					t.Fatalf("StopTurnIf(current): %v", err)
				}
				umgr, usp, uinfo, _ := newFencedStopSession(t, fam.provider, fam.meta, mode)
				if err := umgr.StopTurn(uinfo.ID); err != nil {
					t.Fatalf("StopTurn: %v", err)
				}
				checked := callMethods(inputCalls(csp.SnapshotCalls(), cinfo.SessionName))
				plain := callMethods(inputCalls(usp.SnapshotCalls(), uinfo.SessionName))
				if len(checked) == 0 || !slices.Equal(checked, plain) {
					t.Fatalf("StopTurnIf(current) sent %v, StopTurn %v; want one sequence", checked, plain)
				}
			})
		}
	}
}

// Kills a check made outside the session mutation lock: while it runs, no
// other holder can take the lock.
func TestStopTurnIfChecksUnderTheMutationLock(t *testing.T) {
	mgr, _, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	var lockErr error
	err := mgr.StopTurnIf(context.Background(), info.ID, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		lockErr = WithSessionMutationLockContext(ctx, info.ID, func() error { return nil })
		return nil
	})
	if err != nil {
		t.Fatalf("StopTurnIf: %v", err)
	}
	if !errors.Is(lockErr, ErrSessionMutationBusy) {
		t.Fatalf("taking the lock during the check = %v, want ErrSessionMutationBusy", lockErr)
	}
}

// A stop that finds no turn returns as StopTurn does, without the check:
// there is nothing to check a turn against.
func TestStopTurnIfWithoutATurnSkipsTheCheck(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "codex", nil, runtime.TurnInterruptKeystroke)
	if err := sp.Stop(info.SessionName); err != nil {
		t.Fatal(err)
	}
	sp.Calls = nil
	err := mgr.StopTurnIf(context.Background(), info.ID, func() error {
		t.Fatal("the check ran for a session with no runtime")
		return nil
	})
	if err != nil {
		t.Fatalf("StopTurnIf(not running) = %v, want nil as StopTurn", err)
	}
	if sent := inputCalls(sp.SnapshotCalls(), info.SessionName); len(sent) != 0 {
		t.Fatalf("a stop with no turn sent %v", callMethods(sent))
	}
}

// A wait after the interrupt that fails is sent but not seen to end.
func TestStopTurnWaitFailureIsTurnNotEnded(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptKeystroke)
	sp.WaitForIdleErrors[info.SessionName] = errors.New("still busy")
	err := mgr.StopTurnIf(context.Background(), info.ID, func() error { return nil })
	if !errors.Is(err, runtime.ErrTurnNotEnded) {
		t.Fatalf("StopTurnIf with a failed idle wait = %v, want ErrTurnNotEnded", err)
	}
	if lastInterrupt := countMethod(sp.Calls, "Interrupt"); lastInterrupt == 0 {
		t.Fatalf("calls = %v, want the interrupt sent", callMethods(sp.Calls))
	}
}

// newProtocolTurnSession is a protocol-mode session on a runtime with a turn
// fence, scripted by turns.
func newProtocolTurnSession(t *testing.T, turns ...string) (*Manager, *runtime.Fake, Info) {
	t.Helper()
	sp := runtime.NewFake()
	mgr, info, _ := newFencedStopSessionOn(t, runtime.FullFake{Fake: sp}, sp, "claude", nil, runtime.TurnInterruptProtocol)
	sp.TurnScripts = map[string][]string{info.SessionName: turns}
	return mgr, sp, info
}

func turnFenceCalls(sp *runtime.Fake) []string {
	var out []string
	for _, c := range sp.SnapshotCalls() {
		switch c.Method {
		case "InterruptTurn":
			out = append(out, c.Method+"("+c.Key+"):"+c.Value)
		case "WaitTurnEnded", "Interrupt", "SendKeys":
			out = append(out, c.Method)
		}
	}
	return out
}

func TestManagerInterruptTurn(t *testing.T) {
	t.Run("current turn settles", func(t *testing.T) {
		mgr, sp, info := newProtocolTurnSession(t, "t1", "")
		sp.FencedTurnInterrupts = true
		ack, err := mgr.InterruptTurn(context.Background(), info.ID, "t1")
		if err != nil || !ack.Fenced {
			t.Fatalf("InterruptTurn = %+v, %v; want a fenced, settled interrupt", ack, err)
		}
		if got, want := turnFenceCalls(sp), []string{"InterruptTurn(t1):sent", "WaitTurnEnded"}; !slices.Equal(got, want) {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	})
	t.Run("stale turn sends nothing", func(t *testing.T) {
		mgr, sp, info := newProtocolTurnSession(t, "t2")
		_, err := mgr.InterruptTurn(context.Background(), info.ID, "t1")
		var nc *runtime.TurnNotCurrentError
		if !errors.As(err, &nc) || nc.Current.TurnID != "t2" {
			t.Fatalf("InterruptTurn(stale) = %v, want *TurnNotCurrentError reading t2", err)
		}
		if got, want := turnFenceCalls(sp), []string{"InterruptTurn(t1):not_current"}; !slices.Equal(got, want) {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	})
	t.Run("not ended", func(t *testing.T) {
		mgr, _, info := newProtocolTurnSession(t, "t1")
		if _, err := mgr.InterruptTurn(context.Background(), info.ID, "t1"); !errors.Is(err, runtime.ErrTurnNotEnded) {
			t.Fatalf("InterruptTurn(never ends) = %v, want ErrTurnNotEnded", err)
		}
	})
	t.Run("dispatch failure is unknown", func(t *testing.T) {
		mgr, sp, info := newProtocolTurnSession(t, "t1")
		sp.InterruptTurnErrors = map[string]error{info.SessionName: runtime.ErrRuntimeUnavailable}
		_, err := mgr.InterruptTurn(context.Background(), info.ID, "t1")
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) {
			t.Fatalf("InterruptTurn(dispatch fails) = %v, want ErrRuntimeUnavailable only", err)
		}
	})
	t.Run("suspended row has no turn", func(t *testing.T) {
		mgr, sp, info := newProtocolTurnSession(t, "t1")
		if err := mgr.store.SetMetadata(info.ID, "state", string(StateSuspended)); err != nil {
			t.Fatal(err)
		}
		if _, err := mgr.InterruptTurn(context.Background(), info.ID, "t1"); !errors.Is(err, runtime.ErrTurnNotCurrent) {
			t.Fatalf("InterruptTurn(suspended) = %v, want ErrTurnNotCurrent", err)
		}
		if got := turnFenceCalls(sp); len(got) != 0 {
			t.Fatalf("a suspended row got %v", got)
		}
	})
	t.Run("runtime that cannot interrupt is refused", func(t *testing.T) {
		sp := runtime.NewFake()
		mgr, info, _ := newFencedStopSessionOn(t, runtime.FullFake{Fake: sp}, sp, "claude", nil, runtime.TurnInterruptUnsupported)
		sp.TurnScripts = map[string][]string{info.SessionName: {"t1"}}
		if _, err := mgr.InterruptTurn(context.Background(), info.ID, "t1"); !errors.Is(err, ErrTurnInterruptUnsupported) {
			t.Fatalf("InterruptTurn(unsupported) = %v, want ErrTurnInterruptUnsupported", err)
		}
		if got := turnFenceCalls(sp); len(got) != 0 {
			t.Fatalf("a runtime that cannot interrupt got %v", got)
		}
	})
	t.Run("keystroke-mode fence still interrupts by turn", func(t *testing.T) {
		// ACP interrupts by keystroke mode (its Interrupt is session/cancel)
		// and fences turns: the turn-scoped path takes its fence.
		sp := runtime.NewFake()
		mgr, info, _ := newFencedStopSessionOn(t, runtime.FullFake{Fake: sp}, sp, "claude", nil, runtime.TurnInterruptKeystroke)
		sp.TurnScripts = map[string][]string{info.SessionName: {"t1", ""}}
		if _, err := mgr.InterruptTurn(context.Background(), info.ID, "t1"); err != nil {
			t.Fatalf("InterruptTurn(keystroke mode) = %v, want nil", err)
		}
		if got, want := turnFenceCalls(sp), []string{"InterruptTurn(t1):sent", "WaitTurnEnded"}; !slices.Equal(got, want) {
			t.Fatalf("calls = %v, want %v", got, want)
		}
	})
	t.Run("runtime without a turn fence is unsupported", func(t *testing.T) {
		mgr, sp, info, _ := newFencedStopSession(t, "claude", nil, runtime.TurnInterruptProtocol)
		if _, err := mgr.InterruptTurn(context.Background(), info.ID, "t1"); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
			t.Fatalf("InterruptTurn(no fence) = %v, want ErrTurnIdentityUnsupported", err)
		}
		if sent := inputCalls(sp.SnapshotCalls(), info.SessionName); len(sent) != 0 {
			t.Fatalf("an unsupported interrupt sent %v", callMethods(sent))
		}
	})
}

// A checked stop waits for the session mutation lock only as long as ctx
// allows: a lock still held when ctx ends is unknown, nothing sent, and the
// check never runs.
func TestStopTurnIfLockWaitHonorsCtx(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "codex", nil, runtime.TurnInterruptKeystroke)
	held, release := make(chan struct{}), make(chan struct{})
	go func() {
		_ = WithSessionMutationLock(info.ID, func() error { close(held); <-release; return nil })
	}()
	<-held
	defer close(release)
	sp.Calls = nil
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := mgr.StopTurnIf(ctx, info.ID, func() error {
		t.Error("the check ran without the lock")
		return nil
	})
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || !errors.Is(err, ErrSessionMutationBusy) {
		t.Fatalf("StopTurnIf(lock held past ctx) = %v, want ErrRuntimeUnavailable and ErrSessionMutationBusy", err)
	}
	if sent := inputCalls(sp.SnapshotCalls(), info.SessionName); len(sent) != 0 {
		t.Fatalf("sent %v without the lock", callMethods(sent))
	}
}

// A runtime that stops between a passed check and the keys gets nothing:
// the checked stop says so (runtime.ErrSessionNotFound, or under a fence
// the miss), never ErrTurnNotEnded, which would claim the keys went out.
func TestStopTurnIfRuntimeGoneAfterTheCheck(t *testing.T) {
	for _, fenced := range []bool{false, true} {
		t.Run(map[bool]string{false: "unfenced", true: "fenced"}[fenced], func(t *testing.T) {
			mgr, sp, info, f := newFencedStopSession(t, "codex", nil, runtime.TurnInterruptKeystroke)
			ctx := context.Background()
			if fenced {
				ctx = runtime.WithInputFence(ctx, f)
			}
			sp.Calls = nil
			err := mgr.StopTurnIf(ctx, info.ID, func() error { return sp.Stop(info.SessionName) })
			if errors.Is(err, runtime.ErrTurnNotEnded) || err == nil {
				t.Fatalf("StopTurnIf(runtime gone after check) = %v, want a nothing-sent error", err)
			}
			if fenced && !errors.Is(err, runtime.ErrFenceMismatch) {
				t.Fatalf("StopTurnIf(fenced, runtime gone) = %v, want the miss", err)
			}
			if !fenced && !errors.Is(err, runtime.ErrSessionNotFound) {
				t.Fatalf("StopTurnIf(runtime gone) = %v, want ErrSessionNotFound", err)
			}
			if sent := inputCalls(sp.SnapshotCalls(), info.SessionName); len(sent) != 0 {
				t.Fatalf("sent %v to a runtime gone", callMethods(sent))
			}
		})
	}
}

// failingInterrupt is a runtime whose unfenced Interrupt fails after the
// check passed: unknown whether it reached the agent.
type failingInterrupt struct{ runtime.FullFake }

func (failingInterrupt) Interrupt(string) error { return errors.New("tmux: send-keys failed") }

func TestStopTurnIfSendFailureAfterTheCheckIsUnknown(t *testing.T) {
	sp := runtime.NewFake()
	mgr, info, _ := newFencedStopSessionOn(t, failingInterrupt{runtime.FullFake{Fake: sp}}, sp, "claude", nil, runtime.TurnInterruptKeystroke)
	err := mgr.StopTurnIf(context.Background(), info.ID, func() error { return nil })
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotEnded) {
		t.Fatalf("StopTurnIf(send fails) = %v, want ErrRuntimeUnavailable alone", err)
	}
	if countMethod(sp.SnapshotCalls(), "WaitForIdle") != 0 {
		t.Fatalf("waited after a failed send: %v", callMethods(sp.SnapshotCalls()))
	}
	// StopTurn, unchecked, keeps its error as it was.
	usp := runtime.NewFake()
	umgr, uinfo, _ := newFencedStopSessionOn(t, failingInterrupt{runtime.FullFake{Fake: usp}}, usp, "claude", nil, runtime.TurnInterruptKeystroke)
	if err := umgr.StopTurn(uinfo.ID); err == nil || errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("StopTurn(send fails) = %v, want its send error unwrapped", err)
	}
}

// A failed interrupt-boundary wait (Codex) is sent but not seen to end.
func TestStopTurnBoundaryWaitFailureIsTurnNotEnded(t *testing.T) {
	mgr, sp, info, _ := newFencedStopSession(t, "codex", nil, runtime.TurnInterruptKeystroke)
	sp.InterruptBoundaryErrors[info.SessionName] = errors.New("no boundary")
	err := mgr.StopTurnIf(context.Background(), info.ID, func() error { return nil })
	if !errors.Is(err, runtime.ErrTurnNotEnded) {
		t.Fatalf("StopTurnIf with a failed boundary wait = %v, want ErrTurnNotEnded", err)
	}
	if countMethod(sp.SnapshotCalls(), "WaitForInterruptBoundary") == 0 {
		t.Fatalf("calls = %v, want the boundary wait", callMethods(sp.SnapshotCalls()))
	}
}

// lockProbingTurns is a turn-fenced runtime whose InterruptTurn tries the
// session mutation lock, and whose WaitTurnEnded can fail with waitErr.
type lockProbingTurns struct {
	runtime.FullFake
	id      string
	lockErr *error
	waitErr error
}

func (p lockProbingTurns) InterruptTurn(ctx context.Context, name, turnID string) (runtime.TurnInterrupt, error) {
	if p.lockErr != nil {
		probe, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		*p.lockErr = WithSessionMutationLockContext(probe, p.id, func() error { return nil })
	}
	return p.FullFake.InterruptTurn(ctx, name, turnID)
}

func (p lockProbingTurns) WaitTurnEnded(ctx context.Context, name, turnID string, timeout time.Duration) (runtime.TurnState, error) {
	if p.waitErr != nil {
		return runtime.TurnState{}, p.waitErr
	}
	return p.FullFake.WaitTurnEnded(ctx, name, turnID, timeout)
}

func TestManagerInterruptTurnEdges(t *testing.T) {
	t.Run("runs under the session mutation lock", func(t *testing.T) {
		sp := runtime.NewFake()
		var lockErr error
		rt := &lockProbingTurns{FullFake: runtime.FullFake{Fake: sp}, lockErr: &lockErr}
		mgr, info, _ := newFencedStopSessionOn(t, rt, sp, "claude", nil, runtime.TurnInterruptProtocol)
		rt.id = info.ID
		sp.TurnScripts = map[string][]string{info.SessionName: {"t1", ""}}
		if _, err := mgr.InterruptTurn(context.Background(), info.ID, "t1"); err != nil {
			t.Fatalf("InterruptTurn = %v", err)
		}
		if !errors.Is(lockErr, ErrSessionMutationBusy) {
			t.Fatalf("taking the lock during InterruptTurn = %v, want ErrSessionMutationBusy", lockErr)
		}
	})
	t.Run("lock wait honors ctx", func(t *testing.T) {
		mgr, sp, info := newProtocolTurnSession(t, "t1", "")
		held, release := make(chan struct{}), make(chan struct{})
		go func() {
			_ = WithSessionMutationLock(info.ID, func() error { close(held); <-release; return nil })
		}()
		<-held
		defer close(release)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, err := mgr.InterruptTurn(ctx, info.ID, "t1"); !errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("InterruptTurn(lock held past ctx) = %v, want ErrRuntimeUnavailable", err)
		}
		if got := turnFenceCalls(sp); len(got) != 0 {
			t.Fatalf("calls without the lock: %v", got)
		}
	})
	t.Run("a backend without the fence is unsupported", func(t *testing.T) {
		mgr, sp, info := newProtocolTurnSession(t, "t1")
		sp.InterruptTurnErrors = map[string]error{info.SessionName: runtime.ErrInteractionUnsupported}
		if _, err := mgr.InterruptTurn(context.Background(), info.ID, "t1"); !errors.Is(err, runtime.ErrTurnIdentityUnsupported) {
			t.Fatalf("InterruptTurn(backend unsupported) = %v, want ErrTurnIdentityUnsupported", err)
		}
	})
	t.Run("a gone runtime is not current", func(t *testing.T) {
		mgr, sp, info := newProtocolTurnSession(t, "t1")
		sp.TurnReadErrors = map[string][]error{info.SessionName: {runtime.ErrSessionNotFound}}
		_, err := mgr.InterruptTurn(context.Background(), info.ID, "t1")
		var nc *runtime.TurnNotCurrentError
		if !errors.As(err, &nc) || errors.Is(err, runtime.ErrSessionNotFound) {
			t.Fatalf("InterruptTurn(runtime gone) = %v, want *TurnNotCurrentError", err)
		}
	})
	t.Run("gone after a failed attempt is unknown", func(t *testing.T) {
		mgr, sp, info := newProtocolTurnSession(t, "t1")
		sp.InterruptTurnErrors = map[string]error{info.SessionName: fmt.Errorf("%w: %w", runtime.ErrRuntimeUnavailable, runtime.ErrSessionNotFound)}
		_, err := mgr.InterruptTurn(context.Background(), info.ID, "t1")
		if !errors.Is(err, runtime.ErrRuntimeUnavailable) || errors.Is(err, runtime.ErrTurnNotCurrent) {
			t.Fatalf("InterruptTurn(gone after an attempt) = %v, want ErrRuntimeUnavailable", err)
		}
	})
	t.Run("a failed wait is not ended, and only that", func(t *testing.T) {
		sp := runtime.NewFake()
		rt := lockProbingTurns{FullFake: runtime.FullFake{Fake: sp}, waitErr: fmt.Errorf("bridge: %w", runtime.ErrRuntimeUnavailable)}
		mgr, info, _ := newFencedStopSessionOn(t, rt, sp, "claude", nil, runtime.TurnInterruptProtocol)
		sp.TurnScripts = map[string][]string{info.SessionName: {"t1"}}
		_, err := mgr.InterruptTurn(context.Background(), info.ID, "t1")
		if !errors.Is(err, runtime.ErrTurnNotEnded) || errors.Is(err, runtime.ErrRuntimeUnavailable) {
			t.Fatalf("InterruptTurn(wait fails) = %v, want ErrTurnNotEnded alone: it was sent", err)
		}
	})
}
