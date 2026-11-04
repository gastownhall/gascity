package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// The visible shape and current-turn identity are from the retained42c GC API
// receipts225/227. This is a source regression fixture, not native acceptance.
const (
	laneNativeStreamingScreen = "GCGEN|619fa169543f|0054\nGCGEN|619fa169543f|0055\nGCGEN|619fa169543f|0056\n\n› Ask Codex to do anything\nGPT-6.1-Sol medium"
	laneNativeHoldTurn        = "01a1a3fa-fd71-7351-8ffc-cf796b268edd"
)

func nativeBoundaryExecutor(t *testing.T) (*turnLaneFixture, *opjournal.Store, *conversationLaneExec, opjournal.ConversationView) {
	t.Helper()
	f := newTurnLaneFixture(t, runtime.ProfileTmux)
	f.transcript = true
	f.turns.history = func(int, int) (runtime.TurnHistory, error) { return running(laneNativeHoldTurn), nil }
	st := f.laneStore(t)
	e, v := f.exec(t, st)
	// The captured live composer has no busy status row. A negative screen
	// reading cannot outweigh its source's recorded current native turn.
	if strings.Contains(laneNativeStreamingScreen, "esc to interrupt") {
		t.Fatal("capture unexpectedly contains a busy status row")
	}
	e.idle = func(nudgeTarget, worker.LiveObservation) bool { return true }
	e.turnOnScreen = func(nudgeTarget) (bool, bool, error) { return false, true, nil }
	return f, st, e, v
}

func TestLaneNativeBoundaryCapturedCurrentTurnVetoesIdleScreen(t *testing.T) {
	_, _, e, v := nativeBoundaryExecutor(t)
	p, err := e.Probe(context.Background(), v)
	if err != nil || p.Boundary != opjournal.TriNo || p.TurnLive != opjournal.TriYes || p.Turn != laneNativeHoldTurn {
		t.Fatalf("captured live turn with spinner-free composer: probe=%+v err=%v; want busy with exact native turn", p, err)
	}
}

func TestLaneNativeBoundaryReprobeRetainsCurrentTurnVeto(t *testing.T) {
	_, _, e, v := nativeBoundaryExecutor(t)
	if _, err := e.Probe(context.Background(), v); err != nil {
		t.Fatal(err)
	}
	p, err := e.Reprobe(context.Background(), v)
	if err != nil || p.Boundary != opjournal.TriNo || p.TurnLive != opjournal.TriYes || p.Turn != laneNativeHoldTurn {
		t.Fatalf("native current turn dropped by reprobe: probe=%+v err=%v", p, err)
	}
}

func TestLaneNativeBoundaryUnreadableSupportedSourceNeverMeansIdle(t *testing.T) {
	for _, mode := range []string{"turn read", "capability read", "source disappeared"} {
		t.Run(mode, func(t *testing.T) {
			f, _, e, v := nativeBoundaryExecutor(t)
			switch mode {
			case "turn read":
				f.turns.history = func(int, int) (runtime.TurnHistory, error) {
					return runtime.TurnHistory{}, runtime.ErrRuntimeUnavailable
				}
			case "capability read":
				e.turns = func() (worker.TurnHandle, error) { return laneStubTurns{capsErr: runtime.ErrRuntimeUnavailable}, nil }
			case "source disappeared":
				e.turns = func() (worker.TurnHandle, error) { return nil, runtime.ErrSessionNotFound }
			}
			for _, read := range []func(context.Context, opjournal.ConversationView) (opjournal.Probe, error){e.Probe, e.Reprobe} {
				p, err := read(context.Background(), v)
				if err != nil || p.Boundary != opjournal.TriUnknown || !laneProbeStillBusy(p) {
					t.Fatalf("unreadable %s: probe=%+v err=%v; want unknown boundary", mode, p, err)
				}
			}
			if len(f.nudges()) != 0 {
				t.Fatal("unreadable native source caused input")
			}
		})
	}
}

