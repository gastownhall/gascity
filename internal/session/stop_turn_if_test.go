package session

import (
	"context"
	"errors"
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
