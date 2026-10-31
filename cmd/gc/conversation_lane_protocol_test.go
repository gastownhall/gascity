package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
)

// Protocol-mode sessions on the conversation lane (OJ-21 slice 21p): the
// producers' rule for whether a lane serves a session (laneServesLive) is
// the executor's Caps.Live rule, so a t3bridge session's notices are lane
// ops, and a protocol-mode runtime whose turn starts are not recorded keeps
// its producers' legacy path.

// TestLaneServesLiveMatchesExecutorCaps pins the producers' predicate to the
// executor's caps over each runtime profile: whatever the executor would
// call live, the producers propose to the lane, and nothing else.
func TestLaneServesLiveMatchesExecutorCaps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile runtime.Profile
		// plain uses the unprofiled fake; protocol sets its interrupt mode.
		plain    bool
		protocol bool
		want     bool
	}{
		{name: "fake keystroke", plain: true, want: true},
		{name: "fake protocol, starts unrecorded", plain: true, protocol: true, want: false},
		{name: "tmux", profile: runtime.ProfileTmux, want: true},
		{name: "acp", profile: runtime.ProfileACP, want: true},
		{name: "t3bridge", profile: runtime.ProfileT3Bridge, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				sp   runtime.Provider
				name string
				live bool
			)
			if tc.plain {
				f := newLaneFixture(t)
				if tc.protocol {
					f.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
				}
				st := f.laneStore(t)
				v, row := f.view(t)
				p, err := newConversationLaneExec(f.deps(st), row).Probe(context.Background(), v)
				if err != nil {
					t.Fatalf("Probe: %v", err)
				}
				sp, name, live = f.sp, f.name, p.Caps.Live
			} else {
				f := newTurnLaneFixture(t, tc.profile)
				e, v := f.exec(t, f.laneStore(t))
				p, err := e.Probe(context.Background(), v)
				if err != nil {
					t.Fatalf("Probe: %v", err)
				}
				sp, name, live = f.sp, f.name, p.Caps.Live
			}
			got := laneServesLive(sp, name)
			if got != live || got != tc.want {
				t.Fatalf("laneServesLive = %v, executor Caps.Live = %v; want both %v", got, live, tc.want)
			}
		})
	}
}

// A t3bridge session's mail notice is a lane notice, not a direct nudge or a
// queue item.
func TestMailNotifyUnderLaneProposesForAProtocolSession(t *testing.T) {
	c := newCLILaneCity(t)
	sp := c.asT3Bridge()
	id, err := sendMailNotifyWithOp(c.target, c.store, sp, "alice", "msg-1")
	if err != nil {
		t.Fatalf("notify: %v", err)
	}
	op := c.noticeBy(t, mailNoticePrincipal, "msg-1")
	if id != op.ID || op.Status == opjournal.StatusClosed {
		t.Fatalf("op %s = %+v, want an open notice caused by msg-1", id, op)
	}
	assertLaneNotice(t, op, mailNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("mail", "You have mail from alice"))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}