func TestLaneNativeBoundaryPassWaitsForRecordedEndAndDeliversOnce(t *testing.T) {
	f, st, e, v := nativeBoundaryExecutor(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("native-boundary", opjournal.RolePrompt, "QUEUE1"))
	res, err := l.Pass(context.Background(), v, e)
	if err != nil || res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitBusy) || len(f.nudges()) != 0 {
		t.Fatalf("busy native pass=%+v err=%v nudges=%+v; want waiting and no native input", res, err, f.nudges())
	}
	f.turns.history = func(int, int) (runtime.TurnHistory, error) { return idleHistory, nil }
	e, v = f.exec(t, st)
	e.idle = func(nudgeTarget, worker.LiveObservation) bool { return true }
	e.turnOnScreen = func(nudgeTarget) (bool, bool, error) { return false, true, nil }
	res, err = l.Pass(context.Background(), v, e)
	if err != nil || res.Done != opjournal.DoneAccepted || len(f.nudges()) != 1 {
		t.Fatalf("recorded end pass=%+v err=%v nudges=%+v", res, err, f.nudges())
	}
	got := f.get(t, st, op.ID)
	if got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded || f.nudges()[0].Message != "QUEUE1" {
		t.Fatalf("final op=%+v nudges=%+v", got, f.nudges())
	}
}

func TestLaneNativeBoundaryHandoffRechecksNewTurnWithoutConsumingOperation(t *testing.T) {
	f, st, e, v := nativeBoundaryExecutor(t)
	f.turns.history = func(int, int) (runtime.TurnHistory, error) { return idleHistory, nil }
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("handoff-race", opjournal.RolePrompt, "frozen QUEUE1"))
	exec := beforeDeliverExec{conversationLaneExec: e, before: func() {
		f.turns.history = func(int, int) (runtime.TurnHistory, error) { return running(laneNativeHoldTurn), nil }
	}}
	res, err := l.Pass(context.Background(), v, exec)
	if err != nil || len(f.nudges()) != 0 {
		t.Fatalf("new-turn handoff pass=%+v err=%v nudges=%+v", res, err, f.nudges())
	}
	held := f.get(t, st, op.ID)
	if held.Status == opjournal.StatusClosed || held.ClientKey != op.ClientKey || held.Payload != op.Payload || held.MaybeDelivered != 0 {
		t.Fatalf("deferred op changed/closed=%+v", held)
	}
	f.turns.history = func(int, int) (runtime.TurnHistory, error) { return idleHistory, nil }
	f.clk.now = f.clk.now.Add(opjournal.DefaultLeaseTTL)
	e, v = f.exec(t, st)
	e.idle = func(nudgeTarget, worker.LiveObservation) bool { return true }
	e.turnOnScreen = func(nudgeTarget) (bool, bool, error) { return false, true, nil }
	res, err = l.Pass(context.Background(), v, e)
	if err != nil || res.Done != opjournal.DoneAccepted || len(f.nudges()) != 1 {
		t.Fatalf("retry after end=%+v err=%v nudges=%+v", res, err, f.nudges())
	}
	got := f.get(t, st, op.ID)
	if got.ClientKey != op.ClientKey || got.Payload != op.Payload || got.Outcome.Arm != opjournal.ArmSucceeded || f.nudges()[0].Fence != (runtime.InputFence{Generation: 1, Epoch: 1, Token: f.token}) {
		t.Fatalf("frozen retry changed=%+v nudges=%+v", got, f.nudges())
	}
}

func TestLaneNativeBoundaryHandoffUnreadableSendsNothing(t *testing.T) {
	f, st, e, v := nativeBoundaryExecutor(t)
	f.turns.history = func(int, int) (runtime.TurnHistory, error) { return idleHistory, nil }
	op := f.propose(t, st, f.msg("handoff-unknown", opjournal.RolePrompt, "QUEUE1"))
	exec := beforeDeliverExec{conversationLaneExec: e, before: func() {
		f.turns.history = func(int, int) (runtime.TurnHistory, error) {
			return runtime.TurnHistory{}, errors.New("native source unreadable")
		}
	}}
	res, err := f.lane(t, st).Pass(context.Background(), v, exec)
	if err != nil || len(f.nudges()) != 0 || f.get(t, st, op.ID).Status == opjournal.StatusClosed {
		t.Fatalf("unknown handoff pass=%+v err=%v op=%+v nudges=%+v", res, err, f.get(t, st, op.ID), f.nudges())
	}
}

