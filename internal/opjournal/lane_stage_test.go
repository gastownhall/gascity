package opjournal

import (
	"context"
	"errors"
	"slices"
	"testing"
)

// errStageStop ends a stage function's pass the way a process kill would:
// the test panics at the stage and recovers here.
type errStageStop struct{ stage LaneStage }

// passStoppingAt runs one pass whose stage function records each stage,
// checks the store there and, at stop, panics so nothing after it runs.
func passStoppingAt(t *testing.T, l *ConversationLane, exec Executor, stop LaneStage, at func(LaneStage)) (stages []LaneStage, stopped bool) {
	t.Helper()
	l.OnStage(func(s LaneStage) {
		stages = append(stages, s)
		if at != nil {
			at(s)
		}
		if s == stop {
			panic(errStageStop{stage: s})
		}
	})
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(errStageStop); !ok {
				panic(r)
			}
			stopped = true
		}
	}()
	if _, err := l.Pass(context.Background(), view(), exec); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	return stages, false
}

func TestLaneOnStageRunsAtTakeAndEffect(t *testing.T) {
	t.Run("content", func(t *testing.T) {
		h := newLaneHarness(t)
		op := h.propose(t, laneMsg("p", RolePrompt, "go"))
		exec := newFakeExec(h.clk)
		l := h.lane(t, "ctl")
		var atTake, atEffect Op
		var deliveriesAtTake, deliveriesAtEffect int
		stages, stopped := passStoppingAt(t, l, exec, "", func(s LaneStage) {
			switch s {
			case LaneStageTaken:
				atTake, deliveriesAtTake = h.get(t, op.ID), len(exec.delivered())
			case LaneStageEffected:
				atEffect, deliveriesAtEffect = h.get(t, op.ID), len(exec.delivered())
			}
		})
		if stopped || !slices.Equal(stages, []LaneStage{LaneStageTaken, LaneStageEffected}) {
			t.Fatalf("stages %v (stopped %v), want take then effect", stages, stopped)
		}
		if atTake.Status != StatusExecuting || deliveriesAtTake != 0 {
			t.Fatalf("at take: op %s, %d deliveries; want executing and none", atTake.Status, deliveriesAtTake)
		}
		if atEffect.Status != StatusExecuting || deliveriesAtEffect != 1 {
			t.Fatalf("at effect: op %s, %d deliveries; want executing and one", atEffect.Status, deliveriesAtEffect)
		}
		if got := h.get(t, op.ID); got.Status != StatusClosed || got.Outcome.Arm != ArmSucceeded {
			t.Fatalf("after the pass: %+v", got)
		}
	})
	t.Run("control", func(t *testing.T) {
		h := newLaneHarness(t)
		h.propose(t, Request{
			Conversation: "gc-1", Kind: KindInterrupt, Executor: ExecutorLane, ClientKey: "stop:1:t1",
			Producer: Producer{Principal: "operator", Actor: ActorOperator},
			Scope:    Scope{Kind: ScopeSession, Generation: 1, Turn: "t1"},
		})
		exec := newFakeExec(h.clk)
		stages, _ := passStoppingAt(t, h.lane(t, "ctl"), exec, "", nil)
		if !slices.Equal(stages, []LaneStage{LaneStageTaken, LaneStageEffected}) {
			t.Fatalf("control stages %v", stages)
		}
	})
	t.Run("a stop at take leaves the batch executing and unsent", func(t *testing.T) {
		h := newLaneHarness(t)
		op := h.propose(t, laneMsg("p", RolePrompt, "go"))
		exec := newFakeExec(h.clk)
		stages, stopped := passStoppingAt(t, h.lane(t, "ctl"), exec, LaneStageTaken, nil)
		if !stopped || !slices.Equal(stages, []LaneStage{LaneStageTaken}) {
			t.Fatalf("stages %v stopped %v", stages, stopped)
		}
		if got := h.get(t, op.ID); got.Status != StatusExecuting || len(exec.delivered()) != 0 {
			t.Fatalf("op %s with %d deliveries", got.Status, len(exec.delivered()))
		}
	})
	t.Run("nothing taken runs no stage", func(t *testing.T) {
		h := newLaneHarness(t)
		h.propose(t, laneMsg("p", RolePrompt, "go"))
		exec := newFakeExec(h.clk)
		exec.probe.Boundary = TriNo
		stages, _ := passStoppingAt(t, h.lane(t, "ctl"), exec, "", nil)
		if len(stages) != 0 {
			t.Fatalf("a pass that took nothing ran stages %v", stages)
		}
	})
	t.Run("nil clears", func(t *testing.T) {
		h := newLaneHarness(t)
		h.propose(t, laneMsg("p", RolePrompt, "go"))
		l := h.lane(t, "ctl")
		called := false
		l.OnStage(func(LaneStage) { called = true })
		l.OnStage(nil)
		if _, err := l.Pass(context.Background(), view(), newFakeExec(h.clk)); err != nil && !errors.Is(err, errCrash) {
			t.Fatalf("Pass: %v", err)
		}
		if called {
			t.Fatal("a cleared stage function ran")
		}
	})
}