// A t3bridge session's sling notice is a lane notice.
func TestSlingNudgeUnderLaneProposesForAProtocolSession(t *testing.T) {
	c := newCLILaneCity(t)
	sp := c.asT3Bridge()
	var stdout, stderr bytes.Buffer
	deliverSlingNudge(c.target, sp, c.store, c.store, c.dir, "gc-bead-1", &stdout, &stderr)
	if !strings.Contains(stdout.String(), "in the conversation lane") || stderr.Len() != 0 {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
	op := c.noticeBy(t, slingNoticePrincipal, "gc-bead-1")
	assertLaneNotice(t, op, slingNoticePrincipal, opjournal.WakeIfNeeded, reminderGolden("sling", slingNudgeText))
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}

// A t3bridge session's hook-claim continuation is a lane notice.
func TestHookClaimContinuationUnderLaneProposesForAProtocolSession(t *testing.T) {
	c := newCLILaneCity(t)
	sp := c.asT3Bridge()
	if handled, err := continuationLaneNotice(c.target, c.store, sp, "gc-root-1"); !handled || err != nil {
		t.Fatalf("continuationLaneNotice = %v, %v; want proposed", handled, err)
	}
	c.noticeBy(t, hookClaimContinuationPrincipal, "gc-root-1")
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}

// gc session nudge to a t3bridge session is a lane op.
func TestSessionNudgeUnderLaneProposesForAProtocolSession(t *testing.T) {
	c := newCLILaneCity(t)
	sp := c.asT3Bridge()
	var stdout, stderr bytes.Buffer
	handled, code := deliverSessionNudgeLane(c.target, c.store, sp, cliLaneTestMessage, nudgeDeliveryQueue, "", false, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("deliverSessionNudgeLane = %v, %d; stderr=%s", handled, code, stderr.String())
	}
	op := c.only(t)
	if op.Executor != opjournal.ExecutorLane || op.Status == opjournal.StatusClosed {
		t.Fatalf("op %+v, want an open lane op", op)
	}
	if n := len(c.nudges()); n != 0 || c.queuedItems(t) != 0 {
		t.Fatalf("legacy delivery ran: %d nudges, %d queued", n, c.queuedItems(t))
	}
}

// A planner reminder for a t3bridge session goes to the lane.
func TestPlannerNoticeProtocolSessionGoesToTheLane(t *testing.T) {
	f := newLaneFixture(t)
	f.asT3Bridge()
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}
	observed := f.clk.Now().Add(-time.Hour)
	var out bytes.Buffer
	runNoticeBackstop(f, backstopRows(t, f, 0, observed), pred, &out)
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("the backstop typed %d nudges itself, want 0", n)
	}
	op := f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(observed), 1))
	if op.Payload != "claim wb-1" || op.Policy.Message.Role != opjournal.RoleNotice || op.Executor != opjournal.ExecutorLane {
		t.Fatalf("op = %+v, want the claim notice on the lane", op)
	}
}

// A ready wait for a t3bridge session proposes its lane notice.
func TestWaitNoticeProtocolSessionGoesToTheLane(t *testing.T) {
	f := newLaneFixture(t)
	f.asT3Bridge()
	f.cfg.Ops.Authoritative = config.OpsKinds("message")
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	wait := readyWait(t, f, "Continue after review closes.")
	dispatchWaits(t, f)
	op := waitNotice(t, f, st, wait)
	if op.Executor != opjournal.ExecutorLane || op.Policy.Message.Role != opjournal.RoleNotice {
		t.Fatalf("op = %+v, want the wait's lane notice", op)
	}
	if got := waitNow(t, f, wait).NudgeID; got != op.ID {
		t.Fatalf("wait nudge_id = %q, want the notice's %q", got, op.ID)
	}
}

// The lane types a t3bridge session the same reminder block the legacy
// dispatcher did, once, under its delivery key.
func TestProtocolSessionNoticeDeliversTheReminderText(t *testing.T) {
	f := newTurnLaneFixture(t, runtime.ProfileT3Bridge)
	st := f.laneStore(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("k-notice", opjournal.RoleNotice, "You have mail from alice", func(r *opjournal.Request) {
		r.Producer.Principal = mailNoticePrincipal
	}))
	f.pass(t, l, st)
	nudges := f.nudges()
	if len(nudges) != 1 {
		t.Fatalf("deliveries = %+v, want one", nudges)
	}
	if want := reminderGolden("mail", "You have mail from alice"); nudges[0].Message != want {
		t.Fatalf("delivered text = %q, want %q", nudges[0].Message, want)
	}
	if got := f.get(t, st, op.ID); got.Status != opjournal.StatusClosed {
		t.Fatalf("op after the pass = %+v, want it closed delivered", got)
	}
}
