package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/opjournal"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// newNoticeLaneHost registers a running lane host over f's sessions store
// with kinds latched and no context, so Poke starts no pass: the op rows a
// producer writes stay as it wrote them.
func newNoticeLaneHost(t *testing.T, f *laneFixture, kinds ...string) *conversationLaneHost {
	t.Helper()
	h := newConversationLaneHost(&laneHostEnvFake{f: f, store: f.store}, kinds)
	h.clk = f.clk
	h.state = laneHostStateRunning
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	registerLaneHost(ctx, f.dir, h)
	return h
}

// noticeOpID is the pinned id of a planner notice's attempt.
func noticeOpID(t *testing.T, f *laneFixture, purpose, trigger string, attempt int) string {
	t.Helper()
	id, err := opjournal.DeriveID("gc", f.id, plannerNoticePrincipalPrefix+purpose,
		purpose+":"+f.token+":"+trigger+":"+strconv.Itoa(attempt))
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
}

func TestProposePlannerNoticeWritesANoticeOp(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	n := plannerNotice{Purpose: "idle-claim-nudge", Trigger: "wb-1", Attempt: 1, Content: "claim it", StartWithin: 3 * time.Minute}
	outcome, op, err := proposePlannerNotice(h, f.id, f.token, n)
	if err != nil || outcome != plannerNoticeProposed {
		t.Fatalf("propose = %v, %v; want proposed", outcome, err)
	}
	if want := noticeOpID(t, f, "idle-claim-nudge", "wb-1", 1); op.ID != want {
		t.Fatalf("op id = %s, want %s", op.ID, want)
	}
	got := f.get(t, f.laneStore(t), op.ID)
	m := got.Policy.Message
	switch {
	case got.Kind != opjournal.KindMessage || got.Executor != opjournal.ExecutorLane:
		t.Errorf("kind/executor = %s/%s, want message/lane", got.Kind, got.Executor)
	case m.Role != opjournal.RoleNotice || m.When != opjournal.WhenBoundary || m.Wake != opjournal.WakeLiveOnly:
		t.Errorf("policy = %+v, want notice, boundary, live_only", m)
	case got.Producer.Principal != "gc:planner/idle-claim-nudge":
		t.Errorf("principal = %q", got.Producer.Principal)
	case got.ClientKey != "idle-claim-nudge:tok-1:wb-1:1":
		t.Errorf("client key = %q", got.ClientKey)
	case got.Causation != "wb-1" || got.Payload != "claim it":
		t.Errorf("causation/payload = %q/%q", got.Causation, got.Payload)
	case !got.StartBy.Equal(laneT0.Add(3 * time.Minute)):
		t.Errorf("start_by = %s, want the reminder's cadence after %s", got.StartBy, laneT0)
	}

	// The same attempt again is the same op: nothing new is written.
	again, op2, err := proposePlannerNotice(h, f.id, f.token, n)
	if err != nil || again != plannerNoticeProposed || op2.ID != op.ID || op2.Revision != got.Revision {
		t.Fatalf("re-propose = %v %s rev %d, %v; want the stored op unchanged", again, op2.ID, op2.Revision, err)
	}
}

