package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/opjournal"
)

// TestNudgeStatusListsLaneOps pins gc nudge status over lane ops (OJ-21
// 21a): an open lane op is pending, an executing one in flight with its
// lease, a lane op closed failed within the hour dead; a legacy op, an older
// close and a success are left out; the items fit the command's schema v1.
func TestNudgeStatusListsLaneOps(t *testing.T) {
	c := newCLILaneCity(t)
	st := c.laneStore(t)
	// The executing op is taken first: a later take would batch the
	// conversation's other open ops with it.
	withLaneFailpoint(t, "cli:take", true)
	if !crashed(func() { c.nudge(nudgeDeliveryWaitIdle, "k-executing", false, cliLaneTestMessage) }) {
		t.Fatal("the take failpoint did not fire")
	}
	var executing opjournal.Op
	for _, op := range c.ops(t) {
		if op.ClientKey == "k-executing" {
			executing = op
		}
	}
	pending := c.propose(t, st, c.msg("k-pending", opjournal.RoleNotice, "you have mail", func(r *opjournal.Request) {
		r.Producer.Principal = mailNoticePrincipal
	}))
	failed := c.propose(t, st, c.msg("k-failed", opjournal.RoleNotice, "stale"))
	if _, err := st.Close(failed.ID, opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: "expired"}, "lane:ctl"); err != nil {
		t.Fatal(err)
	}
	done := c.propose(t, st, c.msg("k-done", opjournal.RoleNotice, "delivered"))
	if _, err := st.Close(done.ID, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, "lane:ctl"); err != nil {
		t.Fatal(err)
	}
	now := c.clk.Now()
	p, f, d, err := laneNudgeItems(st, c.target, now)
	if err != nil {
		t.Fatalf("laneNudgeItems: %v", err)
	}
	if len(p) != 1 || p[0].ID != pending.ID || p[0].Source != mailNoticePrincipal || p[0].Message != "you have mail" || p[0].Agent != c.target.agentKey() {
		t.Fatalf("pending = %+v, want the open notice", p)
	}
	if !p[0].DeliverAfter.Equal(pending.CreatedAt) {
		t.Fatalf("deliver_after = %v, want the creation %v with no not-before", p[0].DeliverAfter, pending.CreatedAt)
	}
	if len(f) != 1 || f[0].ID != executing.ID || !f[0].LeaseUntil.Equal(executing.Lease.Deadline) || f[0].ClaimedAt.IsZero() || f[0].Attempts != 1 {
		t.Fatalf("in flight = %+v, want the executing op with its lease", f)
	}
	if len(d) != 1 || d[0].ID != failed.ID || d[0].DeadAt.IsZero() || !strings.Contains(d[0].LastError, "failed") {
		t.Fatalf("dead = %+v, want the failed op", d)
	}
	if _, _, later, err := laneNudgeItems(st, c.target, now.Add(2*time.Hour)); err != nil || len(later) != 0 {
		t.Fatalf("dead two hours on = %+v, %v; want none past the window", later, err)
	}

	var out bytes.Buffer
	if err := writeCLIJSONLine(&out, nudgeStatusJSON{
		SchemaVersion: "1", Command: "nudge status", CityPath: c.dir,
		Agent: c.target.agentKey(), Session: c.target.sessionName, SessionID: c.target.sessionID,
		Counts:  nudgeStatusCounts{Pending: len(p), InFlight: len(f), Dead: len(d)},
		Pending: nudgeStatusItems(p), InFlight: nudgeStatusItems(f), Dead: nudgeStatusItems(d),
	}); err != nil {
		t.Fatal(err)
	}
	contentOpValidate(t, "schemas/nudge/status/result.schema.json", out.String())
}

// TestNudgeDropCancelsALaneOp pins gc nudge drop over lane ops: an open op
// closes canceled as the operator (dropped), an executing one records the
// cancel (cancel_requested), both ok; a closed op answers already closed,
// and an id naming no op is left to the queue's answer.
func TestNudgeDropCancelsALaneOp(t *testing.T) {
	c := newCLILaneCity(t)
	st := c.laneStore(t)
	open := c.propose(t, st, c.msg("k-open", opjournal.RoleNotice, "drop me"))
	res, found, err := laneNudgeDrop(st, open.ID)
	if err != nil || !found || !res.OK || res.Outcome != "dropped" {
		t.Fatalf("drop open = %+v, %v, %v; want dropped", res, found, err)
	}
	if got := c.get(t, st, open.ID); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.By != laneNudgeDropPrincipal {
		t.Fatalf("op after drop = %+v, want canceled by the operator", got)
	}
	if res, found, err := laneNudgeDrop(st, open.ID); err != nil || !found || res.OK || res.Error != "already closed" {
		t.Fatalf("drop again = %+v, %v, %v; want already closed", res, found, err)
	}

	withLaneFailpoint(t, "cli:take", true)
	if !crashed(func() { c.nudge(nudgeDeliveryWaitIdle, "k-executing", false, cliLaneTestMessage) }) {
		t.Fatal("the take failpoint did not fire")
	}
	var executing opjournal.Op
	for _, op := range c.ops(t) {
		if op.ClientKey == "k-executing" {
			executing = op
		}
	}
	if res, found, err := laneNudgeDrop(st, executing.ID); err != nil || !found || !res.OK || res.Outcome != "cancel_requested" {
		t.Fatalf("drop executing = %+v, %v, %v; want cancel_requested", res, found, err)
	}
	if res, found, err := laneNudgeDrop(st, "gc-op-nosuch000000000000000000000000"); err != nil || found {
		t.Fatalf("drop of no op = %+v, %v, %v; want not found", res, found, err)
	}
}
