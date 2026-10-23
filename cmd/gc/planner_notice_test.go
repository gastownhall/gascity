package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// newNoticeLaneHost registers a running lane host over f's sessions store
// with kinds latched and no context, so Poke starts no pass: the op rows a
// producer writes stay as it wrote them.
func newNoticeLaneHost(t *testing.T, f *laneFixture, kinds ...string) *conversationLaneHost {
	t.Helper()
	return newNoticeLaneHostOver(t, f, f.store, kinds...)
}

// newNoticeLaneHostOver is newNoticeLaneHost with the host's sessions store
// set to store.
func newNoticeLaneHostOver(t *testing.T, f *laneFixture, store beads.Store, kinds ...string) *conversationLaneHost {
	t.Helper()
	h := newConversationLaneHost(&laneHostEnvFake{f: f, store: store}, kinds)
	h.clk = f.clk
	h.state = laneHostStateRunning
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	registerLaneHost(ctx, f.dir, h)
	return h
}

// noticeKey is a planner notice's client key, spelled out here so the tests
// pin its format: <purpose>:<token>:<trigger>[:<episode>]:<attempt>.
func noticeKey(token, purpose, trigger, episode string, attempt int) string {
	parts := []string{purpose, token, trigger}
	if episode != "" {
		parts = append(parts, episode)
	}
	return strings.Join(append(parts, strconv.Itoa(attempt)), ":")
}

// noticeOpID is the pinned id of a planner notice's attempt for f's
// incarnation.
func noticeOpID(t *testing.T, f *laneFixture, purpose, trigger, episode string, attempt int) string {
	t.Helper()
	id, err := opjournal.DeriveID("gc", f.id, plannerNoticePrincipalPrefix+purpose, noticeKey(f.token, purpose, trigger, episode, attempt))
	if err != nil {
		t.Fatalf("DeriveID: %v", err)
	}
	return id
}

// noticeOpAbsent fails unless the op id names no row.
func noticeOpAbsent(t *testing.T, st *opjournal.Store, id string) {
	t.Helper()
	if _, err := st.Get(id); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(%s) = %v, want not found", id, err)
	}
}

// row reads f's session row.
func (f *laneFixture) row(t *testing.T) beads.Bead {
	t.Helper()
	b, err := f.store.Get(f.id)
	if err != nil {
		t.Fatalf("reading the session row: %v", err)
	}
	return b
}

// openOps are f's conversation's open ops.
func (f *laneFixture) openOps(t *testing.T) []opjournal.Op {
	t.Helper()
	ops, err := f.laneStore(t).ListOpen(f.id)
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	return ops
}

// failingOpListStore is a sessions store whose op listing (the read every
// planner notice starts with) fails while fail is set.
type failingOpListStore struct {
	laneTestStore
	fail atomic.Bool
}

func (s *failingOpListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.fail.Load() && q.Metadata["gc.op.open"] != "" {
		return nil, errors.New("op listing unavailable")
	}
	return s.laneTestStore.List(q)
}

func TestPlannerNoticeLaneForFollowsTheSwitch(t *testing.T) {
	f := newLaneFixture(t)
	if got := plannerNoticeLaneFor(f.store); got != nil {
		t.Fatalf("no host: lane = %v, want nil", got)
	}
	newNoticeLaneHost(t, f)
	if got := plannerNoticeLaneFor(f.store); got != nil {
		t.Fatalf("host with [ops] authoritative empty: lane = %v, want nil", got)
	}

	g := newLaneFixture(t)
	h := newNoticeLaneHost(t, g, "message")
	for name, store := range map[string]beads.Store{
		"the store":          g.store,
		"its class view":     beads.SessionStore{Store: g.store},
		"its live lane view": laneLiveSessions{Store: g.store},
		"a cache over it":    beads.NewCachingStore(g.store, nil),
	} {
		if got := plannerNoticeLaneFor(store); got != plannerNoticeLane(h) {
			t.Errorf("%s: lane = %v, want the host", name, got)
		}
	}
	if got := plannerNoticeLaneFor(newLaneTestStore()); got != nil {
		t.Errorf("another store: lane = %v, want nil", got)
	}
	if got := plannerNoticeLaneFor(nil); got != nil {
		t.Errorf("nil store: lane = %v, want nil", got)
	}
	h.mu.Lock()
	h.state = laneHostStateOff
	h.mu.Unlock()
	if got := plannerNoticeLaneFor(g.store); got != nil {
		t.Errorf("host not running: lane = %v, want nil", got)
	}

	// A host over a cache is found from the cache's backing too.
	c := newLaneFixture(t)
	hc := newNoticeLaneHostOver(t, c, beads.NewCachingStore(c.store, nil), "message")
	if got := plannerNoticeLaneFor(c.store); got != plannerNoticeLane(hc) {
		t.Errorf("the backing of the host's cache: lane = %v, want the host", got)
	}
}

// TestPlannerNoticeLaneForFindsTheHostFromBothReconcilers pins that one
// edit serves legacy and v2: the store the legacy tick hands the backstops
// (CityRuntime.sessionsBeadStore, city_runtime.go) and the one v2's C8 hands
// them (beads.SessionStore over gatherEnv.Sessions, which newPlannerHost
// wires to CityRuntime.v2SessionsStore) both find the lane host the
// controller starts over controllerState.
func TestPlannerNoticeLaneForFindsTheHostFromBothReconcilers(t *testing.T) {
	f := newLaneFixture(t)
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Ops: config.OpsConfig{Authoritative: []string{"message"}}}
	cs := &controllerState{cfg: cfg, cityPath: f.dir, cityName: "test-city", cityBeadStore: beads.NewCachingStore(f.store, nil)}
	cr := &CityRuntime{cityName: "test-city", cityPath: f.dir, cfg: cfg, cs: cs}
	h := newConversationLaneHost(cs, cfg.Ops.Authoritative)
	h.state = laneHostStateRunning
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	registerLaneHost(ctx, f.dir, h)

	if got := plannerNoticeLaneFor(cr.sessionsBeadStore()); got != plannerNoticeLane(h) {
		t.Errorf("legacy tick's sessions store: lane = %v, want the controller's host", got)
	}
	if got := plannerNoticeLaneFor(beads.SessionStore{Store: cr.v2SessionsStore()}); got != plannerNoticeLane(h) {
		t.Errorf("v2 C8's sessions store: lane = %v, want the controller's host", got)
	}
}

func TestProposePlannerNoticeWritesANoticeOp(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	n := plannerNotice{
		Purpose: "idle-claim-nudge", Trigger: "wb-1/rig:a", Causation: "wb-1", Episode: "1700000000",
		Attempt: 1, Content: "claim it", StartWithin: 3 * time.Minute,
	}
	outcome, op, err := proposePlannerNotice(h, f.row(t), n)
	if err != nil || outcome != plannerNoticeProposed {
		t.Fatalf("propose = %v, %v; want proposed", outcome, err)
	}
	if want := noticeOpID(t, f, "idle-claim-nudge", "wb-1/rig:a", "1700000000", 1); op.ID != want {
		t.Fatalf("op id = %s, want %s", op.ID, want)
	}
	got := f.get(t, f.laneStore(t), op.ID)
	m := got.Policy.Message
	switch {
	case got.Kind != opjournal.KindMessage || got.Executor != opjournal.ExecutorLane:
		t.Errorf("kind/executor = %s/%s, want message/lane", got.Kind, got.Executor)
	case m.Role != opjournal.RoleNotice || m.When != opjournal.WhenBoundary || m.Wake != opjournal.WakeLiveOnly:
		t.Errorf("policy = %+v, want notice, boundary, live_only", m)
	case got.Producer.Principal != "gc:planner/idle-claim-nudge" || got.Producer.Actor != opjournal.ActorController:
		t.Errorf("producer = %+v, want gc:planner/idle-claim-nudge as the controller", got.Producer)
	case got.ClientKey != "idle-claim-nudge:tok-1:wb-1/rig:a:1700000000:1":
		t.Errorf("client key = %q", got.ClientKey)
	case got.Causation != "wb-1" || got.Payload != "claim it":
		t.Errorf("causation/payload = %q/%q, want the bead id and the text", got.Causation, got.Payload)
	case !got.StartBy.Equal(laneT0.Add(3 * time.Minute)):
		t.Errorf("start_by = %s, want the notice's window after %s", got.StartBy, laneT0)
	}

	// The same attempt again is the same op: nothing new is written.
	again, op2, err := proposePlannerNotice(h, f.row(t), n)
	if err != nil || again != plannerNoticeAlreadyProposed || op2.ID != op.ID || op2.Revision != got.Revision {
		t.Fatalf("re-propose = %v %s rev %d, %v; want the stored op unchanged", again, op2.ID, op2.Revision, err)
	}
}

// TestProposePlannerNoticeReproposesTheSameOpAfterTheClockAdvances is a
// crash between the op and the producer's marker: the retry comes later,
// with a later start_by, and still finds the same op.
func TestProposePlannerNoticeReproposesTheSameOpAfterTheClockAdvances(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	n := plannerNotice{Purpose: "idle-claim-nudge", Trigger: "wb-1", Causation: "wb-1", Episode: "1", Attempt: 1, Content: "claim it", StartWithin: time.Minute}
	_, op, err := proposePlannerNotice(h, f.row(t), n)
	if err != nil {
		t.Fatalf("propose: %v", err)
	}
	for _, step := range []time.Duration{30 * time.Second, time.Hour} {
		f.clk.Advance(step)
		outcome, again, err := proposePlannerNotice(h, f.row(t), n)
		if err != nil || outcome != plannerNoticeAlreadyProposed || again.ID != op.ID || again.Revision != op.Revision {
			t.Fatalf("retry %s later = %v %s rev %d, %v; want the same op, unchanged", step, outcome, again.ID, again.Revision, err)
		}
	}
	if n := len(f.openOps(t)); n != 1 {
		t.Fatalf("open ops = %d, want the one", n)
	}
}

