package opjournal

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// contentOnlyProbe is an idle probe from a host whose pass runs no control
// (Caps.ContentOnly: the CLI's inline pass).
func contentOnlyProbe() Probe {
	p := idleProbe()
	p.Caps.ContentOnly = true
	return p
}

// A content-only pass takes no control op: an interrupt and a respond each
// wait not_live for a lane host that runs control.
func TestPlanContentOnlyTakesNoControl(t *testing.T) {
	p := newPlanner(t)
	stop := p.op(interruptReq())
	answer := p.op(respondReq("perm-1#1", "gc", RespondPayload{Action: "approve"}))
	for _, ops := range [][]Op{{stop}, {answer}} {
		tp := takeFor(t, ops, contentOnlyProbe())
		if tp.take != nil {
			t.Fatalf("a content-only pass took %v", takenOf(tp))
		}
		if got := waitsOf(tp.waits)[ops[0].ID]; got != WaitNotLive {
			t.Fatalf("%s %s waits %q, want not_live", ops[0].Kind, ops[0].ID, got)
		}
		if len(tp.closes) != 0 {
			t.Fatalf("a content-only pass decided control from its probe: %+v", tp.closes)
		}
	}
}

// A content-only pass takes no deliver-now unit, nor closes it from its
// probe: the unit waits for the controller, and the content behind it waits
// blocked.
func TestPlanContentOnlyTakesNoDeliverNowUnit(t *testing.T) {
	now := t0.Add(time.Hour)
	for name, probe := range map[string]Probe{
		"idle": contentOnlyProbe(),
		"not running": func() Probe {
			p := contentOnlyProbe()
			p.Running = false
			return p
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			p := newPlanner(t)
			m, i := lowered(t, p.op(nowReq("a")), now)
			behind := p.op(laneMsg("b", RoleNotice, "later"))
			tp := takeFor(t, []Op{m, i, behind}, probe)
			if tp.take != nil {
				t.Fatalf("a content-only pass took %v", takenOf(tp))
			}
			if len(tp.closes) != 0 {
				t.Fatalf("a content-only pass closed %+v", tp.closes)
			}
			waits := waitsOf(tp.waits)
			if waits[m.ID] != WaitNotLive || waits[i.ID] != WaitNotLive || waits[behind.ID] != WaitBlocked {
				t.Fatalf("waits %v, want the unit not_live and the content behind it blocked", waits)
			}
		})
	}
}

// Content never overtakes control a content-only pass leaves to the
// controller (I10): a CLI nudge must not start a turn that an earlier
// session-scoped stop then kills.
func TestPlanContentOnlyContentWaitsBehindControl(t *testing.T) {
	p := newPlanner(t)
	stop := p.op(interruptReq())
	msg := p.op(laneMsg("a", RolePrompt, "x", withWhen(WhenBoundary)))
	tp := takeFor(t, []Op{stop, msg}, contentOnlyProbe())
	if tp.take != nil {
		t.Fatalf("a content-only pass took %v", takenOf(tp))
	}
	waits := waitsOf(tp.waits)
	if waits[stop.ID] != WaitNotLive || waits[msg.ID] != WaitBlocked {
		t.Fatalf("waits %v, want the stop not_live and the message blocked behind it", waits)
	}
}

// With no control open, a content-only pass takes content as any pass does.
func TestPlanContentOnlyTakesContentWithNoControl(t *testing.T) {
	p := newPlanner(t)
	msg := p.op(laneMsg("a", RolePrompt, "x", withWhen(WhenBoundary)))
	tp := takeFor(t, []Op{msg}, contentOnlyProbe())
	if tp.take == nil || tp.take.kind != TakeContent || takenOf(tp)[0] != msg.ID {
		t.Fatalf("take %+v, want the message as content", tp.take)
	}
}

