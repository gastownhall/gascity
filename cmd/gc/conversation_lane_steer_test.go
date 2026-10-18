package main

import (
	"context"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// laneProvider sets the fixture session's provider.
func laneProvider(provider string) laneFixtureOpt {
	return func(meta map[string]string) { meta["provider"] = provider }
}

// steerMsg is a steer message with fallback fb.
func steerMsg(fb opjournal.Fallback) func(*opjournal.Request) {
	return func(r *opjournal.Request) {
		r.Policy.Message.When = opjournal.WhenSteer
		r.Policy.Message.OnUnsupported = fb
	}
}

// busyLane makes e's runtime look mid-turn.
func busyLane(e *conversationLaneExec) {
	e.idle = func(nudgeTarget, worker.LiveObservation) bool { return false }
}

// recordingDeliverExec records every Delivery the lane hands its executor.
type recordingDeliverExec struct {
	*conversationLaneExec
	got *[]opjournal.Delivery
}

func (e recordingDeliverExec) Deliver(ctx context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	*e.got = append(*e.got, d)
	return e.conversationLaneExec.Deliver(ctx, d)
}

// TestLaneExecSteerCapIsRuntimeTimesFamily kills a probe whose Caps.Steer
// reads only the runtime or only the family: a keystroke runtime steers
// Codex and Claude, never Gemini; a runtime that cannot steer steers nobody;
// and a conversation another host serves is steered by none here.
func TestLaneExecSteerCapIsRuntimeTimesFamily(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provider string
		mode     runtime.SteerMode
		want     bool
	}{
		{"codex keystroke", "codex", runtime.SteerKeystroke, true},
		{"claude keystroke", "claude", runtime.SteerKeystroke, true},
		{"gemini keystroke", "gemini", runtime.SteerKeystroke, false},
		{"codex none", "codex", runtime.SteerNone, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t, laneProvider(tc.provider))
			f.sp.SteerModeValue = tc.mode
			st := f.laneStore(t)
			v, info := f.view(t)
			p, err := newConversationLaneExec(f.deps(st), info).Probe(context.Background(), v)
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}
			if p.Caps.Steer != tc.want {
				t.Fatalf("Caps.Steer = %v, want %v", p.Caps.Steer, tc.want)
			}
		})
	}
	t.Run("hosted elsewhere", func(t *testing.T) {
		f := newLaneFixture(t, laneProvider("codex"))
		st := f.laneStore(t)
		stampHost(t, f, st, 1, opjournal.KindMessage)
		v, info := f.view(t)
		p, err := newConversationLaneExec(f.deps(st), info).Probe(context.Background(), v)
		if err != nil || p.Caps.Live || p.Caps.Steer {
			t.Fatalf("Probe = %+v, %v; want neither live nor steer", p.Caps, err)
		}
	})
}

// TestLaneExecBusyCodexDefaultWaits is D-STEER: a default (boundary) message
// to a busy Codex session waits for the boundary, typing nothing, although
// its runtime could steer it.
func TestLaneExecBusyCodexDefaultWaits(t *testing.T) {
	f := newLaneFixture(t, laneProvider("codex"))
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "after this turn"))
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		busyLane(e)
		return e
	})
	if err != nil || res.Took != opjournal.TakeNone || !res.Busy || !laneWaitsFor(res, opjournal.WaitBusy) {
		t.Fatalf("Pass = %+v, %v; want a busy wait and nothing taken", res, err)
	}
	if calls := f.nudges(); len(calls) != 0 {
		t.Fatalf("runtime nudges = %+v, want none", calls)
	}
	if got := f.get(t, st, op.ID); got.Status != opjournal.StatusSubmitted {
		t.Fatalf("op status = %s, want submitted", got.Status)
	}
}