// TestProposePlannerNoticeSupersedesAnUnstartedNotice: a reminder's next
// attempt cancels its earlier notice that has not started, so one notice
// stands, with the latest text; another reminder's notice, or another
// trigger's, is not touched. Once the lane has started a notice, the next
// attempt waits on it instead.
func TestProposePlannerNoticeSupersedesAnUnstartedNotice(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	n := plannerNotice{Purpose: "idle-claim-nudge", Trigger: "wb-1", Causation: "wb-1", Episode: "1", Attempt: 1, Content: "claim it", StartWithin: time.Hour}
	_, first, err := proposePlannerNotice(h, f.row(t), n)
	if err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	var others []string
	for _, o := range []plannerNotice{
		{Purpose: "continuation-claim-nudge", Trigger: "wb-1", Causation: "wb-1", Attempt: 1, Content: "x", StartWithin: time.Hour},
		{Purpose: "idle-claim-nudge", Trigger: "wb-2", Causation: "wb-2", Attempt: 1, Content: "x", StartWithin: time.Hour},
	} {
		outcome, op, err := proposePlannerNotice(h, f.row(t), o)
		if err != nil || outcome != plannerNoticeProposed {
			t.Fatalf("%s for %s = %v, %v; want proposed", o.Purpose, o.Trigger, outcome, err)
		}
		others = append(others, op.ID)
	}

	n.Attempt, n.Episode, n.Content = 2, "2", "claim it now"
	outcome, second, err := proposePlannerNotice(h, f.row(t), n)
	if err != nil || outcome != plannerNoticeProposed || second.Payload != "claim it now" {
		t.Fatalf("attempt 2 while 1 is unstarted = %v %q, %v; want proposed with the new text", outcome, second.Payload, err)
	}
	if got := f.get(t, st, first.ID); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.Reason != "superseded by "+second.ID {
		t.Fatalf("attempt 1 = %s %+v, want canceled as superseded by attempt 2", got.Status, got.Cancel)
	}
	for _, id := range others {
		if got := f.get(t, st, id); got.Status == opjournal.StatusClosed {
			t.Errorf("another reminder's notice %s was closed: %+v", id, got.Cancel)
		}
	}

	// Once the lane has started attempt 2, attempt 3 waits on it, due or not.
	if _, err := st.Transition(second.ID, opjournal.StatusSubmitted, opjournal.StatusExecuting, laneHostName); err != nil {
		t.Fatalf("starting attempt 2: %v", err)
	}
	n.Attempt, n.Episode = 3, "3"
	for _, step := range []time.Duration{time.Second, 2 * time.Hour} {
		f.clk.Advance(step)
		outcome, prev, err := proposePlannerNotice(h, f.row(t), n)
		if err != nil || outcome != plannerNoticePending || prev.ID != second.ID {
			t.Fatalf("attempt 3, %s later, while 2 is in flight = %v %s, %v; want pending on attempt 2", step, outcome, prev.ID, err)
		}
	}
	noticeOpAbsent(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", "3", 3))
	if got := f.get(t, st, second.ID); got.Status != opjournal.StatusExecuting || got.Cancel.Recorded() {
		t.Fatalf("attempt 2 = %s, cancel %+v; want executing, not canceled", got.Status, got.Cancel)
	}
}

// TestProposePlannerNoticeLateRetryOfASupersededAttemptCancelsNothing: a
// retry of attempt 1 that lands after attempt 2 superseded it finds its own
// op, closed, and cancels nothing, so attempt 2's notice still stands.
func TestProposePlannerNoticeLateRetryOfASupersededAttemptCancelsNothing(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	n1 := plannerNotice{Purpose: "idle-claim-nudge", Trigger: "wb-1", Causation: "wb-1", Episode: "1", Attempt: 1, Content: "claim it", StartWithin: time.Hour}
	_, first, err := proposePlannerNotice(h, f.row(t), n1)
	if err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	n2 := n1
	n2.Attempt, n2.Episode = 2, "2"
	_, second, err := proposePlannerNotice(h, f.row(t), n2)
	if err != nil {
		t.Fatalf("attempt 2: %v", err)
	}
	outcome, op, err := proposePlannerNotice(h, f.row(t), n1)
	if err != nil || outcome != plannerNoticeAlreadyProposed || op.ID != first.ID || op.Status != opjournal.StatusClosed {
		t.Fatalf("late retry of attempt 1 = %v %s %s, %v; want its own closed op", outcome, op.ID, op.Status, err)
	}
	if got := f.get(t, f.laneStore(t), second.ID); got.Status == opjournal.StatusClosed {
		t.Fatalf("attempt 2 was closed by attempt 1's retry: %+v", got.Cancel)
	}
}

// TestProposePlannerNoticeScopesAReminderToItsTrigger: one bead id in two
// stores is two triggers, so two reminders. Neither supersedes nor holds
// the other's notice.
func TestProposePlannerNoticeScopesAReminderToItsTrigger(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	var notices []plannerNotice
	var ids []string
	for _, ref := range []string{"rig:a", "rig:b"} {
		n := plannerNotice{
			Purpose: "idle-claim-nudge", Trigger: backstopNoticeTrigger(backstopTarget{ID: "wb-1", StoreRef: ref}),
			Causation: "wb-1", Episode: "1", Attempt: 1, Content: "claim wb-1", StartWithin: time.Hour,
		}
		outcome, op, err := proposePlannerNotice(h, f.row(t), n)
		if err != nil || outcome != plannerNoticeProposed {
			t.Fatalf("wb-1 in %s = %v, %v; want proposed", ref, outcome, err)
		}
		notices, ids = append(notices, n), append(ids, op.ID)
	}
	if n := len(f.openOps(t)); n != 2 {
		t.Fatalf("open ops = %d, want one per store", n)
	}
	if _, _, err := f.laneStore(t).Cancel(ids[0], "test", "done"); err != nil {
		t.Fatalf("closing rig:a's notice: %v", err)
	}
	for i, want := range []bool{false, true} {
		if open, err := plannerNoticeOpen(h, f.row(t), notices[i]); err != nil || open != want {
			t.Errorf("notice %d open = %v, %v; want %v", i, open, err, want)
		}
	}
}

// TestWithdrawPlannerNoticesCancelsThePurposesOpenNotices: every open notice
// of the purpose goes, whatever its trigger or incarnation; one in flight
// keeps the cancel as a request for the lane, recorded once; another
// purpose's notice stays.
func TestWithdrawPlannerNoticesCancelsThePurposesOpenNotices(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	propose := func(purpose, trigger string) opjournal.Op {
		t.Helper()
		_, op, err := proposePlannerNotice(h, f.row(t), plannerNotice{
			Purpose: purpose, Trigger: trigger, Causation: trigger, Episode: "1", Attempt: 1, Content: "claim " + trigger, StartWithin: time.Hour,
		})
		if err != nil {
			t.Fatalf("proposing %s for %s: %v", purpose, trigger, err)
		}
		return op
	}
	unstarted := propose("idle-claim-nudge", "wb-1")
	inFlight := propose("idle-claim-nudge", "wb-2")
	if _, err := st.Transition(inFlight.ID, opjournal.StatusSubmitted, opjournal.StatusExecuting, laneHostName); err != nil {
		t.Fatalf("starting the wb-2 notice: %v", err)
	}
	if err := f.store.SetMetadata(f.id, "instance_token", "tok-2"); err != nil {
		t.Fatalf("respawning: %v", err)
	}
	respawned := propose("idle-claim-nudge", "wb-1")
	other := propose("continuation-claim-nudge", "wb-1")

	const reason = "no work is outstanding"
	if err := withdrawPlannerNotices(h, f.row(t), "idle-claim-nudge", reason); err != nil {
		t.Fatalf("withdraw: %v", err)
	}
	for _, id := range []string{unstarted.ID, respawned.ID} {
		if got := f.get(t, st, id); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.Reason != reason || got.Cancel.By != "gc:planner/idle-claim-nudge" {
			t.Errorf("notice %s = %s %+v, want withdrawn by its producer", id, got.Status, got.Cancel)
		}
	}
	requested := f.get(t, st, inFlight.ID)
	if requested.Status != opjournal.StatusExecuting || !requested.Cancel.Recorded() || requested.Cancel.Reason != reason {
		t.Fatalf("the in-flight notice = %s %+v, want still executing with the cancel requested", requested.Status, requested.Cancel)
	}
	if got := f.get(t, st, other.ID); got.Status == opjournal.StatusClosed {
		t.Fatalf("another purpose's notice was closed: %+v", got.Cancel)
	}

	if err := withdrawPlannerNotices(h, f.row(t), "idle-claim-nudge", reason); err != nil {
		t.Fatalf("withdraw again: %v", err)
	}
	if got := f.get(t, st, inFlight.ID); got.Revision != requested.Revision {
		t.Fatalf("withdrawing again rewrote the in-flight notice: revision %d -> %d", requested.Revision, got.Revision)
	}
}

// TestProposePlannerNoticeIgnoresAnotherIncarnationsNotice pins the
// incarnation scope: a respawned session is reminded afresh even while its
// predecessor's notice is open.
func TestProposePlannerNoticeIgnoresAnotherIncarnationsNotice(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	n := plannerNotice{Purpose: "idle-claim-nudge", Trigger: "wb-1", Causation: "wb-1", Episode: "1", Attempt: 1, Content: "claim it", StartWithin: time.Hour}
	if _, _, err := proposePlannerNotice(h, f.row(t), n); err != nil {
		t.Fatalf("first incarnation: %v", err)
	}
	if err := f.store.SetMetadata(f.id, "instance_token", "tok-2"); err != nil {
		t.Fatalf("respawning: %v", err)
	}
	if outcome, _, err := proposePlannerNotice(h, f.row(t), n); err != nil || outcome != plannerNoticeProposed {
		t.Fatalf("the respawned incarnation = %v, %v; want proposed", outcome, err)
	}
}

func TestPlannerNoticeTokenFallsBackToTheGeneration(t *testing.T) {
	if got := plannerNoticeToken(beads.Bead{Metadata: map[string]string{"instance_token": " tok-9 ", "generation": "4"}}); got != "tok-9" {
		t.Errorf("token = %q, want the instance token", got)
	}
	if got := plannerNoticeToken(beads.Bead{Metadata: map[string]string{"generation": "4"}}); got != "g4" {
		t.Errorf("token-less row = %q, want g4, the generation", got)
	}
	a := plannerNoticeToken(beads.Bead{Metadata: map[string]string{"generation": "4"}})
	b := plannerNoticeToken(beads.Bead{Metadata: map[string]string{"generation": "5"}})
	if a == b {
		t.Errorf("two generations of a token-less row share %q", a)
	}
}

func TestPlannerNoticeStartWithinLeavesAMargin(t *testing.T) {
	if got, want := plannerNoticeStartWithin(idleClaimNudgeBackoff), idleClaimNudgeBackoff-plannerNoticeStartMargin; got != want {
		t.Errorf("backoff window = %s, want %s", got, want)
	}
	if got := plannerNoticeStartWithin(20 * time.Second); got != 10*time.Second {
		t.Errorf("short cadence window = %s, want half of it", got)
	}
	if plannerNoticeStartMargin <= time.Second {
		t.Errorf("margin %s does not cover the pacing time's one-second truncation", plannerNoticeStartMargin)
	}
}

func TestProposePlannerNoticeRefusesBadInput(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	if outcome, _, err := proposePlannerNotice(nil, f.row(t), plannerNotice{Purpose: "p", Trigger: "t", Attempt: 1, Content: "x"}); err == nil || outcome != plannerNoticeNone {
		t.Errorf("nil lane = %v, %v; want an error and no outcome", outcome, err)
	}
	if _, _, err := proposePlannerNotice(h, f.row(t), plannerNotice{Purpose: "p", Trigger: "t", Attempt: 0, Content: "x"}); err == nil {
		t.Error("attempt 0: want an error")
	}
	_, _, err := proposePlannerNotice(h, f.row(t), plannerNotice{Purpose: "p", Trigger: "t", Attempt: 1})
	if err == nil || !plannerNoticeRefused(err) {
		t.Errorf("empty content = %v; want a refusal", err)
	}
}

func TestPlannerNoticeStillDue(t *testing.T) {
	now := laneT0
	for _, tc := range []struct {
		status  opjournal.Status
		startBy time.Time
		want    bool
	}{
		{opjournal.StatusSubmitted, now.Add(time.Minute), true},
		{opjournal.StatusAccepted, now.Add(time.Minute), true},
		{opjournal.StatusSubmitted, now, false},
		{opjournal.StatusAccepted, now.Add(-time.Minute), false},
		{opjournal.StatusExecuting, now.Add(-time.Minute), true},
		{opjournal.StatusSettling, now.Add(-time.Minute), true},
		{opjournal.StatusClosed, now.Add(time.Minute), false},
	} {
		if got := plannerNoticeStillDue(opjournal.Op{Status: tc.status, StartBy: tc.startBy}, now); got != tc.want {
			t.Errorf("%s with start_by %s: due = %v, want %v", tc.status, tc.startBy.Sub(now), got, tc.want)
		}
	}
}

// noticeTestBackstop is a backstop predicate that has one target
// outstanding, or nothing once done is set, and paces through two metadata
// keys on the session row. exhaustedCalls, when set, counts its terminal
// actions; failReserve, when set, fails that many reservations first.
type noticeTestBackstop struct {
	target         backstopTarget
	text           string
	done           bool
	exhaustedCalls *int
	failReserve    *int
}

// The predicate paces through the claim backstop's registered keys.
const (
	noticeTestAttemptsKey = idleClaimNudgeCountKey
	noticeTestAtKey       = idleClaimNudgeAtKey
)

func (noticeTestBackstop) governs(beads.Bead) bool { return true }
func (p noticeTestBackstop) resolve(beads.Bead, map[string]beads.Bead, string) (backstopTarget, backstopResolution) {
	if p.done {
		return backstopTarget{}, backstopResolutionClear
	}
	return p.target, backstopResolutionOutstanding
}

func (noticeTestBackstop) state(s beads.Bead, _ backstopTarget) (bool, int, time.Time) {
	at, _ := time.Parse(time.RFC3339, s.Metadata[noticeTestAtKey])
	return true, atoiOr0(s.Metadata[noticeTestAttemptsKey]), at
}
func (p noticeTestBackstop) content(beads.Bead) string { return p.text }
func (noticeTestBackstop) revalidate(beads.Bead, string, backstopTarget) (backstopResolution, string) {
	return backstopResolutionOutstanding, ""
}
func (noticeTestBackstop) observe(beads.Store, *beads.Bead, backstopTarget, time.Time, io.Writer) {}
func (p noticeTestBackstop) reserve(store beads.Store, s *beads.Bead, _ backstopTarget, attempts int, now time.Time, _ io.Writer) bool {
	if p.failReserve != nil && *p.failReserve > 0 {
		*p.failReserve--
		return false
	}
	kv := map[string]string{noticeTestAttemptsKey: strconv.Itoa(attempts), noticeTestAtKey: now.UTC().Format(time.RFC3339)}
	if err := store.SetMetadataBatch(s.ID, kv); err != nil {
		return false
	}
	for k, v := range kv {
		s.Metadata[k] = v
	}
	return true
}

func (p noticeTestBackstop) exhausted(beads.Store, *beads.Bead, io.Writer) {
	if p.exhaustedCalls != nil {
		*p.exhaustedCalls++
	}
}

// clear wipes the pacing keys and reports whether there were any.
func (noticeTestBackstop) clear(store beads.Store, s *beads.Bead, _ io.Writer) bool {
	if s.Metadata[noticeTestAttemptsKey] == "" && s.Metadata[noticeTestAtKey] == "" {
		return false
	}
	kv := map[string]string{noticeTestAttemptsKey: "", noticeTestAtKey: ""}
	if err := store.SetMetadataBatch(s.ID, kv); err != nil {
		return false
	}
	for k := range kv {
		delete(s.Metadata, k)
	}
	return true
}

// backstopRows reads f's session row, its pacing set as attempts reserved
// at last.
func backstopRows(t *testing.T, f *laneFixture, attempts int, last time.Time) []beads.Bead {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.id, map[string]string{
		noticeTestAttemptsKey: strconv.Itoa(attempts), noticeTestAtKey: last.UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seeding the pacing: %v", err)
	}
	return []beads.Bead{f.row(t)}
}

// runNoticeBackstop runs the backstop over rows with pred's target as the
// tick's work.
func runNoticeBackstop(f *laneFixture, rows []beads.Bead, pred noticeTestBackstop, out *bytes.Buffer) {
	work := []beads.Bead{{ID: pred.target.ID, Status: "open"}}
	runNudgeBackstop(f.sp, f.store, rows, work, f.clk.Now(), out, "idle-claim-nudge", pred)
}

// episode is the Episode a backstop attempt paced from last carries.
func episode(last time.Time) string { return strconv.FormatInt(last.Truncate(time.Second).Unix(), 10) }

func TestRunNudgeBackstopProposesNoticeOpsWhenTheLaneExecutesMessages(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}
	var out bytes.Buffer

	observed := f.clk.Now().Add(-time.Hour)
	runNoticeBackstop(f, backstopRows(t, f, 0, observed), pred, &out)
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("the backstop typed %d nudges itself, want 0", n)
	}
	op := f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(observed), 1))
	if op.Payload != "claim wb-1" || op.Policy.Message.Role != opjournal.RoleNotice || op.Causation != "wb-1" {
		t.Fatalf("op = %+v, want the claim notice", op)
	}
	if want := f.clk.Now().Add(idleClaimNudgeBackoff - plannerNoticeStartMargin); !op.StartBy.Equal(want) {
		t.Errorf("start_by = %s, want %s: the backoff less the margin", op.StartBy, want)
	}
	if got := f.meta(t, noticeTestAttemptsKey); got != "1" {
		t.Fatalf("reserved attempts = %q, want 1", got)
	}
	if !strings.Contains(out.String(), "idle-claim-nudge: queued notice for "+f.name+" for wb-1 (attempt 1/3)") {
		t.Errorf("journal = %q", out.String())
	}

	// The next attempt supersedes attempt 1, which has not started.
	f.clk.Advance(time.Minute)
	reserved := f.clk.Now().Add(-idleClaimNudgeBackoff)
	runNoticeBackstop(f, backstopRows(t, f, 1, reserved), pred, &out)
	second := f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(reserved), 2))
	if got := f.get(t, st, op.ID); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.Reason != "superseded by "+second.ID {
		t.Fatalf("attempt 1 = %s %+v, want canceled as superseded by attempt 2", got.Status, got.Cancel)
	}
	if got := f.meta(t, noticeTestAttemptsKey); got != "2" {
		t.Fatalf("reserved attempts = %q, want 2", got)
	}

	// While attempt 2 is in flight, attempt 3 writes nothing and is not spent.
	if _, err := st.Transition(second.ID, opjournal.StatusSubmitted, opjournal.StatusExecuting, laneHostName); err != nil {
		t.Fatalf("starting attempt 2: %v", err)
	}
	f.clk.Advance(idleClaimNudgeBackoff)
	reserved = f.clk.Now().Add(-idleClaimNudgeBackoff)
	runNoticeBackstop(f, backstopRows(t, f, 2, reserved), pred, &out)
	noticeOpAbsent(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(reserved), 3))
	if got := f.meta(t, noticeTestAttemptsKey); got != "2" {
		t.Fatalf("reserved attempts = %q, want still 2 while attempt 2 is in flight", got)
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("the backstop typed %d nudges itself, want 0", n)
	}
}

