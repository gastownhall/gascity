package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
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

func TestProposePlannerNoticeWaitsForAnOpenNotice(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	n := plannerNotice{Purpose: "idle-claim-nudge", Trigger: "wb-1", Causation: "wb-1", Episode: "1", Attempt: 1, Content: "claim it", StartWithin: time.Hour}
	if _, _, err := proposePlannerNotice(h, f.row(t), n); err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	n.Attempt, n.Episode = 2, "2"
	outcome, prev, err := proposePlannerNotice(h, f.row(t), n)
	if err != nil || outcome != plannerNoticePending {
		t.Fatalf("attempt 2 while 1 is open = %v, %v; want pending", outcome, err)
	}
	if prev.ID != noticeOpID(t, f, "idle-claim-nudge", "wb-1", "1", 1) {
		t.Fatalf("pending names %s, want attempt 1", prev.ID)
	}
	noticeOpAbsent(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", "2", 2))

	// Another reminder's notice, or another trigger's, does not hold it.
	other := plannerNotice{Purpose: "continuation-claim-nudge", Trigger: "wb-1", Causation: "wb-1", Attempt: 1, Content: "x", StartWithin: time.Hour}
	if outcome, _, err := proposePlannerNotice(h, f.row(t), other); err != nil || outcome != plannerNoticeProposed {
		t.Fatalf("another purpose = %v, %v; want proposed", outcome, err)
	}
	other = plannerNotice{Purpose: "idle-claim-nudge", Trigger: "wb-2", Causation: "wb-2", Attempt: 1, Content: "x", StartWithin: time.Hour}
	if outcome, _, err := proposePlannerNotice(h, f.row(t), other); err != nil || outcome != plannerNoticeProposed {
		t.Fatalf("another trigger = %v, %v; want proposed", outcome, err)
	}

	// Past attempt 1's start_by it never started: the lane closes it as
	// expired, and attempt 2 replaces it.
	f.clk.Advance(time.Hour)
	if outcome, _, err := proposePlannerNotice(h, f.row(t), n); err != nil || outcome != plannerNoticeProposed {
		t.Fatalf("attempt 2 after 1 expired = %v, %v; want proposed", outcome, err)
	}
	f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", "2", 2))
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

// noticeTestBackstop is a backstop predicate that always has one target
// outstanding and paces through two metadata keys on the session row.
// exhaustedCalls, when set, counts its terminal actions; failReserve, when
// set, fails that many reservations first.
type noticeTestBackstop struct {
	target         backstopTarget
	text           string
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
	return p.target, backstopResolutionOutstanding
}

func (noticeTestBackstop) state(s beads.Bead, _ backstopTarget) (bool, int, time.Time) {
	at, _ := time.Parse(time.RFC3339, s.Metadata[noticeTestAtKey])
	return true, atoiOr0(s.Metadata[noticeTestAttemptsKey]), at
}
func (p noticeTestBackstop) content(beads.Bead) string { return p.text }
func (noticeTestBackstop) revalidate(backstopTarget) backstopResolution {
	return backstopResolutionOutstanding
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
func (noticeTestBackstop) clear(beads.Store, *beads.Bead, io.Writer) {}

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

	// The next attempt, while attempt 1 is still due, writes nothing.
	f.clk.Advance(time.Minute)
	reserved := f.clk.Now().Add(-idleClaimNudgeBackoff)
	runNoticeBackstop(f, backstopRows(t, f, 1, reserved), pred, &out)
	noticeOpAbsent(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(reserved), 2))
	if got := f.meta(t, noticeTestAttemptsKey); got != "1" {
		t.Fatalf("reserved attempts = %q, want still 1 while attempt 1 is pending", got)
	}

	// Once attempt 1 is past its start_by, attempt 2 goes out.
	f.clk.Advance(idleClaimNudgeBackoff)
	reserved = f.clk.Now().Add(-idleClaimNudgeBackoff)
	runNoticeBackstop(f, backstopRows(t, f, 1, reserved), pred, &out)
	f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", episode(reserved), 2))
	if got := f.meta(t, noticeTestAttemptsKey); got != "2" {
		t.Fatalf("reserved attempts = %q, want 2", got)
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
}

// TestDrainReminderStaysDirectWithTheMessageLane pins that the drain
// reminder keeps its direct delivery under [ops] authoritative = [message]:
// the lane's content gate holds content for a draining row, which is the
// only row this reminder is for.
func TestDrainReminderStaysDirectWithTheMessageLane(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
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
	if got := sessionpkg.ContentGate(info, true, f.clk.Now()); got != sessionpkg.ContentGateDraining {
		t.Fatalf("content gate = %q, want draining: the reason the reminder stays direct", got)
	}
	var out bytes.Buffer
	if got := maybeRemindDrainingSession(f.sp, f.store, info, f.clk, &out); got != drainReminderDelivered {
		t.Fatalf("outcome = %v, want delivered; journal %q", got, out.String())
	}
	if n := len(f.nudges()); n != 1 {
		t.Fatalf("direct nudges = %d, want 1", n)
	}
	convs, _, err := f.laneStore(t).OpenLaneConversations(10)
	if err != nil || len(convs) != 0 {
		t.Fatalf("open lane conversations = %v, %v; want none", convs, err)
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