func TestLaneNativeBoundaryCanceledAndUnsupportedSteerNeverDispatch(t *testing.T) {
	f, st, e, v := nativeBoundaryExecutor(t)
	canceled := f.propose(t, st, f.msg("canceled-boundary", opjournal.RolePrompt, "CANCEL"))
	if _, _, err := st.Cancel(canceled.ID, "operator", "owned cancellation"); err != nil {
		t.Fatal(err)
	}
	steer := f.propose(t, st, f.msg("unsupported-steer", opjournal.RolePrompt, "STEER", func(r *opjournal.Request) {
		r.Policy.Message.When = opjournal.WhenSteer
		r.Policy.Message.OnUnsupported = opjournal.FallbackFail
	}))
	res, err := f.lane(t, st).Pass(context.Background(), v, e)
	if err != nil || len(f.nudges()) != 0 {
		t.Fatalf("unsupported/canceled pass=%+v err=%v nudges=%+v", res, err, f.nudges())
	}
	if got := f.get(t, st, canceled.ID); got.Outcome.Arm != opjournal.ArmCanceled {
		t.Fatalf("canceled op changed=%+v", got)
	}
	if got := f.get(t, st, steer.ID); got.Outcome.Arm != opjournal.ArmFailed || got.Outcome.Reason != opjournal.ReasonUnsupported {
		t.Fatalf("unsupported steer=%+v", got)
	}
}

func TestLaneNativeBoundaryExplicitUnsupportedSourceKeepsScreenFallback(t *testing.T) {
	f, st, e, v := nativeBoundaryExecutor(t)
	capReads := 0
	e.turns = func() (worker.TurnHandle, error) {
		capReads++
		return laneStubTurns{caps: worker.TurnCapabilities{}, read: func(context.Context) (runtime.TurnHistory, error) {
			t.Fatal("explicitly unsupported source was read")
			return runtime.TurnHistory{}, runtime.ErrTurnIdentityUnsupported
		}}, nil
	}
	p, err := e.Probe(context.Background(), v)
	if err != nil || p.Boundary != opjournal.TriYes {
		t.Fatalf("unsupported boundary=%+v err=%v; want existing screen fallback", p, err)
	}
	if _, err := e.Reprobe(context.Background(), v); err != nil || capReads != 1 {
		t.Fatalf("explicit unsupported capabilities were not cached: reads=%d err=%v", capReads, err)
	}
	op := f.propose(t, st, f.msg("unsupported-source", opjournal.RolePrompt, "QUEUE1"))
	res, err := f.lane(t, st).Pass(context.Background(), v, e)
	if err != nil || res.Done != opjournal.DoneAccepted || len(f.nudges()) != 1 || f.get(t, st, op.ID).Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("unsupported fallback pass=%+v err=%v nudges=%+v", res, err, f.nudges())
	}
}

func TestLaneNativeBoundaryTransientCapabilitiesRecoverOnReprobe(t *testing.T) {
	for _, missing := range []bool{false, true} {
		for _, active := range []bool{false, true} {
			t.Run(fmt.Sprintf("missing=%v/active=%v", missing, active), func(t *testing.T) {
				f, _, e, v := nativeBoundaryExecutor(t)
				reads := 0
				e.turns = func() (worker.TurnHandle, error) {
					reads++
					if reads == 1 {
						if missing {
							return nil, runtime.ErrSessionNotFound
						}
						return laneStubTurns{capsErr: runtime.ErrRuntimeUnavailable}, nil
					}
					return laneStubTurns{caps: laneTranscriptCaps, read: func(context.Context) (runtime.TurnHistory, error) {
						if active {
							return running(laneNativeHoldTurn), nil
						}
						return idleHistory, nil
					}}, nil
				}
				p, err := e.Probe(context.Background(), v)
				if err != nil || p.Boundary != opjournal.TriUnknown || reads != 1 {
					t.Fatalf("first unavailable probe=%+v err=%v reads=%d", p, err, reads)
				}
				p, err = e.Reprobe(context.Background(), v)
				want := opjournal.TriYes
				if active {
					want = opjournal.TriNo
				}
				if err != nil || p.Boundary != want || reads != 2 {
					t.Fatalf("recovered reprobe=%+v err=%v reads=%d wantboundary=%v", p, err, reads, want)
				}
				p, err = e.Reprobe(context.Background(), v)
				if err != nil || p.Boundary != want || reads != 2 {
					t.Fatalf("successful capability was not cached: probe=%+v err=%v reads=%d", p, err, reads)
				}
				if len(f.nudges()) != 0 {
					t.Fatal("observation recovery sent native input")
				}
			})
		}
	}
}