// TestRunNudgeBackstopNextAttemptFindsTheLastExpired pins the start_by
// margin: one backoff after a reservation, as the pacing marker stores it
// (to the second), the previous notice is past its start_by, so the next
// attempt goes out on that tick instead of waiting another.
func TestRunNudgeBackstopNextAttemptFindsTheLastExpired(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	f.clk.Advance(900 * time.Millisecond)
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}
	var out bytes.Buffer
	runNoticeBackstop(f, backstopRows(t, f, 0, f.clk.Now().Add(-time.Hour)), pred, &out)
	last, _ := time.Parse(time.RFC3339, f.meta(t, noticeTestAtKey))
	f.clk.Advance(last.Add(idleClaimNudgeBackoff).Sub(f.clk.Now()))
	runNoticeBackstop(f, []beads.Bead{f.row(t)}, pred, &out)
	if got := f.meta(t, noticeTestAttemptsKey); got != "2" {
		t.Fatalf("reserved attempts = %q one backoff after attempt 1, want 2; journal %q", got, out.String())
	}
}

// TestRunNudgeBackstopRemindsAReobservedTargetAfresh: the predicate cleared
// its markers and observed the same target again in one incarnation. The new
// episode's attempt 1 is a new op, not the old one, even with edited text.
func TestRunNudgeBackstopRemindsAReobservedTargetAfresh(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	var out bytes.Buffer
	first := f.clk.Now().Add(-time.Hour)
	runNoticeBackstop(f, backstopRows(t, f, 0, first), noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}, &out)

	f.clk.Advance(time.Hour)
	second := f.clk.Now().Add(-idleClaimNudgeGrace)
	out.Reset()
	runNoticeBackstop(f, backstopRows(t, f, 0, second), noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1 (edited)"}, &out)
	op := f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(second), 1))
	if op.Payload != "claim wb-1 (edited)" {
		t.Fatalf("episode 2's notice = %q, want the edited text", op.Payload)
	}
	if got := f.meta(t, noticeTestAttemptsKey); got != "1" {
		t.Fatalf("reserved attempts = %q, want 1; journal %q", got, out.String())
	}
	if n := len(f.openOps(t)); n != 2 {
		t.Fatalf("open ops = %d, want both episodes' notices", n)
	}
}

// TestRunNudgeBackstopReobservedTargetSupersedesItsDueNotice: the target is
// observed again while its last episode's notice is still due. The new
// episode's notice supersedes it, so the stale text never reaches the
// boundary.
func TestRunNudgeBackstopReobservedTargetSupersedesItsDueNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	var out bytes.Buffer
	first := f.clk.Now().Add(-time.Hour)
	runNoticeBackstop(f, backstopRows(t, f, 0, first), noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}, &out)
	stale := f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(first), 1))

	f.clk.Advance(time.Minute)
	second := f.clk.Now().Add(-idleClaimNudgeGrace)
	runNoticeBackstop(f, backstopRows(t, f, 0, second), noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1 (edited)"}, &out)
	fresh := f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(second), 1))
	if fresh.Payload != "claim wb-1 (edited)" {
		t.Fatalf("episode 2's notice = %q, want the edited text", fresh.Payload)
	}
	if got := f.get(t, st, stale.ID); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.Reason != "superseded by "+fresh.ID {
		t.Fatalf("episode 1's notice = %s %+v, want canceled as superseded by episode 2's", got.Status, got.Cancel)
	}
	if got := f.meta(t, noticeTestAttemptsKey); got != "1" {
		t.Fatalf("reserved attempts = %q, want 1; journal %q", got, out.String())
	}
	if n := len(f.openOps(t)); n != 1 {
		t.Fatalf("open ops = %d, want only episode 2's notice", n)
	}
}

// targetKeyedBackstop is noticeTestBackstop with its pacing kept for one
// target at a time, as a real predicate's is: state reads the target its
// marker names, and observe writes that marker with a fresh grace clock.
type targetKeyedBackstop struct{ noticeTestBackstop }

// noticeTestTargetKey is targetKeyedBackstop's marker, the claim backstop's.
const noticeTestTargetKey = idleClaimNudgeTriggerKey

func (targetKeyedBackstop) state(s beads.Bead, target backstopTarget) (bool, int, time.Time) {
	at, _ := time.Parse(time.RFC3339, s.Metadata[noticeTestAtKey])
	return s.Metadata[noticeTestTargetKey] == target.ID, atoiOr0(s.Metadata[noticeTestAttemptsKey]), at
}

func (targetKeyedBackstop) observe(store beads.Store, s *beads.Bead, target backstopTarget, now time.Time, _ io.Writer) {
	kv := map[string]string{noticeTestTargetKey: target.ID, noticeTestAttemptsKey: "0", noticeTestAtKey: now.UTC().Format(time.RFC3339)}
	if err := store.SetMetadataBatch(s.ID, kv); err != nil {
		return
	}
	for k, v := range kv {
		s.Metadata[k] = v
	}
}

// TestRunNudgeBackstopRetargetWithdrawsThePreviousTargetsNotice: the
// outstanding target changes from A to B with no tick between that finds
// nothing outstanding. B's notice cannot supersede A's (another trigger is
// another correlation), so the tick that observes B withdraws A's, and the
// slot is reminded of B alone, never of A and B in one turn.
func TestRunNudgeBackstopRetargetWithdrawsThePreviousTargetsNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message") // no pass: the ops stay as written
	st := f.laneStore(t)
	var out bytes.Buffer
	run := func(id string) {
		pred := targetKeyedBackstop{noticeTestBackstop{target: backstopTarget{ID: id}, text: "claim " + id}}
		runNudgeBackstop(f.sp, f.store, []beads.Bead{f.row(t)}, []beads.Bead{{ID: id, Status: "open"}}, f.clk.Now(), &out, "idle-claim-nudge", pred)
	}
	if err := f.store.SetMetadataBatch(f.id, map[string]string{noticeTestTargetKey: "wb-A"}); err != nil {
		t.Fatalf("seeding the marker: %v", err)
	}
	observed := f.clk.Now().Add(-time.Hour)
	backstopRows(t, f, 0, observed)
	run("wb-A")
	a := f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-A", episode(observed), 1))

	f.clk.Advance(30 * time.Second)
	swapped := f.clk.Now()
	run("wb-B")
	if got := f.get(t, st, a.ID); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.Reason != "the reminder's target changed" || got.Cancel.By != "gc:planner/idle-claim-nudge" {
		t.Fatalf("A's notice = %s %+v, want withdrawn by its producer on the tick that observes B; journal %q", got.Status, got.Cancel, out.String())
	}
	if got := f.meta(t, noticeTestTargetKey); got != "wb-B" {
		t.Fatalf("marker = %q, want wb-B", got)
	}

	f.clk.Advance(idleClaimNudgeGrace)
	run("wb-B")
	b := noticeOpID(t, f, "idle-claim-nudge", "wb-B", episode(swapped), 1)
	hh := newLaneHostHarness(t, f, "message")
	hh.start() // its boot scan delivers what is queued
	deadline := time.After(laneWaitBudget)
	for {
		if got := f.get(t, st, b); got.Status == opjournal.StatusClosed {
			if got.Outcome.Arm != opjournal.ArmSucceeded {
				t.Fatalf("B's notice: outcome = %+v, want succeeded", got.Outcome)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("the host never delivered B's notice")
		case <-hh.passes:
		case <-time.After(5 * time.Millisecond):
		}
	}
	nudges := f.nudges()
	if len(nudges) != 1 || !strings.Contains(nudges[0].Message, "claim wb-B") || strings.Contains(nudges[0].Message, "claim wb-A") {
		t.Fatalf("nudges = %+v, want one delivery carrying only B's claim text", nudges)
	}
}

// TestRunNudgeBackstopWithdrawsItsNoticesWhenTheWorkClears: once nothing is
// outstanding the backstop withdraws its own open notices along with its
// pacing state, leaves another backstop's alone, and a later tick with
// nothing to wipe reads no ops.
func TestRunNudgeBackstopWithdrawsItsNoticesWhenTheWorkClears(t *testing.T) {
	store := &countingOpListStore{laneTestStore: newLaneTestStore()}
	f := newLaneFixtureOver(t, store)
	h := newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}
	var out bytes.Buffer
	observed := f.clk.Now().Add(-time.Hour)
	runNoticeBackstop(f, backstopRows(t, f, 0, observed), pred, &out)
	own := f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(observed), 1))
	_, other, err := proposePlannerNotice(h, f.row(t), plannerNotice{
		Purpose: "continuation-claim-nudge", Trigger: "wb-1", Causation: "wb-1", Attempt: 1, Content: "x", StartWithin: time.Hour,
	})
	if err != nil {
		t.Fatalf("another backstop's notice: %v", err)
	}

	pred.done = true
	f.clk.Advance(30 * time.Second)
	runNoticeBackstop(f, []beads.Bead{f.row(t)}, pred, &out)
	if got := f.get(t, st, own.ID); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.Reason != "no work is outstanding" || got.Cancel.By != "gc:planner/idle-claim-nudge" {
		t.Fatalf("the backstop's notice = %s %+v, want withdrawn by its producer; journal %q", got.Status, got.Cancel, out.String())
	}
	if got := f.get(t, st, other.ID); got.Status == opjournal.StatusClosed {
		t.Fatalf("another backstop's notice was closed: %+v", got.Cancel)
	}
	if attempts, at := f.meta(t, noticeTestAttemptsKey), f.meta(t, noticeTestAtKey); attempts != "" || at != "" {
		t.Fatalf("pacing = %q, %q; want cleared", attempts, at)
	}

	before := store.opLists.Load()
	runNoticeBackstop(f, []beads.Bead{f.row(t)}, pred, &out)
	if n := store.opLists.Load() - before; n != 0 {
		t.Fatalf("op listings on a tick with nothing to clear = %d, want 0", n)
	}
}

// TestExecutionBackstopMarkStartedWithdrawsItsNotices: once the claimed
// step's start is stamped, the execution backstop's open notices, which
// carry the unstarted claim text, are withdrawn; another backstop's stay.
func TestExecutionBackstopMarkStartedWithdrawsItsNotices(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	var ids []string
	for _, purpose := range []string{executionClaimNudgeLabel, "idle-claim-nudge"} {
		_, op, err := proposePlannerNotice(h, f.row(t), plannerNotice{
			Purpose: purpose, Trigger: "wb-1", Causation: "wb-1", Episode: "1", Attempt: 1, Content: "claim wb-1", StartWithin: time.Hour,
		})
		if err != nil {
			t.Fatalf("proposing %s: %v", purpose, err)
		}
		ids = append(ids, op.ID)
	}
	row := f.row(t)
	var out bytes.Buffer
	p := poolExecutionBackstop{store: f.store, stdout: &out}
	if !p.markStarted(&row, "wb-1") {
		t.Fatal("markStarted = false, want the evidence to hold")
	}
	if got, want := f.meta(t, executionClaimStartedKey), startedValue(row, "wb-1"); got != want {
		t.Fatalf("started marker = %q, want %q", got, want)
	}
	if got := f.get(t, st, ids[0]); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.Reason != "the claimed step started" {
		t.Fatalf("the execution notice = %s %+v, want withdrawn; journal %q", got.Status, got.Cancel, out.String())
	}
	if got := f.get(t, st, ids[1]); got.Status == opjournal.StatusClosed {
		t.Fatalf("another backstop's notice was closed: %+v", got.Cancel)
	}
}

// TestRunNudgeBackstopSpendsAnAttemptWhoseOpAlreadyStood: a crash between
// the op and its reservation; the retry spends the attempt with its own
// line, after the op is no longer due, and writes no second op.
func TestRunNudgeBackstopSpendsAnAttemptWhoseOpAlreadyStood(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	failOnce := 1
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1", failReserve: &failOnce}
	var out bytes.Buffer
	observed := f.clk.Now().Add(-time.Hour)
	runNoticeBackstop(f, backstopRows(t, f, 0, observed), pred, &out)
	if got := f.meta(t, noticeTestAttemptsKey); got != "0" {
		t.Fatalf("reserved = %q after a failed reservation, want 0", got)
	}
	f.clk.Advance(time.Minute)
	out.Reset()
	runNoticeBackstop(f, []beads.Bead{f.row(t)}, pred, &out)
	if got := f.meta(t, noticeTestAttemptsKey); got != "1" {
		t.Fatalf("reserved = %q on the retry, want 1", got)
	}
	if !strings.Contains(out.String(), "attempt 1/3 was already proposed") {
		t.Errorf("journal = %q, want the already-proposed line", out.String())
	}
	if n := len(f.openOps(t)); n != 1 {
		t.Fatalf("open ops = %d, want the one", n)
	}
}

func TestRunNudgeBackstopSpendsARefusedNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: strings.Repeat("x", opjournal.MaxInlinePayload+1)}
	var out bytes.Buffer
	runNoticeBackstop(f, backstopRows(t, f, 0, f.clk.Now().Add(-time.Hour)), pred, &out)
	if got := f.meta(t, noticeTestAttemptsKey); got != "1" {
		t.Fatalf("reserved attempts = %q, want 1: a refused notice spends its attempt", got)
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("a refused notice fell back to %d direct nudges, want 0", n)
	}
	if !strings.Contains(out.String(), "idle-claim-nudge: notice for "+f.name+" for wb-1 refused; attempt 1/3 spent") {
		t.Errorf("journal = %q, want the refusal", out.String())
	}
	if n := len(f.openOps(t)); n != 0 {
		t.Fatalf("open ops = %d, want none", n)
	}
}