// A Take of an interrupt with on_failure restart holds a lease long enough
// for the restart, which runs apart from the effect's context for up to two
// minutes once decided; any other Take keeps the probe's lease.
func TestTakeLeaseCoversAnInterruptRestart(t *testing.T) {
	for name, tc := range map[string]struct {
		onFailure OnFailure
		want      time.Duration
	}{
		"restart": {OnFailureRestart, RestartLeaseTTL},
		"fail":    {OnFailureFail, DefaultLeaseTTL},
	} {
		t.Run(name, func(t *testing.T) {
			h := newLaneHarness(t)
			req := interruptReq()
			req.Policy.Interrupt.OnFailure = tc.onFailure
			op := h.propose(t, req)
			exec := newFakeExec(h.clk)
			var lease time.Duration
			var budget time.Duration
			exec.interrupt = func(InterruptCall) ControlResult {
				lease = h.get(t, op.ID).Lease.Deadline.Sub(h.clk.Now())
				return Settled{Via: "fake"}
			}
			exec.onEffect = func() {}
			ctxExec := &deadlineExec{fakeExec: exec, budget: &budget}
			if _, err := h.lane(t, "ctl").Pass(context.Background(), view(), ctxExec); err != nil {
				t.Fatalf("Pass: %v", err)
			}
			if lease != tc.want {
				t.Fatalf("the Take's lease ran %s, want %s", lease, tc.want)
			}
			if tc.onFailure == OnFailureRestart && budget < RestartLeaseTTL-ExecutorMargin-time.Second {
				t.Fatalf("the effect's budget is %s, want about %s", budget, RestartLeaseTTL-ExecutorMargin)
			}
		})
	}
	if RestartLeaseTTL < DefaultLeaseTTL+2*time.Minute {
		t.Fatalf("RestartLeaseTTL %s does not cover a two-minute restart past the default lease", RestartLeaseTTL)
	}
}

// deadlineExec records the effect context's remaining budget.
type deadlineExec struct {
	*fakeExec
	budget *time.Duration
}

func (d *deadlineExec) Interrupt(ctx context.Context, c InterruptCall) ControlResult {
	if dl, ok := ctx.Deadline(); ok {
		*d.budget = time.Until(dl)
	}
	return d.fakeExec.Interrupt(ctx, c)
}

// A restart that stopped the runtime and could not start it again closes
// the op failed{restart_not_started} at once, its cause kept as the op's
// last error, and credits its turn: the turn ended with the runtime.
func TestInterruptCreditsRestartNotStarted(t *testing.T) {
	h := newLaneHarness(t)
	req := interruptReq()
	req.Policy.Interrupt.OnFailure = OnFailureRestart
	op := h.propose(t, req)
	exec := newFakeExec(h.clk)
	exec.interrupt = func(InterruptCall) ControlResult {
		return Rejected{Arm: ArmFailed, Reason: ReasonRestartNotStarted, Turn: "t1", Cause: errors.New("cannot start")}
	}
	if _, err := h.lane(t, "ctl").Pass(context.Background(), view(), exec); err != nil {
		t.Fatalf("Pass: %v", err)
	}
	got := h.get(t, op.ID)
	if got.Status != StatusClosed || got.Outcome.Arm != ArmFailed || got.Outcome.Reason != ReasonRestartNotStarted || got.Outcome.Result.Turn != "t1" {
		t.Fatalf("op %s %+v, want closed failed{restart_not_started} naming t1", got.Status, got.Outcome)
	}
	if !strings.Contains(got.LastError, "cannot start") {
		t.Fatalf("last error %q, want the start's cause kept", got.LastError)
	}
	if arm, reason := got.Outcome.Effective(); arm != ArmFailed || reason != ReasonRestartNotStarted {
		t.Fatalf("effective outcome %s{%s}, want a known failed reason", arm, reason)
	}
	refs, err := h.store.InterruptCredits("gc-1", 1)
	if err != nil {
		t.Fatalf("InterruptCredits: %v", err)
	}
	if len(refs) != 1 || refs[0].Op != op.ID || refs[0].Turn != "t1" {
		t.Fatalf("credits %+v, want the op's turn t1", refs)
	}
}
