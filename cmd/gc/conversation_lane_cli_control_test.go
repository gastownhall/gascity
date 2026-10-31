package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
)

// controlCalls are the fake runtime's calls that interrupt or answer the
// session.
func (c *cliLaneCity) controlCalls() []runtime.Call {
	var out []runtime.Call
	for _, call := range c.sp.Calls {
		switch call.Method {
		case "Interrupt", "SendKeys", "Respond", "Stop", "Start":
			out = append(out, call)
		}
	}
	return out
}

// gc session nudge's inline pass runs no control (opjournal.Caps.ContentOnly):
// a pending interrupt or respond waits for the controller's lane host, and
// the nudge waits behind it, so the CLI never starts a turn an earlier stop
// then kills, nor runs an interrupt past its 30 s lease.
func TestSessionNudgeLaneLeavesControlToTheController(t *testing.T) {
	for _, kind := range []opjournal.Kind{opjournal.KindInterrupt, opjournal.KindRespond} {
		t.Run(string(kind), func(t *testing.T) {
			c := newCLILaneCity(t)
			st := c.laneStore(t)
			var control opjournal.Op
			if kind == opjournal.KindInterrupt {
				control = c.propose(t, st, opjournal.Request{
					Conversation: c.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: "stop-1",
					Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
					Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1},
				})
			} else {
				c.prompt(laneTestRequest)
				control, _ = c.respond(t, st, opjournal.RespondPayload{Action: "approve"})
			}
			c.sp.Calls = nil
			code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-behind-1", false, cliLaneTestMessage)
			if code != 0 {
				t.Fatalf("nudge = %d; stderr=%s", code, stderr)
			}
			if calls := c.controlCalls(); len(calls) != 0 {
				t.Fatalf("the CLI's pass ran control: %+v", calls)
			}
			if got := c.nudges(); len(got) != 0 {
				t.Fatalf("the nudge overtook pending control: %+v", got)
			}
			if got := c.get(t, st, control.ID); got.Status == opjournal.StatusClosed || got.Attempt != 0 {
				t.Fatalf("control op %s attempt %d, want it open and untaken", got.Status, got.Attempt)
			}
			if !strings.Contains(stdout, "earlier input to the session goes first") {
				t.Fatalf("stdout %q, want the nudge queued behind earlier input", stdout)
			}
		})
	}
}

// A deliver-now unit (the ops route's message with when now) is control too:
// the CLI's pass lowers it by rule but neither interrupts nor delivers it,
// and the nudge waits behind it.
func TestSessionNudgeLaneLeavesADeliverNowUnit(t *testing.T) {
	c := newCLILaneCity(t)
	st := c.laneStore(t)
	m := c.propose(t, st, c.nowMsg())
	c.sp.Calls = nil
	code, stdout, stderr := c.nudge(nudgeDeliveryWaitIdle, "k-behind-2", false, cliLaneTestMessage)
	if code != 0 {
		t.Fatalf("nudge = %d; stderr=%s", code, stderr)
	}
	if calls := c.controlCalls(); len(calls) != 0 {
		t.Fatalf("the CLI's pass ran the unit's interrupt: %+v", calls)
	}
	if got := c.nudges(); len(got) != 0 {
		t.Fatalf("the CLI's pass delivered %+v", got)
	}
	if got := c.get(t, st, m.ID); got.Status == opjournal.StatusClosed || got.Attempt != 0 {
		t.Fatalf("deliver-now message %s attempt %d, want it open and untaken", got.Status, got.Attempt)
	}
	if got := c.get(t, st, laneNowID(t, st, m)); got.Status == opjournal.StatusClosed || got.Attempt != 0 {
		t.Fatalf("derived interrupt %s attempt %d, want it open and untaken", got.Status, got.Attempt)
	}
	if !strings.Contains(stdout, "earlier input to the session goes first") {
		t.Fatalf("stdout %q, want the nudge queued behind the unit", stdout)
	}
}