// TestRunNudgeBackstopSpendsANoticeThatKeepsFailing: a journal that cannot
// be read logs once, not every tick, and after plannerNoticeMaxFailures
// failures in a row the attempt is spent, so the backstop still reaches its
// terminal action.
func TestRunNudgeBackstopSpendsANoticeThatKeepsFailing(t *testing.T) {
	store := &failingOpListStore{laneTestStore: newLaneTestStore()}
	f := newLaneFixtureOver(t, store)
	newNoticeLaneHost(t, f, "message")
	store.fail.Store(true)
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}
	var out bytes.Buffer
	rows := backstopRows(t, f, 0, f.clk.Now().Add(-time.Hour))
	for tick := 1; tick < plannerNoticeMaxFailures; tick++ {
		runNoticeBackstop(f, rows, pred, &out)
		f.clk.Advance(30 * time.Second)
		if got := f.meta(t, noticeTestAttemptsKey); got != "0" {
			t.Fatalf("tick %d: reserved = %q, want 0 before the limit", tick, got)
		}
	}
	if n := strings.Count(out.String(), "failed ("); n != 1 {
		t.Fatalf("failure lines = %d, want 1 (rate-limited); journal %q", n, out.String())
	}
	runNoticeBackstop(f, rows, pred, &out)
	if got := f.meta(t, noticeTestAttemptsKey); got != "1" {
		t.Fatalf("reserved = %q at the limit, want 1; journal %q", got, out.String())
	}
	if !strings.Contains(out.String(), "failed 3 times in a row; attempt 1/3 spent") {
		t.Errorf("journal = %q, want the spent line", out.String())
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("a failing notice fell back to %d direct nudges, want 0", n)
	}
}

// TestRunNudgeBackstopHoldsTheTerminalActionForAnOpenLastNotice: the last
// attempt's notice has not had its chance yet, so the terminal action waits
// for it to close or expire.
func TestRunNudgeBackstopHoldsTheTerminalActionForAnOpenLastNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message") // no ctx: Poke starts no pass, the op stays open
	calls := 0
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1", exhaustedCalls: &calls}
	var out bytes.Buffer
	runNoticeBackstop(f, backstopRows(t, f, idleClaimNudgeMaxAttempts-1, f.clk.Now().Add(-time.Hour)), pred, &out)
	if got := f.meta(t, noticeTestAttemptsKey); got != strconv.Itoa(idleClaimNudgeMaxAttempts) {
		t.Fatalf("reserved = %q, want the last attempt", got)
	}
	f.clk.Advance(30 * time.Second)
	runNoticeBackstop(f, []beads.Bead{f.row(t)}, pred, &out)
	if calls != 0 {
		t.Fatalf("terminal action ran %d times while the last notice is open and due", calls)
	}
	f.clk.Advance(idleClaimNudgeBackoff)
	runNoticeBackstop(f, []beads.Bead{f.row(t)}, pred, &out)
	if calls != 1 {
		t.Fatalf("terminal action ran %d times after the last notice expired, want 1", calls)
	}
}

// TestRunNudgeBackstopHoldsTheTerminalActionWhileTheLastNoticeIsInFlight:
// the lane is still delivering the last notice a backoff after its
// reservation, past its start_by. The terminal action waits on it, for at
// most the lease the lane took it under.
func TestRunNudgeBackstopHoldsTheTerminalActionWhileTheLastNoticeIsInFlight(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	calls := 0
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1", exhaustedCalls: &calls}
	var out bytes.Buffer
	paced := f.clk.Now().Add(-time.Hour)
	runNoticeBackstop(f, backstopRows(t, f, idleClaimNudgeMaxAttempts-1, paced), pred, &out)
	last := noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(paced), idleClaimNudgeMaxAttempts)
	if _, err := st.Transition(last, opjournal.StatusSubmitted, opjournal.StatusExecuting, laneHostName); err != nil {
		t.Fatalf("starting the last notice: %v", err)
	}
	f.clk.Advance(idleClaimNudgeBackoff)
	runNoticeBackstop(f, []beads.Bead{f.row(t)}, pred, &out)
	if calls != 0 {
		t.Fatalf("terminal action ran %d times while the last notice is in flight; journal %q", calls, out.String())
	}
	f.clk.Advance(opjournal.DefaultLeaseTTL)
	runNoticeBackstop(f, []beads.Bead{f.row(t)}, pred, &out)
	if calls != 1 {
		t.Fatalf("terminal action ran %d times once the lease ran out, want 1", calls)
	}
}

func TestRunNudgeBackstopEmptyContentGoesStraightToTheTerminalActionUnderTheLane(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	calls := 0
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, exhaustedCalls: &calls}
	var out bytes.Buffer
	runNoticeBackstop(f, backstopRows(t, f, 0, f.clk.Now().Add(-time.Hour)), pred, &out)
	if calls != 1 {
		t.Fatalf("terminal action ran %d times with nothing to send, want 1", calls)
	}
	if n := len(f.openOps(t)); n != 0 {
		t.Fatalf("open ops = %d, want none", n)
	}
}

func TestRunNudgeBackstopNudgesDirectlyWithTheLaneOff(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f)
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}
	var out bytes.Buffer
	runNoticeBackstop(f, backstopRows(t, f, 0, f.clk.Now().Add(-time.Hour)), pred, &out)
	nudges := f.nudges()
	if len(nudges) != 1 || nudges[0].Message != "claim wb-1" {
		t.Fatalf("nudges = %+v, want the one direct claim nudge", nudges)
	}
	if n := len(f.openOps(t)); n != 0 {
		t.Fatalf("open ops = %d, want none", n)
	}
	if !strings.Contains(out.String(), "idle-claim-nudge: nudged "+f.name+" for wb-1 (attempt 1/3)") {
		t.Errorf("journal = %q", out.String())
	}
}

// TestRunNudgeBackstopLeavesNoticesDirectUnderTheContentGates pins DES-O0API
// D11: the O0 gates, even at require, do not record planner notices; only
// [ops] authoritative moves them to the lane.
func TestRunNudgeBackstopLeavesNoticesDirectUnderTheContentGates(t *testing.T) {
	f := newLaneFixture(t)
	f.cfg.Ops = config.OpsConfig{ContentMode: "require", ControlMode: "require", LifecycleMode: "require"}
	newNoticeLaneHost(t, f, f.cfg.Ops.Authoritative...)
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}
	var out bytes.Buffer
	runNoticeBackstop(f, backstopRows(t, f, 0, f.clk.Now().Add(-time.Hour)), pred, &out)
	if n := len(f.nudges()); n != 1 {
		t.Fatalf("direct nudges = %d, want 1", n)
	}
	if n := len(f.openOps(t)); n != 0 {
		t.Fatalf("open ops = %d, want none: the gates record no planner notice", n)
	}
}

// TestRunNudgeBackstopNudgesAProtocolModeSessionDirectly: no lane can type
// into a protocol-mode runtime (t3bridge) that no other host serves, so its
// reminder is typed directly as before; a host stamped as serving message
// for the row's generation gets the notice instead.
func TestRunNudgeBackstopNudgesAProtocolModeSessionDirectly(t *testing.T) {
	f := newLaneFixture(t)
	f.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	newNoticeLaneHost(t, f, "message")
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}
	var out bytes.Buffer
	runNoticeBackstop(f, backstopRows(t, f, 0, f.clk.Now().Add(-time.Hour)), pred, &out)
	if nudges := f.nudges(); len(nudges) != 1 || nudges[0].Message != "claim wb-1" {
		t.Fatalf("nudges = %+v, want the one direct claim nudge", nudges)
	}
	if n := len(f.openOps(t)); n != 0 {
		t.Fatalf("open ops = %d, want none", n)
	}

	g := newLaneFixture(t)
	g.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	newNoticeLaneHost(t, g, "message")
	stampHost(t, g, g.laneStore(t), 1, opjournal.KindMessage)
	out.Reset()
	runNoticeBackstop(g, backstopRows(t, g, 0, g.clk.Now().Add(-time.Hour)), pred, &out)
	if n := len(g.nudges()); n != 0 {
		t.Fatalf("hosted elsewhere: direct nudges = %d, want 0", n)
	}
	if n := len(g.openOps(t)); n != 1 {
		t.Fatalf("hosted elsewhere: open ops = %d, want the notice for the other host", n)
	}
}

func TestBackstopNoticeTriggerCarriesTheTargetsProvenance(t *testing.T) {
	if got := backstopNoticeTrigger(backstopTarget{ID: "wb-1"}); got != "wb-1" {
		t.Errorf("bare target = %q, want wb-1", got)
	}
	a := backstopNoticeTrigger(backstopTarget{ID: "wb-1", RootID: "r-1", StoreRef: "rig:a", Generation: "3"})
	b := backstopNoticeTrigger(backstopTarget{ID: "wb-1", RootID: "r-1", StoreRef: "rig:b", Generation: "3"})
	if a == b || !strings.HasPrefix(a, "wb-1") {
		t.Errorf("same id in two stores share a trigger: %q, %q", a, b)
	}
}

// warmBindLaneRow turns f's session row into a pool slot bound to trigger
// and returns it.
func warmBindLaneRow(t *testing.T, f *laneFixture, trigger string) *beads.Bead {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.id, map[string]string{
		"pool_managed": "true", beadmeta.TriggerBeadIDMetadataKey: trigger,
	}); err != nil {
		t.Fatalf("binding the slot: %v", err)
	}
	b := f.row(t)
	return &b
}

func TestDeliverWarmBindClaimNudgeProposesANoticeOp(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	row := warmBindLaneRow(t, f, "w-1")
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, row, warmClaimText, alwaysUnclaimed)
	for _, c := range f.sp.Calls {
		if c.Method == "Nudge" || c.Method == "WaitForIdle" {
			t.Fatalf("the warm bind called %s itself; the lane waits for the boundary", c.Method)
		}
	}
	op := f.get(t, f.laneStore(t), noticeOpID(t, f, warmBindNoticePurpose, "w-1", "", 1))
	if op.Payload != warmClaimText || op.Causation != "w-1" {
		t.Fatalf("op = %+v, want the claim notice for w-1", op)
	}
	if want := laneT0.Add(warmBindNoticeStartWithin); !op.StartBy.Equal(want) || warmBindNoticeStartWithin >= idleClaimNudgeGrace {
		t.Errorf("start_by = %s, want %s, inside the claim backstop's grace", op.StartBy, want)
	}
	if got := f.meta(t, warmBindNudgedForTriggerKey); got != "w-1" {
		t.Fatalf("marker = %q, want w-1", got)
	}
	if got := f.meta(t, warmBindNoticeTriggerKey); got != "w-1" {
		t.Fatalf("notice record = %q, want w-1: the proposed notice is recorded for its withdraw", got)
	}
	// The marker still makes the binding fire once.
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, row, warmClaimText, alwaysUnclaimed)
	if again := f.get(t, f.laneStore(t), op.ID); again.Revision != op.Revision {
		t.Fatalf("op rewritten on a second pass: revision %d -> %d", op.Revision, again.Revision)
	}
}

func TestDeliverWarmBindClaimNudgeSpendsARefusedNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	row := warmBindLaneRow(t, f, "w-1")
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, row, strings.Repeat("x", opjournal.MaxInlinePayload+1), alwaysUnclaimed)
	if got := f.meta(t, warmBindNudgedForTriggerKey); got != "w-1" {
		t.Fatalf("marker = %q after a refused notice, want w-1: retrying it can never succeed", got)
	}
	if got := f.meta(t, warmBindNoticeTriggerKey); got != "" {
		t.Fatalf("notice record = %q after a refused notice, want none: no notice stands", got)
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("a refused notice fell back to %d direct nudges, want 0", n)
	}
}

func TestDeliverWarmBindClaimNudgeRetriesAFailingNoticeThenGivesUp(t *testing.T) {
	store := &failingOpListStore{laneTestStore: newLaneTestStore()}
	f := newLaneFixtureOver(t, store)
	newNoticeLaneHost(t, f, "message")
	store.fail.Store(true)
	row := warmBindLaneRow(t, f, "w-1")
	for tick := 1; tick < plannerNoticeMaxFailures; tick++ {
		deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, row, warmClaimText, alwaysUnclaimed)
		if got := f.meta(t, warmBindNudgedForTriggerKey); got != "" {
			t.Fatalf("tick %d: marker = %q, want unset so a later tick retries", tick, got)
		}
	}
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, row, warmClaimText, alwaysUnclaimed)
	if got := f.meta(t, warmBindNudgedForTriggerKey); got != "w-1" {
		t.Fatalf("marker = %q at the limit, want w-1: the claim backstop owns re-delivery", got)
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("a failing notice fell back to %d direct nudges, want 0", n)
	}
}

func TestDeliverWarmBindClaimNudgeNudgesAProtocolModeSlotDirectly(t *testing.T) {
	f := newLaneFixture(t)
	f.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	newNoticeLaneHost(t, f, "message")
	row := warmBindLaneRow(t, f, "w-2")
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, row, warmClaimText, alwaysUnclaimed)
	if nudges := f.nudges(); len(nudges) != 1 || nudges[0].Message != warmClaimText {
		t.Fatalf("nudges = %+v, want the one direct claim nudge", nudges)
	}
	if n := len(f.openOps(t)); n != 0 {
		t.Fatalf("open ops = %d, want none", n)
	}
	if got := f.meta(t, warmBindNoticeTriggerKey); got != "" {
		t.Fatalf("notice record = %q after a direct nudge, want none", got)
	}
}

// wedgeDrainReminderRow puts f's row in the drain reminder's wedge: pool
// managed, drain-ack stop-pending with a drain to scope a budget to, the
// runtime alive, idle and unacknowledged. It returns the drain's drain_at
// and the row's Info.
func wedgeDrainReminderRow(t *testing.T, f *laneFixture) (string, sessionpkg.Info) {
	t.Helper()
	drainAt := f.clk.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	if err := f.store.SetMetadataBatch(f.id, map[string]string{
		"state": string(sessionpkg.StateDraining), "state_reason": sessionpkg.DrainAckStopPendingReason,
		"drain_at": drainAt, "pool_managed": "true",
	}); err != nil {
		t.Fatalf("wedging the row: %v", err)
	}
	mustSetMeta(t, f.sp, f.name, reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue)
	f.sp.SetActivity(f.name, f.clk.Now().Add(-30*time.Minute))
	_, info := f.view(t)
	return drainAt, info
}

