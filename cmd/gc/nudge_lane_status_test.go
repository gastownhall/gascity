package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
)

// TestNudgeStatusListsLaneOps pins gc nudge status over lane ops (OJ-21
// 21a): an open lane op is pending, an executing one in flight with its
// lease, a lane op closed failed within the hour dead; a legacy op, an older
// close, a success and an interrupt op, open or failed, are left out; the
// items fit the command's schema v1.
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
	// A stop (an interrupt op) on the same conversation is no nudge, open
	// or failed.
	c.propose(t, st, laneNudgeInterrupt(c, "stop-open"))
	stopFailed := c.propose(t, st, laneNudgeInterrupt(c, "stop-failed"))
	if _, err := st.Close(stopFailed.ID, opjournal.Outcome{Arm: opjournal.ArmFailed, Reason: "expired"}, "lane:ctl"); err != nil {
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
// cancel (cancel_requested), both ok; a settling op is too late to cancel
// and a closed one answers already closed; an id naming no op, or an
// interrupt op, is left to the queue's answer and not canceled.
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
	if _, err := st.Transition(executing.ID, opjournal.StatusExecuting, opjournal.StatusSettling, "lane:ctl"); err != nil {
		t.Fatal(err)
	}
	if res, found, err := laneNudgeDrop(st, executing.ID); err != nil || !found || res.OK || res.Error != "too late to cancel: its delivery is being settled" {
		t.Fatalf("drop settling = %+v, %v, %v; want too late to cancel", res, found, err)
	}
	if res, found, err := laneNudgeDrop(st, "gc-op-nosuch000000000000000000000000"); err != nil || found {
		t.Fatalf("drop of no op = %+v, %v, %v; want not found", res, found, err)
	}
	stop := c.propose(t, st, laneNudgeInterrupt(c, "stop-1"))
	if res, found, err := laneNudgeDrop(st, stop.ID); err != nil || found {
		t.Fatalf("drop of an interrupt op = %+v, %v, %v; want not found", res, found, err)
	}
	if got := c.get(t, st, stop.ID); got.Status == opjournal.StatusClosed || got.Cancel.Recorded() {
		t.Fatalf("interrupt op after the drop = %s, cancel %+v; want it open and not canceled", got.Status, got.Cancel)
	}
}

// TestNudgeStatusFailsWhenLaneOpsCannotBeRead pins gc nudge status over a
// sessions store that can host lanes but whose ops cannot all be read: an
// open-ops read that fails, an open op row that does not decode, and a
// failed read of the closed ops' history each exit 1 and name the failure
// on stderr. --json then prints no result, whose ok promises the whole
// view, and the text view still lists what was read: the decodable open op
// beside the undecodable row, and the open op when only the history failed.
func TestNudgeStatusFailsWhenLaneOpsCannotBeRead(t *testing.T) {
	store := &laneNudgeFailStore{laneTestStore: newLaneTestStore()}
	c := newCLILaneCityOver(t, store)
	c.nudgeCommandCity(t, store)
	pending := c.propose(t, c.laneStore(t), c.msg("k-pending", opjournal.RoleNotice, "you have mail"))

	code, stdout, stderr := c.nudgeStatus(true)
	var result nudgeStatusJSON
	if code != 0 || json.Unmarshal([]byte(stdout), &result) != nil || result.Counts.Pending != 1 || result.Pending[0].ID != pending.ID {
		t.Fatalf("readable status --json = %d, %q; stderr %s; want the pending op", code, stdout, stderr)
	}

	store.failOpen = true
	code, stdout, stderr = c.nudgeStatus(true)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "conversation-lane ops not all read") || !strings.Contains(stderr, "open hiccup") {
		t.Fatalf("status --json over an unreadable lane store = %d, %q; stderr %q; want exit 1, no result and the failure", code, stdout, stderr)
	}
	code, _, stderr = c.nudgeStatus(false)
	if code != 1 || !strings.Contains(stderr, "open hiccup") {
		t.Fatalf("status over an unreadable lane store = %d; stderr %q; want exit 1 and the failure", code, stderr)
	}

	store.failOpen = false
	row, err := store.Create(beads.Bead{
		Title: "op", Type: "op", Status: "open", Labels: []string{opjournal.LabelOp},
		Metadata: map[string]string{"gc.op.open": c.id, "gc.op.schema": "99"},
	})
	if err != nil {
		t.Fatalf("creating a row from the future: %v", err)
	}
	code, stdout, stderr = c.nudgeStatus(false)
	if code != 1 || !strings.Contains(stdout, "pending  "+pending.ID) || !strings.Contains(stderr, "open op row(s) this gc cannot decode are not listed") || !strings.Contains(stderr, row.ID) {
		t.Fatalf("status beside an undecodable row = %d, %q; stderr %q; want exit 1, the pending op listed and the row named", code, stdout, stderr)
	}
	if code, stdout, _ = c.nudgeStatus(true); code != 1 || stdout != "" {
		t.Fatalf("status --json beside an undecodable row = %d, %q; want exit 1 and no result", code, stdout)
	}
	if err := store.Delete(row.ID); err != nil {
		t.Fatal(err)
	}

	store.failHistory = true
	code, stdout, stderr = c.nudgeStatus(false)
	if code != 1 || !strings.Contains(stdout, "pending  "+pending.ID) || !strings.Contains(stderr, "dead ops not listed") || !strings.Contains(stderr, "history hiccup") {
		t.Fatalf("status over a failed history read = %d, %q; stderr %q; want exit 1 and the pending op still listed", code, stdout, stderr)
	}
	if code, stdout, _ = c.nudgeStatus(true); code != 1 || stdout != "" {
		t.Fatalf("status --json over a failed history read = %d, %q; want exit 1 and no result", code, stdout)
	}
}