func TestProposePlannerNoticeWaitsForAnOpenAttempt(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	n := plannerNotice{Purpose: "idle-claim-nudge", Trigger: "wb-1", Attempt: 1, Content: "claim it", StartWithin: time.Hour}
	if _, _, err := proposePlannerNotice(h, f.id, f.token, n); err != nil {
		t.Fatalf("attempt 1: %v", err)
	}
	n.Attempt = 2
	outcome, prev, err := proposePlannerNotice(h, f.id, f.token, n)
	if err != nil || outcome != plannerNoticePending {
		t.Fatalf("attempt 2 while 1 is open = %v, %v; want pending", outcome, err)
	}
	if prev.ID != noticeOpID(t, f, "idle-claim-nudge", "wb-1", 1) {
		t.Fatalf("pending names %s, want attempt 1", prev.ID)
	}
	noticeOpAbsent(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", 2))

	// Past attempt 1's start_by it never started: the lane closes it as
	// expired, and attempt 2 replaces it.
	f.clk.Advance(time.Hour)
	if outcome, _, err := proposePlannerNotice(h, f.id, f.token, n); err != nil || outcome != plannerNoticeProposed {
		t.Fatalf("attempt 2 after 1 expired = %v, %v; want proposed", outcome, err)
	}
	f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", 2))

	// A previous attempt that was never written does not hold the next one.
	n.Attempt = 5
	if outcome, _, err := proposePlannerNotice(h, f.id, f.token, n); err != nil || outcome != plannerNoticeProposed {
		t.Fatalf("attempt 5 with no attempt 4 = %v, %v; want proposed", outcome, err)
	}
}

func TestProposePlannerNoticeRefusesBadInput(t *testing.T) {
	f := newLaneFixture(t)
	h := newNoticeLaneHost(t, f, "message")
	if _, _, err := proposePlannerNotice(nil, f.id, f.token, plannerNotice{Purpose: "p", Trigger: "t", Attempt: 1, Content: "x"}); err == nil {
		t.Error("nil lane: want an error")
	}
	if _, _, err := proposePlannerNotice(h, f.id, f.token, plannerNotice{Purpose: "p", Trigger: "t", Attempt: 0, Content: "x"}); err == nil {
		t.Error("attempt 0: want an error")
	}
	if _, _, err := proposePlannerNotice(h, f.id, f.token, plannerNotice{Purpose: "p", Trigger: "t", Attempt: 1}); err == nil {
		t.Error("empty content: want an error")
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
type noticeTestBackstop struct {
	target backstopTarget
	text   string
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
func (noticeTestBackstop) reserve(store beads.Store, s *beads.Bead, _ backstopTarget, attempts int, now time.Time, _ io.Writer) bool {
	kv := map[string]string{noticeTestAttemptsKey: strconv.Itoa(attempts), noticeTestAtKey: now.UTC().Format(time.RFC3339)}
	if err := store.SetMetadataBatch(s.ID, kv); err != nil {
		return false
	}
	for k, v := range kv {
		s.Metadata[k] = v
	}
	return true
}
func (noticeTestBackstop) exhausted(beads.Store, *beads.Bead, io.Writer) {}
func (noticeTestBackstop) clear(beads.Store, *beads.Bead, io.Writer)     {}

// backstopRows reads f's session row, its pacing set as attempts reserved
// at last.
func backstopRows(t *testing.T, f *laneFixture, attempts int, last time.Time) []beads.Bead {
	t.Helper()
	if err := f.store.SetMetadataBatch(f.id, map[string]string{
		noticeTestAttemptsKey: strconv.Itoa(attempts), noticeTestAtKey: last.UTC().Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("seeding the pacing: %v", err)
	}
	b, err := f.store.Get(f.id)
	if err != nil {
		t.Fatalf("reading the session row: %v", err)
	}
	return []beads.Bead{b}
}

// runNoticeBackstop runs the backstop over rows with pred's target as the
// tick's work.
func runNoticeBackstop(f *laneFixture, rows []beads.Bead, pred noticeTestBackstop, out *bytes.Buffer) {
	work := []beads.Bead{{ID: pred.target.ID, Status: "open"}}
	runNudgeBackstop(f.sp, f.store, rows, work, f.clk.Now(), out, "idle-claim-nudge", pred)
}

func TestRunNudgeBackstopProposesNoticeOpsWhenTheLaneExecutesMessages(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	st := f.laneStore(t)
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}
	var out bytes.Buffer

	runNoticeBackstop(f, backstopRows(t, f, 0, f.clk.Now().Add(-time.Hour)), pred, &out)
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("the backstop typed %d nudges itself, want 0", n)
	}
	op := f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", 1))
	if op.Payload != "claim wb-1" || op.Policy.Message.Role != opjournal.RoleNotice {
		t.Fatalf("op = %+v, want the claim notice", op)
	}
	if !op.StartBy.Equal(f.clk.Now().Add(idleClaimNudgeBackoff)) {
		t.Errorf("start_by = %s, want one backoff after now", op.StartBy)
	}
	if got := f.meta(t, noticeTestAttemptsKey); got != "1" {
		t.Fatalf("reserved attempts = %q, want 1", got)
	}
	if !strings.Contains(out.String(), "idle-claim-nudge: queued notice for "+f.name+" for wb-1 (attempt 1/3)") {
		t.Errorf("journal = %q", out.String())
	}

	// The next attempt, while attempt 1 is still due, writes nothing.
	f.clk.Advance(idleClaimNudgeBackoff - time.Second)
	runNoticeBackstop(f, backstopRows(t, f, 1, f.clk.Now().Add(-idleClaimNudgeBackoff)), pred, &out)
	noticeOpAbsent(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", 2))
	if got := f.meta(t, noticeTestAttemptsKey); got != "1" {
		t.Fatalf("reserved attempts = %q, want still 1 while attempt 1 is pending", got)
	}

	// Once attempt 1 is past its start_by, attempt 2 goes out.
	f.clk.Advance(time.Second)
	runNoticeBackstop(f, backstopRows(t, f, 1, f.clk.Now().Add(-idleClaimNudgeBackoff)), pred, &out)
	f.get(t, st, noticeOpID(t, f, "idle-claim-nudge", "wb-1", 2))
	if got := f.meta(t, noticeTestAttemptsKey); got != "2" {
		t.Fatalf("reserved attempts = %q, want 2", got)
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("the backstop typed %d nudges itself, want 0", n)
	}
}

func TestRunNudgeBackstopReservesNothingWhenTheNoticeFails(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: strings.Repeat("x", opjournal.MaxInlinePayload+1)}
	var out bytes.Buffer
	runNoticeBackstop(f, backstopRows(t, f, 0, f.clk.Now().Add(-time.Hour)), pred, &out)
	if got := f.meta(t, noticeTestAttemptsKey); got != "0" {
		t.Fatalf("reserved attempts = %q, want 0 after a refused notice", got)
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("a refused notice fell back to %d direct nudges, want 0", n)
	}
	if !strings.Contains(out.String(), "idle-claim-nudge: notice for "+f.name+" failed") {
		t.Errorf("journal = %q, want the failure", out.String())
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
	noticeOpAbsent(t, f.laneStore(t), noticeOpID(t, f, "idle-claim-nudge", "wb-1", 1))
	if !strings.Contains(out.String(), "idle-claim-nudge: nudged "+f.name+" for wb-1 (attempt 1/3)") {
		t.Errorf("journal = %q", out.String())
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
	b, err := f.store.Get(f.id)
	if err != nil {
		t.Fatalf("reading the session row: %v", err)
	}
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
	op := f.get(t, f.laneStore(t), noticeOpID(t, f, warmBindNoticePurpose, "w-1", 1))
	if op.Payload != warmClaimText || op.Causation != "w-1" {
		t.Fatalf("op = %+v, want the claim notice for w-1", op)
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

func TestDeliverWarmBindClaimNudgeLeavesTheMarkerWhenTheNoticeFails(t *testing.T) {
	f := newLaneFixture(t)
	newNoticeLaneHost(t, f, "message")
	row := warmBindLaneRow(t, f, "w-1")
	deliverWarmBindClaimNudge(context.Background(), f.sp, f.store, row, strings.Repeat("x", opjournal.MaxInlinePayload+1), alwaysUnclaimed)
	if got := f.meta(t, warmBindNudgedForTriggerKey); got != "" {
		t.Fatalf("marker = %q after a refused notice, want unset so a later tick retries", got)
	}
	if n := len(f.nudges()); n != 0 {
		t.Fatalf("a refused notice fell back to %d direct nudges, want 0", n)
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
// the started host delivers it at the boundary as a notice.
func TestPlannerNoticeDeliveredByTheLaneHost(t *testing.T) {
	f := newLaneFixture(t)
	hh := newLaneHostHarness(t, f, "message")
	hh.start()
	pred := noticeTestBackstop{target: backstopTarget{ID: "wb-1"}, text: "claim wb-1"}
	var out bytes.Buffer
	runNoticeBackstop(f, backstopRows(t, f, 0, f.clk.Now().Add(-time.Hour)), pred, &out)
	st := f.laneStore(t)
	id := noticeOpID(t, f, "idle-claim-nudge", "wb-1", 1)
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
		case p := <-hh.passes:
			_ = p
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
	b, err := f.store.Get(f.id)
	if err != nil {
		t.Fatalf("reading the session row: %v", err)
	}
	return b.Metadata[key]
}