// TestDrainReminderProposesANoticeOpWhenTheLaneExecutesMessages: under
// [ops] authoritative = [message] the drain reminder is a planner notice op
// keyed by its drain, not a direct nudge, and its attempt is spent.
func TestDrainReminderProposesANoticeOpWhenTheLaneExecutesMessages(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	drainAt, info := wedgeDrainReminderRow(t, f)
	var out bytes.Buffer
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderProposed {
		t.Fatalf("outcome = %v, want proposed; journal %q", got, out.String())
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("direct nudges = %d, want 0: the lane delivers the reminder", n)
	}
	op := f.get(t, f.laneStore(t), noticeOpID(t, f, drainReminderNoticePurpose, drainAt, "", 1))
	if op.Payload != drainReminderContent(info) || op.Causation != f.id {
		t.Fatalf("op = %+v, want the drain reminder about %s", op, f.id)
	}
	if m := op.Policy.Message; m.Role != opjournal.RoleNotice || m.When != opjournal.WhenBoundary || m.Wake != opjournal.WakeLiveOnly ||
		op.Producer.Principal != laneDrainNoticeClass.Principal {
		t.Fatalf("op policy %+v from %q, want the drain notice class %+v", m, op.Producer.Principal, laneDrainNoticeClass)
	}
	if want := laneT0.Add(plannerNoticeStartWithin(drainReminderInterval)); !op.StartBy.Equal(want) {
		t.Errorf("start_by = %s, want %s", op.StartBy, want)
	}
	if got := f.meta(t, drainReminderCountKey); got != "1" {
		t.Fatalf("%s = %q, want 1: the proposal spends the attempt", drainReminderCountKey, got)
	}
	if !strings.Contains(out.String(), "drain-reminder: queued drain reminder 1/3 for "+f.name) {
		t.Errorf("journal = %q", out.String())
	}
	// The next attempt, one cadence on, is attempt 2 of the same drain.
	f.clk.Advance(drainReminderInterval)
	_, info = f.view(t)
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderProposed {
		t.Fatalf("second outcome = %v, want proposed; journal %q", got, out.String())
	}
	f.get(t, f.laneStore(t), noticeOpID(t, f, drainReminderNoticePurpose, drainAt, "", 2))
}

// TestDrainReminderNudgesDirectlyWithTheLaneOff: with [ops] authoritative
// empty the drain reminder is today's direct nudge and writes no op.
func TestDrainReminderNudgesDirectlyWithTheLaneOff(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f)
	_, info := wedgeDrainReminderRow(t, f)
	var out bytes.Buffer
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered; journal %q", got, out.String())
	}
	if nudges := f.nudges(); len(nudges) != 1 || nudges[0].Message != drainReminderContent(info) {
		t.Fatalf("nudges = %+v, want the one direct reminder", nudges)
	}
	if n := len(f.openOps(t)); n != 0 {
		t.Fatalf("open ops = %d, want none", n)
	}
}

// TestDrainReminderNudgesAProtocolModeSessionDirectly: no lane can type
// into a protocol-mode runtime no other host serves, so the reminder is
// typed directly as before.
func TestDrainReminderNudgesAProtocolModeSessionDirectly(t *testing.T) {
	f := newLaneFixture(t)
	f.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	newNoticeLaneHost(t, f, "message")
	_, info := wedgeDrainReminderRow(t, f)
	var out bytes.Buffer
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered; journal %q", got, out.String())
	}
	if n := len(f.nudges()); n != 1 {
		t.Fatalf("direct nudges = %d, want 1", n)
	}
	if n := len(f.openOps(t)); n != 0 {
		t.Fatalf("open ops = %d, want none", n)
	}
}

// TestDrainReminderWaitsOnAnInFlightNotice: while an earlier attempt's
// notice is being delivered, the next attempt writes nothing and is not
// spent.
func TestDrainReminderWaitsOnAnInFlightNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	drainAt, info := wedgeDrainReminderRow(t, f)
	var out bytes.Buffer
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderProposed {
		t.Fatalf("outcome = %v, want proposed", got)
	}
	st := f.laneStore(t)
	first := noticeOpID(t, f, drainReminderNoticePurpose, drainAt, "", 1)
	if _, err := st.Transition(first, opjournal.StatusSubmitted, opjournal.StatusExecuting, laneHostName); err != nil {
		t.Fatalf("starting the first notice: %v", err)
	}
	f.clk.Advance(drainReminderInterval)
	_, info = f.view(t)
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderHeld {
		t.Fatalf("outcome = %v, want held behind the in-flight notice; journal %q", got, out.String())
	}
	noticeOpAbsent(t, st, noticeOpID(t, f, drainReminderNoticePurpose, drainAt, "", 2))
	if got := f.meta(t, drainReminderCountKey); got != "1" {
		t.Fatalf("%s = %q, want 1: a held attempt is not spent", drainReminderCountKey, got)
	}
}

// TestDrainReminderRetriesAFailingNoticeThenSpendsItAsUndeliverable: a
// proposal that fails leaves the attempt unspent for a later tick, up to
// plannerNoticeMaxFailures in a row, when the attempt is spent and counted
// as undeliverable, so the budget still reaches its end.
func TestDrainReminderRetriesAFailingNoticeThenSpendsItAsUndeliverable(t *testing.T) {
	store := &failingOpListStore{laneTestStore: newLaneTestStore()}
	f := newLaneFixtureOver(t, store)
	newNoticeLaneHost(t, f, "message")
	_, info := wedgeDrainReminderRow(t, f)
	store.fail.Store(true)
	var out bytes.Buffer
	for tick := 1; tick < plannerNoticeMaxFailures; tick++ {
		if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderSkipped {
			t.Fatalf("tick %d: outcome = %v, want skipped", tick, got)
		}
		if got := f.meta(t, drainReminderCountKey); got != "" {
			t.Fatalf("tick %d: %s = %q, want unset so a later tick retries", tick, drainReminderCountKey, got)
		}
	}
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderSkipped {
		t.Fatalf("at the limit: outcome = %v, want skipped", got)
	}
	if got, failed := f.meta(t, drainReminderCountKey), f.meta(t, drainReminderFailedKey); got != "1" || failed != "1" {
		t.Fatalf("count, failed = %q, %q at the limit, want 1, 1", got, failed)
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("a failing notice fell back to %d direct nudges, want 0", n)
	}
	if !strings.Contains(out.String(), "was undeliverable") {
		t.Errorf("journal = %q, want the undeliverable line", out.String())
	}
}

// TestDrainReminderDeliveredByTheLaneHostToADrainingSession runs the drain
// reminder end to end: the started host delivers its notice into the
// draining session (I7's exemption), as a notice block.
func TestDrainReminderDeliveredByTheLaneHostToADrainingSession(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f, "message")
	hh.start()
	hh.waitTicker(t)
	drainAt, info := wedgeDrainReminderRow(t, f)
	var out bytes.Buffer
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderProposed {
		t.Fatalf("outcome = %v, want proposed; journal %q", got, out.String())
	}
	st := f.laneStore(t)
	id := noticeOpID(t, f, drainReminderNoticePurpose, drainAt, "", 1)
	deadline := time.After(laneWaitBudget)
	for {
		if got := f.get(t, st, id); got.Status == opjournal.StatusClosed {
			if got.Outcome.Arm != opjournal.ArmSucceeded {
				t.Fatalf("outcome = %+v, want succeeded", got.Outcome)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("the host never delivered the drain reminder")
		case <-hh.passes:
		case <-time.After(5 * time.Millisecond):
		}
	}
	nudges := f.nudges()
	text := drainReminderContent(info)
	if len(nudges) != 1 || !strings.Contains(nudges[0].Message, text) || nudges[0].Message == text {
		t.Fatalf("nudges = %+v, want one notice block carrying the reminder", nudges)
	}
	if got := sessionpkg.State(f.meta(t, "state")); got != sessionpkg.StateDraining {
		t.Fatalf("state = %q, want still draining", got)
	}
}

// TestPlannerNoticeDeliveredByTheLaneHost runs a backstop notice end to end:
// the started host delivers it at the boundary as a notice. The notice is
// proposed only after the host's boot scan finished (its scan ticker
// exists), so only the producer's Poke can start the pass that delivers it.
func TestPlannerNoticeDeliveredByTheLaneHost(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f, "message")
	hh.start()
	hh.waitTicker(t) // the boot scan is done; no scan tick is ever sent
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}
	var out bytes.Buffer
	observed := f.clk.Now().Add(-time.Hour)
	runNoticeBackstop(f, backstopRows(t, f, 0, observed), pred, &out)
	st := f.laneStore(t)
	id := noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(observed), 1)
	deadline := time.After(laneWaitBudget)
	for {
		if got := f.get(t, st, id); got.Status == opjournal.StatusClosed {
			if got.Outcome.Arm != opjournal.ArmSucceeded {
				t.Fatalf("outcome = %+v, want succeeded", got.Outcome)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("the host never delivered the notice")
		case <-hh.passes:
		case <-time.After(5 * time.Millisecond):
		}
	}
	nudges := f.nudges()
	if len(nudges) != 1 || !strings.Contains(nudges[0].Message, "claim wb-1") || nudges[0].Message == "claim wb-1" {
		t.Fatalf("nudges = %+v, want one notice block carrying the claim text", nudges)
	}
	for _, ident := range []string{id, f.id, "gc:planner"} {
		if strings.Contains(nudges[0].Message, ident) {
			t.Errorf("delivered text carries identifier %q: %q", ident, nudges[0].Message)
		}
	}
}

// meta reads one key of f's session row.
func (f *laneFixture) meta(t *testing.T, key string) string {
	t.Helper()
	return f.row(t).Metadata[key]
}

// countingOpListStore is a sessions store that counts its op listings (the
// read every planner notice starts with).
type countingOpListStore struct {
	laneTestStore
	opLists atomic.Int64
}

func (s *countingOpListStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Metadata["gc.op.open"] != "" {
		s.opLists.Add(1)
	}
	return s.laneTestStore.List(q)
}

// TestRunNudgeBackstopExhaustedSkipsTheReadOnceTheLastNoticeIsPastDue: one
// backoff after the last reservation the last notice is past its start_by,
// and one lease later past any delivery the lane took it for, so an
// exhausted backstop no longer lists the conversation's ops each tick (the
// continuation backstop stays exhausted indefinitely).
func TestRunNudgeBackstopExhaustedSkipsTheReadOnceTheLastNoticeIsPastDue(t *testing.T) {
	store := &countingOpListStore{laneTestStore: newLaneTestStore()}
	f := newLaneFixtureOver(t, store)
	newNoticeLaneHost(t, f, "message")
	calls := 0
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1", exhaustedCalls: &calls}
	var out bytes.Buffer
	rows := backstopRows(t, f, idleClaimNudgeMaxAttempts, f.clk.Now().Add(-idleClaimNudgeBackoff-opjournal.DefaultLeaseTTL))
	before := store.opLists.Load()
	const ticks = 5
	for i := 0; i < ticks; i++ {
		runNoticeBackstop(f, rows, pred, &out)
		f.clk.Advance(30 * time.Second)
	}
	if n := store.opLists.Load() - before; n != 0 {
		t.Fatalf("op listings over %d exhausted ticks = %d, want 0", ticks, n)
	}
	if calls != ticks {
		t.Fatalf("terminal action ran %d times, want %d", calls, ticks)
	}
}

// TestRunNudgeBackstopFailedHoldReadDoesNotHoldTheTerminalAction: a journal
// that cannot be read while the last notice may still be due logs once and
// lets the terminal action run, so an outage never parks the backstop.
func TestRunNudgeBackstopFailedHoldReadDoesNotHoldTheTerminalAction(t *testing.T) {
	store := &failingOpListStore{laneTestStore: newLaneTestStore()}
	f := newLaneFixtureOver(t, store)
	newNoticeLaneHost(t, f, "message")
	store.fail.Store(true)
	calls := 0
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1", exhaustedCalls: &calls}
	var out bytes.Buffer
	rows := backstopRows(t, f, idleClaimNudgeMaxAttempts, f.clk.Now().Add(-30*time.Second))
	runNoticeBackstop(f, rows, pred, &out)
	runNoticeBackstop(f, rows, pred, &out)
	if calls != 2 {
		t.Fatalf("terminal action ran %d times with the hold read failing, want 2", calls)
	}
	if n := strings.Count(out.String(), "not holding the terminal action"); n != 1 {
		t.Fatalf("failed-read lines = %d, want 1 (rate-limited); journal %q", n, out.String())
	}
}

func TestPlannerNoticeRefusedCoversRequestErrors(t *testing.T) {
	for _, err := range []error{opjournal.ErrInvalidRequest, opjournal.ErrPayloadTooLarge} {
		if !plannerNoticeRefused(fmt.Errorf("proposing: %w", err)) {
			t.Errorf("plannerNoticeRefused(%v) = false, want true", err)
		}
	}
	// A transient may succeed on a retry, and a reused key is the attempt's
	// own op, which proposePlannerNotice answers itself.
	for _, err := range []error{errors.New("store unavailable"), fmt.Errorf("proposing: %w", opjournal.ErrKeyReuse)} {
		if plannerNoticeRefused(err) {
			t.Errorf("plannerNoticeRefused(%v) = true, want false", err)
		}
	}
}