// TestLaneExecSteersBusyCodex proves an explicit steer to a busy Codex
// session types into the running turn: the lane hands the executor a steer in
// joins mode, the executor sends it with immediate delivery under the Take's
// fence (the runtime's NudgeNow), and the op closes succeeded via steer.
func TestLaneExecSteersBusyCodex(t *testing.T) {
	f := newLaneFixture(t, laneProvider("codex"))
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "also check the logs", steerMsg(opjournal.FallbackBoundary)))
	var mu sync.Mutex
	var reqs []worker.NudgeRequest
	var deliveries []opjournal.Delivery
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		busyLane(e)
		recordRequests(e, &mu, &reqs)
		return recordingDeliverExec{conversationLaneExec: e, got: &deliveries}
	})
	if err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v, %v; want accepted", res, err)
	}
	if len(deliveries) != 1 || !deliveries[0].Steer || deliveries[0].Mode != opjournal.LinkJoins {
		t.Fatalf("deliveries = %+v, want one steer in joins mode", deliveries)
	}
	wantFence := runtime.InputFence{Generation: 1, Epoch: 1, Token: f.token}
	if len(reqs) != 1 || reqs[0].Delivery != worker.NudgeDeliveryImmediate || reqs[0].Wake != worker.NudgeWakeLiveOnly || reqs[0].Fence != wantFence {
		t.Fatalf("requests = %+v, want one fenced live-only immediate nudge", reqs)
	}
	calls := f.nudges()
	if len(calls) != 1 || calls[0].Method != "NudgeNow" || calls[0].Message != "also check the logs" || calls[0].Fence != wantFence {
		t.Fatalf("runtime nudges = %+v, want one fenced NudgeNow of the text", calls)
	}
	o := f.get(t, st, op.ID).Outcome
	if o.Arm != opjournal.ArmSucceeded || o.Result.Via != laneViaSteer || o.Result.TurnLink != opjournal.LinkNone || o.Result.TurnBasis != opjournal.BasisNoSource {
		t.Fatalf("outcome = %+v, want succeeded via steer, link none/no_source", o)
	}
}

// TestLaneExecSteerAtBoundaryIsANormalTurn proves a steer to an idle session
// is delivered as a normal turn: starts mode, default delivery, via live.
func TestLaneExecSteerAtBoundaryIsANormalTurn(t *testing.T) {
	f := newLaneFixture(t, laneProvider("codex"))
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "go", steerMsg(opjournal.FallbackBoundary)))
	var mu sync.Mutex
	var reqs []worker.NudgeRequest
	var deliveries []opjournal.Delivery
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		recordRequests(e, &mu, &reqs)
		return recordingDeliverExec{conversationLaneExec: e, got: &deliveries}
	})
	if err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v, %v; want accepted", res, err)
	}
	if len(deliveries) != 1 || deliveries[0].Steer || deliveries[0].Mode != opjournal.LinkStarts {
		t.Fatalf("deliveries = %+v, want one non-steer delivery in starts mode", deliveries)
	}
	if len(reqs) != 1 || reqs[0].Delivery != worker.NudgeDeliveryDefault {
		t.Fatalf("requests = %+v, want one default nudge", reqs)
	}
	if o := f.get(t, st, op.ID).Outcome; o.Arm != opjournal.ArmSucceeded || o.Result.Via != laneViaLive {
		t.Fatalf("outcome = %+v, want succeeded via live", o)
	}
}

// TestLaneExecSteerUnsupportedFamily proves a steer to a busy session whose
// family takes no mid-turn input follows its fallback: boundary waits busy
// with nothing typed, fail closes failed{unsupported}.
func TestLaneExecSteerUnsupportedFamily(t *testing.T) {
	t.Run("boundary", func(t *testing.T) {
		f := newLaneFixture(t, laneProvider("gemini"))
		st := f.laneStore(t)
		op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi", steerMsg(opjournal.FallbackBoundary)))
		res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
			busyLane(e)
			return e
		})
		if err != nil || res.Took != opjournal.TakeNone || !res.Busy {
			t.Fatalf("Pass = %+v, %v; want a busy wait", res, err)
		}
		if len(f.nudges()) != 0 || f.get(t, st, op.ID).Status != opjournal.StatusSubmitted {
			t.Fatal("a steer the session cannot take was typed or closed")
		}
	})
	t.Run("fail", func(t *testing.T) {
		f := newLaneFixture(t, laneProvider("gemini"))
		st := f.laneStore(t)
		op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi", steerMsg(opjournal.FallbackFail)))
		res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
			busyLane(e)
			return e
		})
		if err != nil || res.Took != opjournal.TakeNone {
			t.Fatalf("Pass = %+v, %v; want nothing taken", res, err)
		}
		if o := f.get(t, st, op.ID).Outcome; o.Arm != opjournal.ArmFailed || o.Reason != opjournal.ReasonUnsupported {
			t.Fatalf("outcome = %+v, want failed{unsupported}", o)
		}
		if len(f.nudges()) != 0 {
			t.Fatal("an unsupported steer was typed")
		}
	})
}