// TestNudgeDropReportsEachIDWhenALaneCancelFails pins gc nudge drop over a
// lane op that cannot be read: that id fails on its own, and the lane op
// and the queued nudge after it are still dropped and reported.
func TestNudgeDropReportsEachIDWhenALaneCancelFails(t *testing.T) {
	store := &laneNudgeFailStore{laneTestStore: newLaneTestStore()}
	c := newCLILaneCityOver(t, store)
	c.nudgeCommandCity(t, store)
	st := c.laneStore(t)
	unreadable := c.propose(t, st, c.msg("k-unreadable", opjournal.RoleNotice, "stuck"))
	droppable := c.propose(t, st, c.msg("k-droppable", opjournal.RoleNotice, "drop me"))
	queued := newQueuedNudgeWithOptions(c.target.agentKey(), "queued reminder", "session", time.Now(), queuedNudgeOptions{ID: "n-queued"})
	if err := enqueueQueuedNudge(c.dir, queued); err != nil {
		t.Fatalf("enqueueQueuedNudge: %v", err)
	}
	store.failGet = unreadable.ID

	var stdout, stderr bytes.Buffer
	code := doNudgeDrop(c.dir, []string{unreadable.ID, queued.ID, droppable.ID}, true, &stdout, &stderr)
	var result nudgeDropJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s; stderr %s", err, stdout.String(), stderr.String())
	}
	if code != 1 || result.OK || len(result.Results) != 3 {
		t.Fatalf("drop = %d, %+v; want exit 1 and a result for each of the three ids", code, result)
	}
	if got := result.Results[0]; got.ID != unreadable.ID || got.OK || !strings.Contains(got.Error, "get hiccup") {
		t.Fatalf("unreadable op result = %+v, want it failed with the read error", got)
	}
	if got := result.Results[1]; got.ID != queued.ID || !got.OK || got.Outcome != "dropped" {
		t.Fatalf("queued nudge result = %+v, want it dropped", got)
	}
	if got := result.Results[2]; got.ID != droppable.ID || !got.OK || got.Outcome != "dropped" {
		t.Fatalf("lane op result = %+v, want it dropped", got)
	}
	store.failGet = ""
	if got := c.get(t, st, droppable.ID); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmCanceled {
		t.Fatalf("droppable op = %s %+v, want it canceled", got.Status, got.Outcome)
	}
	if got := c.get(t, st, unreadable.ID); got.Status == opjournal.StatusClosed {
		t.Fatalf("unreadable op = %s %+v, want it still open", got.Status, got.Outcome)
	}
	_, _, dead, err := listQueuedNudgesForTarget(c.dir, c.target, time.Now())
	if err != nil || len(dead) != 1 || dead[0].ID != queued.ID {
		t.Fatalf("queue dead letters = %+v, %v; want the dropped nudge", dead, err)
	}
}

// laneNudgeInterrupt is a stop of the fixture's session under key: an
// interrupt op on the conversation gc nudge status and drop leave alone.
func laneNudgeInterrupt(c *cliLaneCity, key string) opjournal.Request {
	return opjournal.Request{
		Conversation: c.id, Kind: opjournal.KindInterrupt, Executor: opjournal.ExecutorLane, ClientKey: key,
		Producer: opjournal.Producer{Principal: "operator", Actor: opjournal.ActorOperator},
		Scope:    opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1},
	}
}

// nudgeCommandCity points gc nudge status and gc nudge drop at the fixture:
// its city.toml on disk (cityOnDisk), and the city's store opening as store.
func (c *cliLaneCity) nudgeCommandCity(t *testing.T, store beads.Store) {
	t.Helper()
	clearGCEnv(t)
	c.cityOnDisk(t)
	prev := openStoreFactoryForCity
	t.Cleanup(func() { openStoreFactoryForCity = prev })
	openStoreFactoryForCity = func(context.Context, beads.StoreOpenOptions) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: store}, nil
	}
}

// nudgeStatus runs gc nudge status of the fixture's session, asJSON as
// --json.
func (c *cliLaneCity) nudgeStatus(asJSON bool) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := cmdNudgeStatus([]string{c.id}, asJSON, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// laneNudgeFailStore fails the op store's reads gc nudge status and drop
// make on demand: the open ops (failOpen), the closed ops' history
// (failHistory), or one op by id (failGet).
type laneNudgeFailStore struct {
	laneTestStore
	failOpen    bool
	failHistory bool
	failGet     string
}

func (s *laneNudgeFailStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	switch {
	case s.failOpen && q.Metadata["gc.op.open"] != "":
		return nil, errors.New("open hiccup")
	case s.failHistory && q.Metadata["gc.op.status"] == string(opjournal.StatusClosed):
		return nil, errors.New("history hiccup")
	}
	return s.laneTestStore.List(q)
}

func (s *laneNudgeFailStore) Get(id string) (beads.Bead, error) {
	if s.failGet != "" && id == s.failGet {
		return beads.Bead{}, errors.New("get hiccup")
	}
	return s.laneTestStore.Get(id)
}