// TestProposePlannerNoticeAnswersAnEditedRetryWithItsOp: an attempt whose
// op, no longer due, was written with text since edited is that op, already
// proposed, not an error (opjournal.KeyReuseError); nothing new is written.
func TestProposePlannerNoticeAnswersAnEditedRetryWithItsOp(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	n := plannerNotice{Purpose: "idle-claim-nudge", Trigger: "wb-1", Causation: "wb-1", Episode: "1", Attempt: 1, Content: "claim wb-1", StartWithin: time.Minute}
	_, first, err := proposePlannerNotice(h, f.row(t), n)
	if err != nil {
		t.Fatalf("proposing: %v", err)
	}
	f.clk.Advance(2 * time.Minute)
	n.Content = "claim wb-1 (edited)"
	outcome, op, err := proposePlannerNotice(h, f.row(t), n)
	if err != nil || outcome != plannerNoticeAlreadyProposed || op.ID != first.ID || op.Payload != "claim wb-1" {
		t.Fatalf("the edited retry = %v, %s %q, %v; want already proposed as %s with its text", outcome, op.ID, op.Payload, err, first.ID)
	}
	if n := len(f.openOps(t)); n != 1 {
		t.Fatalf("open ops = %d, want the one", n)
	}
}

// TestRunNudgeBackstopNoticeCausationIsTheWorkBead: a target with
// provenance keys its notice by the full trigger but names the bare work
// bead as the op's causation.
func TestRunNudgeBackstopNoticeCausationIsTheWorkBead(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	target := backstopTarget{ID: "wb-1", StoreRef: "rig:a", RootID: "r-1"}
	pred := noticeTestBackstop{target: target, text: "claim wb-1"}
	var out bytes.Buffer
	observed := f.clk.Now().Add(-time.Hour)
	runNoticeBackstop(f, backstopRows(t, f, 0, observed), pred, &out)
	op := f.get(t, f.laneStore(t), noticeOpID(t, f, "idle-claim-nudge", backstopNoticeTrigger(target), episode(observed), 1))
	if op.Causation != "wb-1" {
		t.Fatalf("causation = %q, want the work bead wb-1", op.Causation)
	}
}

// TestDeliverWarmBindClaimNudgeNoticesARebindingAfresh: A, then B, then A
// again in one incarnation. The second A binding is another binding, so it
// gets its own notice, as the direct nudge gave it its own nudge.
func TestDeliverWarmBindClaimNudgeNoticesARebindingAfresh(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message") // no pass: past start_by, ops stay open and not due
	logs := captureStdLog(t)
	bind := func(trigger string) {
		deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, warmBindLaneRow(t, f, trigger), warmClaimText, alwaysUnclaimed)
		f.clk.Advance(2 * time.Minute)
	}
	bind("w-A")
	bind("w-B")
	logs.Reset()
	bind("w-A")
	st := f.laneStore(t)
	f.get(t, st, noticeOpID(t, f, warmBindNoticePurpose, "w-A", "", 1))
	again := f.get(t, st, noticeOpID(t, f, warmBindNoticePurpose, "w-A", "2", 1))
	if again.Payload != warmClaimText || again.Causation != "w-A" {
		t.Fatalf("second A notice = %+v", again)
	}
	if !strings.Contains(logs.String(), "queued a claim notice for "+f.name+" to claim w-A") {
		t.Errorf("log = %q, want the queued line", logs.String())
	}
	if n := len(f.openOps(t)); n != 3 {
		t.Fatalf("open ops = %d, want one per binding", n)
	}
}

// TestDeliverWarmBindClaimNudgeRebindingWithinTheWindowSupersedes: A, then
// B, then A again while every earlier notice is still due. Each binding
// supersedes the slot's earlier warm-bind notice that has not started,
// whatever its trigger, so only the newest binding's stands, with a whole
// start window.
func TestDeliverWarmBindClaimNudgeRebindingWithinTheWindowSupersedes(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	logs := captureStdLog(t)
	bind := func(trigger string) {
		f.clk.Advance(10 * time.Second)
		deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, warmBindLaneRow(t, f, trigger), warmClaimText, alwaysUnclaimed)
	}
	bind("w-A")
	bind("w-B")
	logs.Reset()
	bind("w-A")
	st := f.laneStore(t)
	again := f.get(t, st, noticeOpID(t, f, warmBindNoticePurpose, "w-A", "2", 1))
	if again.Status == opjournal.StatusClosed || !again.StartBy.Equal(f.clk.Now().Add(warmBindNoticeStartWithin)) {
		t.Fatalf("the rebinding's notice = %s, start_by %s; want open with a whole window from now", again.Status, again.StartBy)
	}
	bID := noticeOpID(t, f, warmBindNoticePurpose, "w-B", "", 1)
	if got := f.get(t, st, noticeOpID(t, f, warmBindNoticePurpose, "w-A", "", 1)); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.Reason != "superseded by "+bID {
		t.Fatalf("A's first notice = %s %+v, want superseded by B's", got.Status, got.Cancel)
	}
	if got := f.get(t, st, bID); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.Reason != "superseded by "+again.ID {
		t.Fatalf("B's notice = %s %+v, want superseded by the rebinding's", got.Status, got.Cancel)
	}
	if n := len(f.openOps(t)); n != 1 {
		t.Fatalf("open ops = %d, want only the newest binding's notice", n)
	}
	if got := f.meta(t, warmBindNudgedForTriggerKey); got != "w-A" {
		t.Fatalf("marker = %q, want w-A", got)
	}
	if !strings.Contains(logs.String(), "queued a claim notice for "+f.name+" to claim w-A") {
		t.Errorf("log = %q, want the queued line", logs.String())
	}
}

// TestDeliverWarmBindClaimNudgeRebindingBeforeABoundaryDeliversOnlyTheNewest:
// A, then B, both proposed before the slot reaches a boundary. The lane
// delivers the fresh notices it holds in one turn, so only B's claim line
// may reach the slot, not both bindings' at once.
func TestDeliverWarmBindClaimNudgeRebindingBeforeABoundaryDeliversOnlyTheNewest(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message") // no pass: both notices queue
	for _, trigger := range []string{"w-A", "w-B"} {
		deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, warmBindLaneRow(t, f, trigger), "claim "+trigger, alwaysUnclaimed)
		f.clk.Advance(10 * time.Second)
	}
	hh := newLaneHostHarness(t, f, "message")
	hh.start() // its boot scan delivers what is queued
	st := f.laneStore(t)
	bID := noticeOpID(t, f, warmBindNoticePurpose, "w-B", "", 1)
	deadline := time.After(laneWaitBudget)
	for {
		if got := f.get(t, st, bID); got.Status == opjournal.StatusClosed {
			if got.Outcome.Arm != opjournal.ArmSucceeded {
				t.Fatalf("B's notice: outcome = %+v, want succeeded", got.Outcome)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("the host never delivered B's notice")
		case <-hh.passes:
		case <-time.After(5 * time.Millisecond):
		}
	}
	nudges := f.nudges()
	if len(nudges) != 1 || !strings.Contains(nudges[0].Message, "claim w-B") || strings.Contains(nudges[0].Message, "claim w-A") {
		t.Fatalf("nudges = %+v, want one delivery carrying only B's claim line", nudges)
	}
	if got := f.get(t, st, noticeOpID(t, f, warmBindNoticePurpose, "w-A", "", 1)); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.Reason != "superseded by "+bID {
		t.Fatalf("A's notice = %s %+v, want superseded by B's", got.Status, got.Cancel)
	}
}

// TestDeliverWarmBindClaimNudgeRebindingWithEditedTextGetsItsOwnNotice: A,
// B, then A again after the claim text was edited. A's first episode is
// written with the old text (opjournal.KeyReuseError), an earlier binding's
// notice, so the rebinding takes the next episode instead of giving up.
func TestDeliverWarmBindClaimNudgeRebindingWithEditedTextGetsItsOwnNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	logs := captureStdLog(t)
	bind := func(trigger, text string) {
		deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, warmBindLaneRow(t, f, trigger), text, alwaysUnclaimed)
		f.clk.Advance(2 * time.Minute)
	}
	bind("w-A", warmClaimText)
	bind("w-B", warmClaimText)
	logs.Reset()
	edited := warmClaimText + " (edited)"
	bind("w-A", edited)
	if again := f.get(t, f.laneStore(t), noticeOpID(t, f, warmBindNoticePurpose, "w-A", "2", 1)); again.Payload != edited {
		t.Fatalf("the rebinding's notice = %q, want the edited text", again.Payload)
	}
	if got := f.meta(t, warmBindNudgedForTriggerKey); got != "w-A" {
		t.Fatalf("marker = %q, want w-A", got)
	}
	got := logs.String()
	if strings.Contains(got, "gave up") || strings.Contains(got, "refused") || !strings.Contains(got, "queued a claim notice for "+f.name+" to claim w-A") {
		t.Errorf("log = %q, want the queued line and no refusal", got)
	}
}

// TestDeliverWarmBindClaimNudgeRebindingDeliversEveryBinding runs A, B, A
// through a started lane host: three bindings, three deliveries, as with
// the direct nudge.
func TestDeliverWarmBindClaimNudgeRebindingDeliversEveryBinding(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f, "message")
	hh.start()
	hh.waitTicker(t)
	st := f.laneStore(t)
	for i, b := range []struct{ trigger, episode string }{{"w-A", ""}, {"w-B", ""}, {"w-A", "2"}} {
		deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, warmBindLaneRow(t, f, b.trigger), warmClaimText, alwaysUnclaimed)
		id := noticeOpID(t, f, warmBindNoticePurpose, b.trigger, b.episode, 1)
		deadline := time.After(laneWaitBudget)
		for {
			if got := f.get(t, st, id); got.Status == opjournal.StatusClosed {
				if got.Outcome.Arm != opjournal.ArmSucceeded {
					t.Fatalf("binding %d (%s): outcome = %+v, want succeeded", i+1, b.trigger, got.Outcome)
				}
				break
			}
			select {
			case <-deadline:
				t.Fatalf("binding %d (%s): the host never delivered its notice", i+1, b.trigger)
			case <-hh.passes:
			case <-time.After(5 * time.Millisecond):
			}
		}
		f.clk.Advance(2 * time.Minute)
	}
	if n := len(f.nudges()); n != 3 {
		t.Fatalf("deliveries = %d, want 3", n)
	}
}

// TestDeliverWarmBindClaimNudgeRetryFindsItsDueNotice: the marker write was
// lost after the notice was proposed. The retry, while that notice is due,
// writes nothing new and stamps the marker.
func TestDeliverWarmBindClaimNudgeRetryFindsItsDueNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, warmBindLaneRow(t, f, "w-1"), warmClaimText, alwaysUnclaimed)
	if err := f.store.SetMetadataBatch(f.id, map[string]string{warmBindNudgedForTriggerKey: ""}); err != nil {
		t.Fatalf("clearing the marker: %v", err)
	}
	f.clk.Advance(10 * time.Second)
	row := f.row(t)
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, &row, warmClaimText, alwaysUnclaimed)
	if n := len(f.openOps(t)); n != 1 {
		t.Fatalf("open ops = %d, want the one notice", n)
	}
	if got := f.meta(t, warmBindNudgedForTriggerKey); got != "w-1" {
		t.Fatalf("marker = %q, want w-1", got)
	}
}

// TestWarmBindNoticeWaitsOnAnInFlightNotice: while the lane is delivering an
// earlier warm-bind notice, a rebinding of that trigger or a binding of
// another proposes nothing and leaves the marker unset, so a later tick
// proposes the binding's own notice once the delivery has finished.
func TestWarmBindNoticeWaitsOnAnInFlightNotice(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, warmBindLaneRow(t, f, "w-A"), warmClaimText, alwaysUnclaimed)
	first := noticeOpID(t, f, warmBindNoticePurpose, "w-A", "", 1)
	if _, err := st.Transition(first, opjournal.StatusSubmitted, opjournal.StatusExecuting, laneHostName); err != nil {
		t.Fatalf("starting A's notice: %v", err)
	}
	f.clk.Advance(10 * time.Second)
	for _, trigger := range []string{"w-A", "w-B"} {
		if got := warmBindNotice(h, f.row(t), f.name, trigger, warmClaimText, true); got != warmBindNoticeRetry {
			t.Fatalf("binding %s with a notice in flight = %v, want retry", trigger, got)
		}
	}
	noticeOpAbsent(t, st, noticeOpID(t, f, warmBindNoticePurpose, "w-A", "2", 1))
	noticeOpAbsent(t, st, noticeOpID(t, f, warmBindNoticePurpose, "w-B", "", 1))
	if got := f.get(t, st, first); got.Status != opjournal.StatusExecuting || got.Cancel.Recorded() {
		t.Fatalf("A's notice = %s %+v, want executing and not canceled", got.Status, got.Cancel)
	}
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, warmBindLaneRow(t, f, "w-B"), warmClaimText, alwaysUnclaimed)
	if got := f.meta(t, warmBindNudgedForTriggerKey); got != "w-A" {
		t.Fatalf("marker = %q, want w-A until B's notice is proposed", got)
	}

	if _, err := st.Close(first, opjournal.Outcome{Arm: opjournal.ArmSucceeded}, laneHostName); err != nil {
		t.Fatalf("finishing A's notice: %v", err)
	}
	row := f.row(t)
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, &row, warmClaimText, alwaysUnclaimed)
	if got := f.get(t, st, noticeOpID(t, f, warmBindNoticePurpose, "w-B", "", 1)); got.Status == opjournal.StatusClosed {
		t.Fatalf("B's notice = %s %+v, want open", got.Status, got.Cancel)
	}
	if got := f.meta(t, warmBindNudgedForTriggerKey); got != "w-B" {
		t.Fatalf("marker = %q, want w-B", got)
	}
}

// TestPoolClaimBackstopClearWithdrawsTheWarmBindNotice: the claim backstop
// observes the slot's bound trigger, then sees it claimed. Its clear
// withdraws the slot's warm-bind claim notice with its own pacing, so the
// claim line never reaches a slot whose work is taken, and a later tick with
// nothing to clear reads no ops.
func TestPoolClaimBackstopClearWithdrawsTheWarmBindNotice(t *testing.T) {
	store := &countingOpListStore{laneTestStore: newLaneTestStore()}
	f := newLaneFixtureOver(t, store)
	newNoticeLaneHost(t, f, "message")
	f.cfg.Agents = []config.Agent{{Name: "worker", Nudge: warmClaimText}}
	st := f.laneStore(t)
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, warmBindLaneRow(t, f, "w-1"), warmClaimText, alwaysUnclaimed)
	warm := noticeOpID(t, f, warmBindNoticePurpose, "w-1", "", 1)
	var out bytes.Buffer
	tick := func(work beads.Bead) {
		nudgeStalledPoolClaims(f.sp, f.cfg, f.store, []beads.Bead{f.row(t)}, []beads.Bead{work}, nil, f.clk.Now(), poolClaimDrain{}, &out)
	}
	tick(beads.Bead{ID: "w-1", Status: "open"})
	if got := f.meta(t, idleClaimNudgeTriggerKey); got != "w-1" {
		t.Fatalf("claim backstop marker = %q, want w-1; journal %q", got, out.String())
	}

	f.clk.Advance(10 * time.Second)
	tick(beads.Bead{ID: "w-1", Status: "in_progress", Assignee: f.name})
	if got := f.get(t, st, warm); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.By != plannerNoticePrincipalPrefix+warmBindNoticePurpose || got.Cancel.Reason != "the claim backstop found no work outstanding" {
		t.Fatalf("the warm-bind notice = %s %+v, want withdrawn by its producer; journal %q", got.Status, got.Cancel, out.String())
	}
	if got := f.meta(t, idleClaimNudgeTriggerKey); got != "" {
		t.Fatalf("claim backstop marker = %q, want cleared", got)
	}

	before := store.opLists.Load()
	tick(beads.Bead{ID: "w-1", Status: "in_progress", Assignee: f.name})
	if n := store.opLists.Load() - before; n != 0 {
		t.Fatalf("op listings on a tick with nothing to clear = %d, want 0", n)
	}
}

// TestPoolClaimBackstopClearWithdrawsAWarmBindNoticeItNeverPaced: the slot
// claims its bound trigger before the claim backstop first sees it, so the
// backstop writes no marker and has none to wipe. Its clear still withdraws
// the slot's warm-bind claim notice, through the record the warm bind left
// beside its marker, and a later tick reads no ops.
func TestPoolClaimBackstopClearWithdrawsAWarmBindNoticeItNeverPaced(t *testing.T) {
	store := &countingOpListStore{laneTestStore: newLaneTestStore()}
	f := newLaneFixtureOver(t, store)
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, warmBindLaneRow(t, f, "w-1"), defaultPoolClaimNudge, alwaysUnclaimed)
	warm := noticeOpID(t, f, warmBindNoticePurpose, "w-1", "", 1)
	if got := f.get(t, st, warm); got.Status == opjournal.StatusClosed || !strings.Contains(got.Payload, "--drain-ack") {
		t.Fatalf("the warm-bind notice = %s %q, want open with the default claim line", got.Status, got.Payload)
	}
	var out bytes.Buffer
	tick := func() {
		nudgeStalledPoolClaims(f.sp, f.cfg, f.store, []beads.Bead{f.row(t)},
			[]beads.Bead{{ID: "w-1", Status: "in_progress", Assignee: f.name}}, nil, f.clk.Now(), poolClaimDrain{}, &out)
	}

	f.clk.Advance(10 * time.Second)
	tick()
	if got := f.get(t, st, warm); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.By != plannerNoticePrincipalPrefix+warmBindNoticePurpose || got.Cancel.Reason != "the claim backstop found no work outstanding" {
		t.Fatalf("the warm-bind notice = %s %+v, want withdrawn by its producer; journal %q", got.Status, got.Cancel, out.String())
	}
	if got := f.meta(t, idleClaimNudgeTriggerKey); got != "" {
		t.Fatalf("claim backstop marker = %q, want none: the backstop never paced the trigger", got)
	}
	if got := f.meta(t, warmBindNoticeTriggerKey); got != "" {
		t.Fatalf("notice record = %q, want cleared with the withdraw", got)
	}

	before := store.opLists.Load()
	tick()
	if n := store.opLists.Load() - before; n != 0 {
		t.Fatalf("op listings on a tick with nothing to clear = %d, want 0", n)
	}
}

// TestExecutionBackstopMarkStartedWithdrawsTheWarmBindNotice: a slot whose
// claimed step started with no claim backstop clear in between (the claim
// backstop does not run on every path) still drops its warm-bind claim
// notice: the started stamp withdraws it, so the claim line's --drain-ack
// never reaches a started seat (mc-ilr3b).
func TestExecutionBackstopMarkStartedWithdrawsTheWarmBindNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, warmBindLaneRow(t, f, "w-1"), defaultPoolClaimNudge, alwaysUnclaimed)
	warm := noticeOpID(t, f, warmBindNoticePurpose, "w-1", "", 1)
	row := f.row(t)
	var out bytes.Buffer
	p := poolExecutionBackstop{store: f.store, stdout: &out}
	if !p.markStarted(&row, "w-1") {
		t.Fatal("markStarted = false, want the evidence to hold")
	}
	if got := f.get(t, st, warm); got.Outcome.Arm != opjournal.ArmCanceled || got.Cancel.By != plannerNoticePrincipalPrefix+warmBindNoticePurpose || got.Cancel.Reason != "the claimed step started" {
		t.Fatalf("the warm-bind notice = %s %+v, want withdrawn by its producer; journal %q", got.Status, got.Cancel, out.String())
	}
	if got := f.meta(t, warmBindNoticeTriggerKey); got != "" {
		t.Fatalf("notice record = %q, want cleared with the withdraw", got)
	}
}

// TestPlannerNoticeClosesStaleAcrossARespawn: a notice still open when its
// session respawns is scoped to the generation it was proposed for, so the
// lane's next pass closes it as stale instead of typing it into the new
// incarnation.
func TestPlannerNoticeClosesStaleAcrossARespawn(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	_, op, err := proposePlannerNotice(h, f.row(t), plannerNotice{
		Purpose: idleClaimNudgeLabel, Trigger: "wb-1", Causation: "wb-1", Episode: "1", Attempt: 1, Content: "claim wb-1", StartWithin: time.Hour,
	})
	if err != nil {
		t.Fatalf("proposing: %v", err)
	}
	if want := (opjournal.Scope{Kind: opjournal.ScopeSession, Generation: 1}); op.Scope != want {
		t.Fatalf("scope = %+v, want %+v", op.Scope, want)
	}
	f.respawn(t, "tok-2", 2)

	st := f.laneStore(t)
	for range 3 { // as many passes as delivering a notice takes
		if _, err := f.pass(context.Background(), t, f.lane(t, st), st, nil); err != nil {
			t.Fatalf("Pass: %v", err)
		}
	}
	if nudges := f.nudges(); len(nudges) != 0 {
		t.Fatalf("nudges = %+v, want none: the notice was for the previous incarnation", nudges)
	}
	if got := f.get(t, st, op.ID); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSkipped || got.Outcome.Reason != opjournal.ReasonStale {
		t.Fatalf("notice = %s %+v, want closed skipped{stale}", got.Status, got.Outcome)
	}
}

// respawn moves f's row and its runtime to the incarnation token at
// generation gen, as a restart of the session does, idle long enough for the
// quiescence rule.
func (f *laneFixture) respawn(t *testing.T, token string, gen int) {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.id, map[string]string{"instance_token": token, "generation": strconv.Itoa(gen)}); err != nil {
		t.Fatalf("respawning the row: %v", err)
	}
	if err := f.sp.Stop(f.name); err != nil {
		t.Fatalf("stopping the fake runtime: %v", err)
	}
	env := map[string]string{runtime.RuntimeEpochEnv: strconv.Itoa(gen), runtime.ContinuationEpochEnv: "1", runtime.InstanceTokenEnv: token}
	if err := f.sp.Start(context.Background(), f.name, runtime.Config{Env: env}); err != nil {
		t.Fatalf("starting the fake runtime: %v", err)
	}
	f.sp.SetActivity(f.name, time.Now().Add(-time.Minute))
	f.token = token
}

// TestWarmBindNoticeGivesUpPastItsEpisodeBound: a trigger rebound more than
// warmBindNoticeMaxEpisodes times in one incarnation is given up on, and the
// claim backstop owns re-delivery.
func TestWarmBindNoticeGivesUpPastItsEpisodeBound(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	for i := 1; i <= warmBindNoticeMaxEpisodes; i++ {
		if got := warmBindNotice(h, f.row(t), f.name, "w-A", warmClaimText, i > 1); got != warmBindNoticeProposed {
			t.Fatalf("binding %d = %v, want proposed", i, got)
		}
		f.clk.Advance(2 * time.Minute)
	}
	if got := warmBindNotice(h, f.row(t), f.name, "w-A", warmClaimText, true); got != warmBindNoticeSpent {
		t.Fatalf("binding past the bound = %v, want spent", got)
	}
}

// TestWarmReuseRemindsWithTheClaimInstructionNotTheReprimePayload: the start
// config of a resumed or prompt-less launch carries the startup prompt ahead
// of the nudge, to re-prime it. A warm reuse of a running slot reminds it
// with the claim instruction alone, through the lane or directly: its
// template's nudge, or the pool default when the template configures none.
func TestWarmReuseRemindsWithTheClaimInstructionNotTheReprimePayload(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kinds []string
		nudge string // the template's configured nudge
		want  string
	}{
		{"lane", []string{"message"}, warmClaimText, warmClaimText},
		{"direct", nil, warmClaimText, warmClaimText},
		{"lane, no configured nudge", []string{"message"}, "", defaultPoolClaimNudge},
		{"direct, no configured nudge", nil, "", defaultPoolClaimNudge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			f.cfg.Agents = []config.Agent{{Name: "worker", Nudge: tc.nudge}}
			newNoticeLaneHost(t, f, tc.kinds...)
			warmBindLaneRow(t, f, "w-1")
			item := preparedStart{
				candidate: startCandidate{
					info: sessionpkg.Info{ID: f.id, SessionName: f.name, SessionNameMetadata: f.name},
					tp:   TemplateParams{Hints: agent.StartupHints{Nudge: tc.nudge}},
				},
				cfg: runtime.Config{Nudge: restartPromptNudge("Read the startup prompt first.", tc.nudge)},
			}
			started, err := startPreparedStartCandidate(context.Background(), item, f.dir, f.store, f.sp, f.cfg, nil, nil, alwaysUnclaimed)
			if err != nil || started {
				t.Fatalf("warm reuse = %v, %v; want the running slot reused", started, err)
			}
			if tc.kinds == nil {
				if nudges := f.nudges(); len(nudges) != 1 || nudges[0].Message != tc.want {
					t.Fatalf("nudges = %+v, want the one claim instruction %q", nudges, tc.want)
				}
				if n := len(f.openOps(t)); n != 0 {
					t.Fatalf("open ops = %d, want none", n)
				}
			} else {
				if op := f.get(t, f.laneStore(t), noticeOpID(t, f, warmBindNoticePurpose, "w-1", "", 1)); op.Payload != tc.want {
					t.Fatalf("notice = %q, want the claim instruction %q alone", op.Payload, tc.want)
				}
				if n := len(f.nudges()); n != 0 {
					t.Fatalf("direct nudges = %d, want 0", n)
				}
			}
			if got := f.meta(t, warmBindNudgedForTriggerKey); got != "w-1" {
				t.Fatalf("marker = %q, want w-1", got)
			}
		})
	}
}

// markIdleDrainedWedge puts f's row in the wedge an idle drain leaves: the
// idle drain's latch (sleep_intent=idle-stop-pending) under the real
// drain-ack stop-pending mark, which keeps it (DrainAckStopPendingPatch writes
// no sleep_intent), pool managed, the runtime alive, idle and unacknowledged.
func markIdleDrainedWedge(t *testing.T, f *laneFixture) sessionpkg.Info {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.id, map[string]string{"sleep_intent": "idle-stop-pending", "pool_managed": "true"}); err != nil {
		t.Fatalf("latching the idle drain: %v", err)
	}
	if err := f.store.SetMetadataBatch(f.id, sessionpkg.DrainAckStopPendingPatch(f.clk.Now().Add(-3*time.Hour))); err != nil {
		t.Fatalf("marking the drain-ack stop-pending: %v", err)
	}
	mustSetMeta(t, f.sp, f.name, reconcilerDrainAckSourceKey, reconcilerDrainAckSourceValue)
	f.sp.SetActivity(f.name, f.clk.Now().Add(-30*time.Minute))
	_, info := f.view(t)
	return info
}

// TestDrainReminderNudgesAnIdleDrainedWedgeDirectlyWithTheMessageLane: the
// lane's gate holds the drain notice from an idle-drained wedge (the idle
// latch reads stop_pending, which I7's exemption does not lift), so with a
// running lane host for [ops] authoritative = [message] the reminder is the
// direct nudge it is with the lane off, never a notice that waits out its
// start_by undelivered.
func TestDrainReminderNudgesAnIdleDrainedWedgeDirectlyWithTheMessageLane(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f, "message")
	hh.start()
	hh.waitTicker(t)
	info := markIdleDrainedWedge(t, f)
	if got := sessionpkg.ContentGate(info, true, f.clk.Now(), sessionpkg.GateContentDrainNotice); got != sessionpkg.ContentGateStopPending {
		t.Fatalf("drain notice gate = %q, want stop_pending on the real mark", got)
	}
	var out bytes.Buffer
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered; journal %q", got, out.String())
	}
	if nudges := f.nudges(); len(nudges) != 1 || nudges[0].Message != drainReminderContent(info) {
		t.Fatalf("nudges = %+v, want the one direct reminder", nudges)
	}
	if n := len(f.openOps(t)); n != 0 {
		t.Fatalf("open ops = %d, want none", n)
	}
	if got := f.meta(t, drainReminderCountKey); got != "1" {
		t.Fatalf("%s = %q, want 1", drainReminderCountKey, got)
	}
	if !strings.Contains(out.String(), sessionpkg.ContentGateStopPending) {
		t.Errorf("journal = %q, want the gate reason that sent the reminder direct", out.String())
	}
}

// TestDrainAckStopPendingMarkKeepsTheIdleLatch runs the legacy reconciler's
// real drain-ack mark over an idle drain and pins the row it leaves: the idle
// latch survives, so the lane's gate holds the drain notice as stop_pending
// (why deliverDrainReminder asks the gate before it proposes).
func TestDrainAckStopPendingMarkKeepsTheIdleLatch(t *testing.T) {
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{Agents: []config.Agent{{Name: "other"}}}
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{"sleep_intent": "idle-stop-pending"})
	if err := setReconcilerDrainAckMetadata(env.sp, "worker", &drainState{reason: "idle", generation: 1, ackSet: true}); err != nil {
		t.Fatal(err)
	}
	dops := newFakeDrainOps()
	if err := dops.setDrainAck("worker"); err != nil {
		t.Fatal(err)
	}
	beginDrainForTest(t, env.store, env.dt, session.ID, "idle", env.clk.Now().Add(-time.Minute), env.clk.Now().Add(time.Minute)).ackSet = true
	snapshot := mustGetBead(t, env.store, session.ID)
	reconcileSessionBeadsAtPath(context.Background(), env.city, []beads.Bead{snapshot}, nil, nil, env.cfg, env.sp, env.store, dops,
		nil, nil, nil, env.dt, map[string]int{}, false, nil, "", nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...)
	info := env.sessionInfo(session.ID)
	if !isDrainAckStopPendingInfo(info) {
		t.Fatalf("row not marked stop-pending: state=%q reason=%q; stderr %q", info.MetadataState, info.StateReason, env.stderr.String())
	}
	if info.SleepIntent != "idle-stop-pending" {
		t.Fatalf("sleep_intent = %q, want the idle latch kept", info.SleepIntent)
	}
	if got := sessionpkg.ContentGate(info, true, env.clk.Now(), sessionpkg.GateContentDrainNotice); got != sessionpkg.ContentGateStopPending {
		t.Fatalf("drain notice gate = %q, want stop_pending", got)
	}
}

// TestDrainReminderCountsNoticesNeverDeliveredAsUndeliverable: a notice the
// lane never delivered (it stood past its start_by, or was superseded) is
// counted undeliverable when its successor is spent, and the last one when
// the budget runs out, so the escalation's record never claims a
// conversation that did not happen.
func TestDrainReminderCountsNoticesNeverDeliveredAsUndeliverable(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	wedgeDrainReminderRow(t, f)
	var out bytes.Buffer
	for attempt := 1; attempt <= drainReminderMaxAttempts; attempt++ {
		_, info := f.view(t)
		if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderProposed {
			t.Fatalf("attempt %d: outcome = %v, want proposed; journal %q", attempt, got, out.String())
		}
		if got, want := f.meta(t, drainReminderFailedKey), strconv.Itoa(attempt-1); got != want {
			t.Fatalf("attempt %d: %s = %q, want %s", attempt, drainReminderFailedKey, got, want)
		}
		f.clk.Advance(drainReminderInterval)
	}
	_, info := f.view(t)
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderExhausted {
		t.Fatalf("outcome = %v, want exhausted; journal %q", got, out.String())
	}
	row := f.row(t)
	if got := row.Metadata[drainReminderFailedKey]; got != "3" {
		t.Fatalf("%s = %q, want 3", drainReminderFailedKey, got)
	}
	if phrase := drainReminderSpendPhraseFor(row); !strings.Contains(phrase, "3 undeliverable") {
		t.Fatalf("spend phrase = %q, want 3 undeliverable", phrase)
	}
	if !drainRemindersSpent(row, f.clk.Now()) {
		t.Fatal("a settled, spent budget did not authorize the escalation")
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("direct nudges = %d, want 0", n)
	}
}

// TestDrainReminderDoesNotCountDeliveredNotices: notices the lane delivered
// are answered-or-not reminders, never undeliverable ones.
func TestDrainReminderDoesNotCountDeliveredNotices(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	wedgeDrainReminderRow(t, f)
	st := f.laneStore(t)
	var out bytes.Buffer
	for attempt := 1; attempt <= drainReminderMaxAttempts; attempt++ {
		_, info := f.view(t)
		if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderProposed {
			t.Fatalf("attempt %d: outcome = %v, want proposed; journal %q", attempt, got, out.String())
		}
		if _, err := f.pass(context.Background(), t, f.lane(t, st), st, nil); err != nil {
			t.Fatalf("attempt %d: Pass: %v", attempt, err)
		}
		f.clk.Advance(drainReminderInterval)
	}
	if n := len(f.nudges()); n != drainReminderMaxAttempts {
		t.Fatalf("the lane delivered %d notices, want %d", n, drainReminderMaxAttempts)
	}
	_, info := f.view(t)
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderExhausted {
		t.Fatalf("outcome = %v, want exhausted; journal %q", got, out.String())
	}
	row := f.row(t)
	if got := row.Metadata[drainReminderFailedKey]; got != "0" {
		t.Fatalf("%s = %q, want 0", drainReminderFailedKey, got)
	}
	if phrase := drainReminderSpendPhraseFor(row); phrase != "3 unanswered reminders" {
		t.Fatalf("spend phrase = %q", phrase)
	}
}

// TestDrainReminderExhaustionWaitsOnTheLastNotice: while the last attempt's
// notice stands (not yet past its start_by, or in flight) the budget is not
// announced exhausted and the escalation is not authorized; an in-flight one
// holds them for at most one lease past the answer window.
func TestDrainReminderExhaustionWaitsOnTheLastNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	drainAt, _ := wedgeDrainReminderRow(t, f)
	var out bytes.Buffer
	for attempt := 1; attempt <= drainReminderMaxAttempts; attempt++ {
		if attempt > 1 {
			f.clk.Advance(drainReminderInterval)
		}
		_, info := f.view(t)
		if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderProposed {
			t.Fatalf("attempt %d: outcome = %v, want proposed; journal %q", attempt, got, out.String())
		}
	}
	last := f.clk.Now()
	f.clk.Advance(time.Second)
	_, info := f.view(t)
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderHeld {
		t.Fatalf("outcome = %v, want held behind the standing last notice; journal %q", got, out.String())
	}
	if got := f.meta(t, drainReminderHoldKey); got == drainReminderHoldExhausted {
		t.Fatal("exhaustion announced while the last notice stands")
	}
	st := f.laneStore(t)
	third := noticeOpID(t, f, drainReminderNoticePurpose, drainAt, "", drainReminderMaxAttempts)
	if _, err := st.Transition(third, opjournal.StatusSubmitted, opjournal.StatusExecuting, laneHostName); err != nil {
		t.Fatalf("starting the last notice: %v", err)
	}
	f.clk.Advance(last.Add(drainReminderInterval).Sub(f.clk.Now()))
	_, info = f.view(t)
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderHeld {
		t.Fatalf("outcome = %v, want held behind the in-flight last notice; journal %q", got, out.String())
	}
	if drainRemindersSpent(f.row(t), f.clk.Now()) {
		t.Fatal("the escalation was authorized while the last notice is in flight")
	}
	f.clk.Advance(last.Add(drainReminderInterval + opjournal.DefaultLeaseTTL).Sub(f.clk.Now()))
	if !drainRemindersSpent(f.row(t), f.clk.Now()) {
		t.Fatal("an in-flight last notice held the escalation past its lease")
	}
	_, info = f.view(t)
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderExhausted {
		t.Fatalf("outcome = %v, want exhausted past the lease; journal %q", got, out.String())
	}
	if got := f.meta(t, drainReminderFailedKey); got != "3" {
		t.Fatalf("%s = %q, want 3: no notice was delivered", drainReminderFailedKey, got)
	}
}

// TestDrainReminderDirectAttemptSettlesAnUndeliveredLaneNotice: an attempt
// nudged directly because the gate holds the notice settles the lane notice
// before it, counting it undeliverable when the lane never delivered it.
func TestDrainReminderDirectAttemptSettlesAnUndeliveredLaneNotice(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	_, info := wedgeDrainReminderRow(t, f)
	var out bytes.Buffer
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderProposed {
		t.Fatalf("first outcome = %v, want proposed; journal %q", got, out.String())
	}
	if err := f.store.SetMetadataBatch(f.id, map[string]string{"sleep_intent": "idle-stop-pending"}); err != nil {
		t.Fatalf("latching the idle stop: %v", err)
	}
	f.clk.Advance(drainReminderInterval)
	_, info = f.view(t)
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderDelivered {
		t.Fatalf("second outcome = %v, want delivered directly; journal %q", got, out.String())
	}
	row := f.row(t)
	if count, failed, settled := row.Metadata[drainReminderCountKey], row.Metadata[drainReminderFailedKey], row.Metadata[drainReminderSettledKey]; count != "2" || failed != "1" || settled != "2" {
		t.Fatalf("count, failed, settled = %q, %q, %q; want 2, 1, 2", count, failed, settled)
	}
	if !strings.Contains(out.String(), "reminder 1/3 to "+f.name+" was undeliverable: the lane never delivered its notice") {
		t.Errorf("journal = %q", out.String())
	}
}

// TestDrainReminderCountsNoticesTheLaneExpiresUndelivered drives real lane
// passes between attempts over a session that never reaches a boundary: each
// notice closes expired_before_start without a delivery, so by the next
// attempt it is closed and undelivered, and every attempt counts it
// undeliverable. The exhaustion line says so rather than claiming three
// ignored reminders.
func TestDrainReminderCountsNoticesTheLaneExpiresUndelivered(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	wedgeDrainReminderRow(t, f)
	st := f.laneStore(t)
	busy := func(e *conversationLaneExec) opjournal.Executor {
		e.idle = func(nudgeTarget, worker.LiveObservation) bool { return false }
		return e
	}
	var out bytes.Buffer
	for attempt := 1; attempt <= drainReminderMaxAttempts; attempt++ {
		_, info := f.view(t)
		if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderProposed {
			t.Fatalf("attempt %d outcome = %v, want proposed; journal %q", attempt, got, out.String())
		}
		if got, want := f.meta(t, drainReminderFailedKey), strconv.Itoa(attempt-1); got != want {
			t.Fatalf("after attempt %d %s = %q, want %s", attempt, drainReminderFailedKey, got, want)
		}
		if _, err := f.pass(context.Background(), t, f.lane(t, st), st, busy); err != nil {
			t.Fatalf("pass: %v", err)
		}
		f.clk.Advance(drainReminderInterval)
		if _, err := f.pass(context.Background(), t, f.lane(t, st), st, busy); err != nil {
			t.Fatalf("pass: %v", err)
		}
		if n := len(f.openOps(t)); n != 0 {
			t.Fatalf("after attempt %d open ops = %d, want the notice closed expired", attempt, n)
		}
	}
	_, info := f.view(t)
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderExhausted {
		t.Fatalf("outcome = %v, want exhausted; journal %q", got, out.String())
	}
	row := f.row(t)
	if failed := row.Metadata[drainReminderFailedKey]; failed != "3" {
		t.Fatalf("%s = %q, want 3", drainReminderFailedKey, failed)
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("direct nudges = %d, want 0", n)
	}
	phrase := drainReminderSpendPhrase(drainReminderMaxAttempts, drainReminderMaxAttempts)
	if drainReminderSpendPhraseFor(row) != phrase || !strings.Contains(out.String(), "exhausted for "+f.name+" after "+phrase) {
		t.Errorf("phrase %q; journal %q; want %q", drainReminderSpendPhraseFor(row), out.String(), phrase)
	}
	if strings.Contains(out.String(), "unanswered") {
		t.Errorf("journal claims unanswered reminders: %q", out.String())
	}
}

// TestDrainReminderNudgesARestartPendingWedgeDirectlyWithTheMessageLane: the
// direct fallback is any gate hold other than draining, not just the idle
// latch's stop_pending; a draining wedge with a recorded restart reads
// restart_pending and is nudged directly with no op.
func TestDrainReminderNudgesARestartPendingWedgeDirectlyWithTheMessageLane(t *testing.T) {
	for _, key := range []string{"restart_requested", "continuation_reset_pending"} {
		t.Run(key, func(t *testing.T) {
			f := newLaneFixture(t)
			newNoticeLaneHost(t, f, "message")
			wedgeDrainReminderRow(t, f)
			if err := f.store.SetMetadataBatch(f.id, map[string]string{key: "true"}); err != nil {
				t.Fatal(err)
			}
			_, info := f.view(t)
			if got := sessionpkg.ContentGate(info, true, f.clk.Now(), sessionpkg.GateContentDrainNotice); got != sessionpkg.ContentGateRestartPending {
				t.Fatalf("gate = %q, want restart_pending", got)
			}
			var out bytes.Buffer
			if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderDelivered {
				t.Fatalf("outcome = %v, want delivered; journal %q", got, out.String())
			}
			if nudges := f.nudges(); len(nudges) != 1 || nudges[0].Message != drainReminderContent(info) {
				t.Fatalf("nudges = %+v, want the one direct reminder", nudges)
			}
			if n := len(f.openOps(t)); n != 0 {
				t.Fatalf("open ops = %d, want none", n)
			}
		})
	}
}

// TestDrainReminderDirectMarkerOmitsTheSettledKey: with the lane off the
// pacing marker is the one written before the lane existed; an absent settled
// count reads as every attempt settled, so the direct path adds none.
func TestDrainReminderDirectMarkerOmitsTheSettledKey(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f)
	_, info := wedgeDrainReminderRow(t, f)
	var out bytes.Buffer
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered; journal %q", got, out.String())
	}
	row := f.row(t)
	if v, ok := row.Metadata[drainReminderSettledKey]; ok {
		t.Fatalf("%s = %q written on the direct path, want absent", drainReminderSettledKey, v)
	}
	if attempts, _, settled, _ := drainReminderState(row, drainReminderIdentity(row)); attempts != 1 || settled != 1 {
		t.Fatalf("attempts, settled = %d, %d; want 1, 1", attempts, settled)
	}
}

// TestDrainReminderStateReadsAnAbsentSettledCountAsAllSettled: a marker
// written before the settled count existed settles every attempt.
func TestDrainReminderStateReadsAnAbsentSettledCountAsAllSettled(t *testing.T) {
	last := laneT0
	row := beads.Bead{Metadata: map[string]string{
		"instance_token": "tok", "drain_at": "at",
		drainReminderDrainKey: "tok/at", drainReminderCountKey: "3", drainReminderFailedKey: "1",
		drainReminderAtKey: last.UTC().Format(time.RFC3339),
	}}
	if attempts, failed, settled, _ := drainReminderState(row, "tok/at"); attempts != 3 || failed != 1 || settled != 3 {
		t.Fatalf("attempts, failed, settled = %d, %d, %d; want 3, 1, 3", attempts, failed, settled)
	}
	if drainRemindersSpent(row, last.Add(drainReminderInterval-time.Second)) || !drainRemindersSpent(row, last.Add(drainReminderInterval)) {
		t.Fatal("a fully settled legacy budget does not keep the ordinary answer window")
	}
}
