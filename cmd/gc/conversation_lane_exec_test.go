package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/tmux"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

var laneT0 = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

// laneTestClock is a fake clock safe to read from worker goroutines.
type laneTestClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *laneTestClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *laneTestClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// laneTestStore is a sessions store with the id prefix a production store
// reports, so the op store discovers its namespace.
type laneTestStore struct {
	*beads.MemStore
}

func (laneTestStore) IDPrefix() string { return "gc" }

func newLaneTestStore() laneTestStore {
	return laneTestStore{&beads.MemStore{IDPrefix: "gc", HonorExplicitIDs: true}}
}

// laneFixture is one session row over a fake runtime, the stores a lane
// needs, and a fake clock.
type laneFixture struct {
	dir     string
	cfg     *config.City
	store   beads.Store
	sp      *runtime.Fake
	clk     *laneTestClock
	id      string
	name    string
	token   string
	started bool
}

// laneFixtureOpt adjusts a fixture's session row metadata before it is
// created.
type laneFixtureOpt func(meta map[string]string)

func noLaneToken(meta map[string]string) { delete(meta, "instance_token") }

func laneTransport(transport string) laneFixtureOpt {
	return func(meta map[string]string) { meta["transport"] = transport }
}

func newLaneFixture(t *testing.T, opts ...laneFixtureOpt) *laneFixture {
	t.Helper()
	return newLaneFixtureOver(t, newLaneTestStore(), opts...)
}

func newLaneFixtureOver(t *testing.T, store beads.Store, opts ...laneFixtureOpt) *laneFixture {
	t.Helper()
	f := &laneFixture{
		dir:   t.TempDir(),
		cfg:   &config.City{},
		store: store,
		sp:    runtime.NewFake(),
		clk:   &laneTestClock{now: laneT0},
		name:  "worker-session",
		token: "tok-1",
	}
	meta := map[string]string{
		"session_name":       f.name,
		"agent_name":         "worker",
		"template":           "worker",
		"state":              string(session.StateActive),
		"generation":         "1",
		"continuation_epoch": "1",
		"instance_token":     f.token,
	}
	for _, o := range opts {
		o(meta)
	}
	b, err := store.Create(beads.Bead{
		Title: "Session: worker", Type: session.BeadType, Status: "open",
		Labels: []string{session.LabelSession}, Metadata: meta,
	})
	if err != nil {
		t.Fatalf("creating the session row: %v", err)
	}
	f.id = b.ID
	f.token = meta["instance_token"]
	f.startRuntime(t, f.token)
	return f
}

// startRuntime starts the fake runtime as the incarnation token names, idle
// long enough for the quiescence rule.
func (f *laneFixture) startRuntime(t *testing.T, token string) {
	t.Helper()
	if f.started {
		if err := f.sp.Stop(f.name); err != nil {
			t.Fatalf("stopping the fake runtime: %v", err)
		}
	}
	env := map[string]string{runtime.RuntimeEpochEnv: "1", runtime.ContinuationEpochEnv: "1"}
	if token != "" {
		env[runtime.InstanceTokenEnv] = token
	}
	if err := f.sp.Start(context.Background(), f.name, runtime.Config{Env: env}); err != nil {
		t.Fatalf("starting the fake runtime: %v", err)
	}
	f.started = true
	f.sp.SetActivity(f.name, time.Now().Add(-time.Minute))
}

// laneStore opens the op store as the lane host does: live reads over the
// sessions store.
func (f *laneFixture) laneStore(t *testing.T) *opjournal.Store {
	t.Helper()
	st, err := opjournal.NewLaneStore(beads.SessionStore{Store: laneLiveSessions{Store: f.store}}, f.clk)
	if err != nil {
		t.Fatalf("NewLaneStore: %v", err)
	}
	return st
}

func (f *laneFixture) lane(t *testing.T, st *opjournal.Store) *opjournal.ConversationLane {
	t.Helper()
	l, err := opjournal.NewLane(st, laneHostName, f.clk)
	if err != nil {
		t.Fatalf("NewLane: %v", err)
	}
	return l
}

// msg is a lane message to the fixture's conversation.
func (f *laneFixture) msg(key string, role opjournal.Role, payload string, mods ...func(*opjournal.Request)) opjournal.Request {
	r := opjournal.Request{
		Conversation: f.id, Kind: opjournal.KindMessage, Executor: opjournal.ExecutorLane, ClientKey: key,
		Producer: opjournal.Producer{Principal: "gc:mail", Actor: opjournal.ActorBackground}, Payload: payload,
		Policy: opjournal.Policy{Message: opjournal.MessagePolicy{Role: role}},
	}
	for _, m := range mods {
		m(&r)
	}
	return r
}

func (f *laneFixture) propose(t *testing.T, st *opjournal.Store, req opjournal.Request) opjournal.Op {
	t.Helper()
	op, created, err := st.Propose(req)
	if err != nil || !created {
		t.Fatalf("Propose(%s) = created %v, %v", req.ClientKey, created, err)
	}
	return op
}

func (f *laneFixture) get(t *testing.T, st *opjournal.Store, id string) opjournal.Op {
	t.Helper()
	op, err := st.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return op
}

// deps are the executor's dependencies over the fixture.
func (f *laneFixture) deps(st *opjournal.Store) laneExecDeps {
	return laneExecDeps{cityPath: f.dir, cfg: f.cfg, sessions: laneLiveSessions{Store: f.store}, sp: f.sp, lane: st, clk: f.clk}
}

// view reads the fixture's conversation as a pass does.
func (f *laneFixture) view(t *testing.T) (opjournal.ConversationView, session.Info) {
	t.Helper()
	v, info, _, err := readConversationLaneView(sessionFrontDoor(laneLiveSessions{Store: f.store}), f.id)
	if err != nil {
		t.Fatalf("reading the view: %v", err)
	}
	return v, info
}

// pass runs one pass with the live executor; wrap may replace it.
func (f *laneFixture) pass(ctx context.Context, t *testing.T, l *opjournal.ConversationLane, st *opjournal.Store, wrap func(*conversationLaneExec) opjournal.Executor) (opjournal.PassResult, error) {
	t.Helper()
	v, info := f.view(t)
	e := newConversationLaneExec(f.deps(st), info)
	var exec opjournal.Executor = e
	if wrap != nil {
		exec = wrap(e)
	}
	return l.Pass(ctx, v, exec)
}

// nudges are the texts the fake runtime received.
func (f *laneFixture) nudges() []runtime.Call {
	var out []runtime.Call
	for _, c := range f.sp.Calls {
		if c.Method == "Nudge" || c.Method == "NudgeNow" {
			out = append(out, c)
		}
	}
	return out
}

func (f *laneFixture) fenceMisses() int {
	n := 0
	for _, c := range f.sp.Calls {
		if c.Method == "FenceMismatch" {
			n++
		}
	}
	return n
}

// recordingHandle records every nudge request its handle sees.
type recordingHandle struct {
	worker.Handle
	mu   *sync.Mutex
	reqs *[]worker.NudgeRequest
}

func (h recordingHandle) Nudge(ctx context.Context, by session.Actor, req worker.NudgeRequest) (worker.NudgeResult, error) {
	h.mu.Lock()
	*h.reqs = append(*h.reqs, req)
	h.mu.Unlock()
	return h.Handle.Nudge(ctx, by, req)
}

// recordRequests makes e's handles record their nudge requests into reqs.
func recordRequests(e *conversationLaneExec, mu *sync.Mutex, reqs *[]worker.NudgeRequest) {
	inner := e.handle
	e.handle = func(t nudgeTarget) (worker.Handle, error) {
		h, err := inner(t)
		if err != nil {
			return nil, err
		}
		return recordingHandle{Handle: h, mu: mu, reqs: reqs}, nil
	}
}

// beforeDeliverExec runs before ahead of its executor's Deliver.
type beforeDeliverExec struct {
	*conversationLaneExec
	before func()
}

func (e beforeDeliverExec) Deliver(ctx context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	e.before()
	return e.conversationLaneExec.Deliver(ctx, d)
}

// TestConversationLaneViewFields pins every field of the view.
func TestConversationLaneViewFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		info session.Info
		want opjournal.ConversationView
	}{
		{
			"active with a token",
			session.Info{ID: "gc-1", MetadataState: "active", Generation: "3", ContinuationEpoch: "2", InstanceToken: "tok"},
			opjournal.ConversationView{ID: "gc-1", Generation: 3, Epoch: 2, Token: "tok"},
		},
		{
			"no token keeps the counters",
			session.Info{ID: "gc-1", MetadataState: "active", Generation: "3", ContinuationEpoch: "2"},
			opjournal.ConversationView{ID: "gc-1", Generation: 3, Epoch: 2},
		},
		{
			"missing counters read as defaults",
			session.Info{ID: "gc-1", MetadataState: "active", InstanceToken: "tok"},
			opjournal.ConversationView{ID: "gc-1", Generation: int64(session.DefaultGeneration), Epoch: int64(session.DefaultContinuationEpoch), Token: "tok"},
		},
		{
			"closed",
			session.Info{ID: "gc-1", Closed: true, Generation: "1", ContinuationEpoch: "1", InstanceToken: "tok"},
			opjournal.ConversationView{ID: "gc-1", Closed: true, Generation: 1, Epoch: 1, Token: "tok"},
		},
		{
			"archived",
			session.Info{ID: "gc-1", MetadataState: "archived", Generation: "1", ContinuationEpoch: "1"},
			opjournal.ConversationView{ID: "gc-1", Closed: true, Generation: 1, Epoch: 1},
		},
		{
			"asleep",
			session.Info{ID: "gc-1", MetadataState: "asleep", Generation: "2", ContinuationEpoch: "1", InstanceToken: "tok"},
			opjournal.ConversationView{ID: "gc-1", Generation: 2, Epoch: 1, Token: "tok", Dormant: true},
		},
		{
			"drained",
			session.Info{ID: "gc-1", MetadataState: "drained", Generation: "2", ContinuationEpoch: "1"},
			opjournal.ConversationView{ID: "gc-1", Generation: 2, Epoch: 1, Dormant: true},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := conversationLaneView(tc.info); got != tc.want {
				t.Fatalf("view = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestConversationLaneViewReadsMissingRowClosed proves a row absent on two
// reads is a closed conversation, reported absent, and a read failure is an
// error.
func TestConversationLaneViewReadsMissingRowClosed(t *testing.T) {
	store := newLaneTestStore()
	v, _, absent, err := readConversationLaneView(sessionFrontDoor(store), "gc-404")
	if err != nil || !v.Closed || !absent || v.ID != "gc-404" {
		t.Fatalf("view of a missing row = %+v absent %v, %v; want closed and absent", v, absent, err)
	}
	notSession, err := store.Create(beads.Bead{Title: "task", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if v, _, absent, err := readConversationLaneView(sessionFrontDoor(store), notSession.ID); err != nil || !v.Closed || !absent {
		t.Fatalf("view of a non-session row = %+v absent %v, %v; want closed", v, absent, err)
	}
	if _, _, _, err := readConversationLaneView(sessionFrontDoor(laneFailingGetStore{store}), "gc-1"); err == nil {
		t.Fatal("a failed read returned no error")
	}
}

// laneFlakyNotFoundStore answers not found to the first Get of each id.
type laneFlakyNotFoundStore struct {
	laneTestStore
	mu   *sync.Mutex
	seen map[string]bool
}

func (s laneFlakyNotFoundStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	first := !s.seen[id]
	s.seen[id] = true
	s.mu.Unlock()
	if first {
		return beads.Bead{}, fmt.Errorf("flaky read of %s: %w", id, beads.ErrNotFound)
	}
	return s.laneTestStore.Get(id)
}

// TestConversationLaneViewConfirmsAbsence proves one not-found answer does
// not close a conversation: the second read finds the row.
func TestConversationLaneViewConfirmsAbsence(t *testing.T) {
	base := newLaneTestStore()
	f := newLaneFixtureOver(t, base)
	flaky := laneFlakyNotFoundStore{laneTestStore: base, mu: &sync.Mutex{}, seen: map[string]bool{}}
	v, info, absent, err := readConversationLaneView(sessionFrontDoor(flaky), f.id)
	if err != nil || v.Closed || absent || info.ID != f.id {
		t.Fatalf("view = %+v absent %v, %v; want the open row from the second read", v, absent, err)
	}
}

// laneFailingGetStore fails every Get.
type laneFailingGetStore struct{ laneTestStore }

func (laneFailingGetStore) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("store down")
}

// TestLaneExecDeliveryResultMapping pins the result mapping row by row, the
// tmux rows with the errors that runtime returns.
func TestLaneExecDeliveryResultMapping(t *testing.T) {
	at := laneT0
	delivered := func(ack opjournal.AckKind, degradation string) opjournal.DeliveryResult {
		return opjournal.Delivered{Record: opjournal.DeliveryRecord{At: at, Ack: ack}, Via: laneViaLive, Degradation: degradation}
	}
	steered := func(ack opjournal.AckKind, degradation string) opjournal.DeliveryResult {
		return opjournal.Delivered{Record: opjournal.DeliveryRecord{At: at, Ack: ack}, Via: laneViaSteer, Degradation: degradation}
	}
	someErr := errors.New("tmux exploded")
	permissionPrompt := fmt.Errorf("%w: session %q is waiting on a permission prompt (Edit file, request tmux-1); answer it with respond (approve or deny) before sending text",
		runtime.ErrPendingInteraction, "s")
	for _, tc := range []struct {
		name       string
		res        worker.NudgeResult
		err        error
		unfenced   bool
		steer      bool
		want       opjournal.DeliveryResult
		transient  bool
		wantUnknwn bool
	}{
		{name: "delivered", res: worker.NudgeResult{Delivered: true}, want: delivered(opjournal.AckDelivered, "")},
		{name: "delivered unfenced", res: worker.NudgeResult{Delivered: true, Unfenced: true}, want: delivered(opjournal.AckDelivered, opjournal.DegradationUnfencedDelivery)},
		{name: "delivered with no token", res: worker.NudgeResult{Delivered: true}, unfenced: true, want: delivered(opjournal.AckDelivered, opjournal.DegradationUnfencedDelivery)},
		{name: "unobserved", err: fmt.Errorf("submit: %w", runtime.ErrNudgeDeliveredUnobserved), want: delivered(opjournal.AckDeliveredUnobserved, laneDegradationTurnStartUnobserved)},
		{name: "unobserved and unfenced", res: worker.NudgeResult{Unfenced: true}, err: runtime.ErrNudgeDeliveredUnobserved, want: delivered(opjournal.AckDeliveredUnobserved, opjournal.DegradationUnfencedDelivery)},
		{name: "steer delivered", res: worker.NudgeResult{Delivered: true}, steer: true, want: steered(opjournal.AckDelivered, "")},
		{name: "steer unobserved", err: runtime.ErrNudgeDeliveredUnobserved, steer: true, want: steered(opjournal.AckDeliveredUnobserved, laneDegradationTurnStartUnobserved)},
		{name: "steer unobserved and unfenced", err: runtime.ErrNudgeDeliveredUnobserved, unfenced: true, steer: true, want: steered(opjournal.AckDeliveredUnobserved, opjournal.DegradationUnfencedDelivery)},
		{name: "not delivered", res: worker.NudgeResult{}, transient: true},
		{name: "pending interaction", err: fmt.Errorf("x: %w", runtime.ErrNudgeRefusedPendingInteraction), transient: true},
		{name: "tmux permission prompt", err: permissionPrompt, transient: true},
		{name: "tmux submit unconfirmed", err: fmt.Errorf("%w: session %q", tmux.ErrNudgeSubmitUnconfirmed, "s"), wantUnknwn: true},
		{name: "tmux re-send refused after a submit", err: fmt.Errorf("%w: session %q: not re-sent: %s", tmux.ErrNudgeSubmitUnconfirmed, "s", permissionPrompt.Error()), wantUnknwn: true},
		{name: "steer submit unconfirmed", err: fmt.Errorf("%w: session %q", tmux.ErrNudgeSubmitUnconfirmed, "s"), steer: true, wantUnknwn: true},
		{name: "steer on a tmux permission prompt", err: permissionPrompt, steer: true, transient: true},
		{name: "fence miss", err: &runtime.FenceMismatchError{Session: "s", Found: "absent"}, transient: true},
		{name: "runtime unavailable", err: fmt.Errorf("x: %w", runtime.ErrRuntimeUnavailable), transient: true},
		{name: "partial fence miss", err: &runtime.FenceMismatchError{Session: "s", Found: "dead", Partial: true}, wantUnknwn: true},
		{name: "canceled during the call", err: fmt.Errorf("x: %w", context.Canceled), wantUnknwn: true},
		{name: "deadline during the call", err: fmt.Errorf("x: %w", context.DeadlineExceeded), wantUnknwn: true},
		{name: "rejected", err: fmt.Errorf("x: %w", runtime.ErrNudgeRejected), want: opjournal.Rejected{KeyRejected: true, Reason: "x: " + runtime.ErrNudgeRejected.Error()}},
		{name: "tmux re-send failed after a submit", err: fmt.Errorf("failed to send submit sequence: %w", fmt.Errorf("%w: re-send failed: %w", tmux.ErrNudgeSubmitUnconfirmed, someErr)), wantUnknwn: true},
		{name: "server lost after fenced input", err: fmt.Errorf("%w after input was applied: %w", runtime.ErrNudgeDeliveryUnconfirmed, fmt.Errorf("x: %w", runtime.ErrRuntimeUnavailable)), wantUnknwn: true},
		{name: "not sent", err: fmt.Errorf("%w: nudge lock timeout for session %q", runtime.ErrNudgeNotSent, "s"), transient: true},
		{name: "anything else may have delivered", err: someErr, wantUnknwn: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := laneDeliveryResult(tc.res, tc.err, at, tc.unfenced, tc.steer)
			switch {
			case tc.transient:
				nd, ok := got.(opjournal.NotDelivered)
				if !ok || !nd.Transient {
					t.Fatalf("result = %#v, want a transient NotDelivered", got)
				}
			case tc.wantUnknwn:
				if _, ok := got.(opjournal.Unknown); !ok {
					t.Fatalf("result = %#v, want Unknown", got)
				}
			default:
				if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", tc.want) {
					t.Fatalf("result = %#v, want %#v", got, tc.want)
				}
			}
		})
	}
}

// TestLaneExecControlUnsupported proves interrupt and respond are refused
// until their slices land.
func TestLaneExecControlUnsupported(t *testing.T) {
	e := &conversationLaneExec{}
	want := opjournal.Rejected{Arm: opjournal.ArmFailed, Reason: opjournal.ReasonUnsupported}
	if got := e.Interrupt(context.Background(), opjournal.InterruptCall{}); got != want {
		t.Fatalf("Interrupt = %#v, want %#v", got, want)
	}
	if got := e.Respond(context.Background(), opjournal.RespondCall{}); got != want {
		t.Fatalf("Respond = %#v, want %#v", got, want)
	}
}

// TestLaneExecDeliversFencedRequest proves a delivery carries the Take's
// fence, the gc:-scoped delivery key, live-only default delivery and
// AllowUnfenced, closes succeeded with link none (no turn source), and stamps
// the session's last nudge.
func TestLaneExecDeliversFencedRequest(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "please run the tests"))
	var mu sync.Mutex
	var reqs []worker.NudgeRequest
	res, err := f.pass(context.Background(), t, l, st, func(e *conversationLaneExec) opjournal.Executor {
		recordRequests(e, &mu, &reqs)
		return e
	})
	if err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v, %v; want accepted", res, err)
	}
	got := f.get(t, st, op.ID)
	if len(reqs) != 1 {
		t.Fatalf("nudge requests = %d, want 1", len(reqs))
	}
	req := reqs[0]
	wantFence := runtime.InputFence{Generation: 1, Epoch: 1, Token: f.token}
	if req.Fence != wantFence || !req.AllowUnfenced || req.Wake != worker.NudgeWakeLiveOnly ||
		req.Delivery != worker.NudgeDeliveryDefault || req.DeliveryKey != "gc:"+got.DeliveryKey || req.Source != "" {
		t.Fatalf("request = %+v, want fence %+v, AllowUnfenced, live_only, default, key gc:%s, no source", req, wantFence, got.DeliveryKey)
	}
	if req.Text != "please run the tests" {
		t.Fatalf("text = %q, want the prompt verbatim", req.Text)
	}
	calls := f.nudges()
	if len(calls) != 1 || calls[0].Fence != wantFence || calls[0].Message != "please run the tests" {
		t.Fatalf("runtime nudges = %+v, want one fenced prompt", calls)
	}
	o := got.Outcome
	if got.Status != opjournal.StatusClosed || o.Arm != opjournal.ArmSucceeded || o.Degradation != "" ||
		o.Result.Via != laneViaLive || o.Result.Delivery.Ack != opjournal.AckDelivered {
		t.Fatalf("op = %s %+v, want succeeded via live, acked delivered", got.Status, o)
	}
	if o.Result.TurnLink != opjournal.LinkNone || o.Result.TurnBasis != opjournal.BasisNoSource {
		t.Fatalf("link = %s/%s, want none/no_source", o.Result.TurnLink, o.Result.TurnBasis)
	}
	row, err := f.store.Get(f.id)
	if err != nil || row.Metadata[session.MetadataLastNudgeDeliveredAt] == "" {
		t.Fatalf("session row = %v, %v; want the last nudge stamped", row.Metadata, err)
	}
}

// TestLaneExecUnfencedRuntime proves a runtime that cannot fence still
// delivers, and the op closes succeeded with unfenced_delivery.
func TestLaneExecUnfencedRuntime(t *testing.T) {
	f := newLaneFixture(t)
	f.sp.InputFencingValue = runtime.InputFencingNone
	st := f.laneStore(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
	if res, err := f.pass(context.Background(), t, l, st, nil); err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v, %v", res, err)
	}
	got := f.get(t, st, op.ID)
	if got.Outcome.Arm != opjournal.ArmSucceeded || got.Outcome.Degradation != opjournal.DegradationUnfencedDelivery {
		t.Fatalf("outcome = %+v, want succeeded with unfenced_delivery", got.Outcome)
	}
	if calls := f.nudges(); len(calls) != 1 || calls[0].Fence != (runtime.InputFence{}) {
		t.Fatalf("runtime nudges = %+v, want one unfenced nudge", calls)
	}
}

// TestLaneExecTokenlessRow proves a row with no instance token delivers
// unfenced and records it.
func TestLaneExecTokenlessRow(t *testing.T) {
	f := newLaneFixture(t, noLaneToken)
	st := f.laneStore(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("k1", opjournal.RoleNotice, "fyi"))
	var mu sync.Mutex
	var reqs []worker.NudgeRequest
	if res, err := f.pass(context.Background(), t, l, st, func(e *conversationLaneExec) opjournal.Executor {
		recordRequests(e, &mu, &reqs)
		return e
	}); err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v, %v", res, err)
	}
	if len(reqs) != 1 || !reqs[0].Fence.IsZero() {
		t.Fatalf("requests = %+v, want one with no fence", reqs)
	}
	if got := f.get(t, st, op.ID); got.Outcome.Degradation != opjournal.DegradationUnfencedDelivery {
		t.Fatalf("outcome = %+v, want unfenced_delivery", got.Outcome)
	}
}

// TestLaneExecRespawnBetweenTakeAndDelivery proves a respawn (a new token)
// after the Take misses the fence: no text reaches the runtime, and the op
// is released without a failure.
func TestLaneExecRespawnBetweenTakeAndDelivery(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
	res, err := f.pass(context.Background(), t, l, st, func(e *conversationLaneExec) opjournal.Executor {
		return beforeDeliverExec{conversationLaneExec: e, before: func() { f.startRuntime(t, "tok-2") }}
	})
	if err != nil || res.Done != opjournal.DoneReleased {
		t.Fatalf("Pass = %+v, %v; want released", res, err)
	}
	if calls := f.nudges(); len(calls) != 0 {
		t.Fatalf("runtime nudges = %+v, want none", calls)
	}
	if f.fenceMisses() != 1 {
		t.Fatalf("fence misses = %d, want 1", f.fenceMisses())
	}
	got := f.get(t, st, op.ID)
	if got.Status != opjournal.StatusAccepted || got.Failures != 0 || got.RetryAt.IsZero() {
		t.Fatalf("op = %s failures %d retry_at %v; want a transient release", got.Status, got.Failures, got.RetryAt)
	}
}

// TestLaneExecUnconfirmedSubmitMayHaveDelivered proves a tmux submit the
// runtime could not confirm counts as possibly delivered: an at_most_once
// message closes failed{ambiguous_delivery} instead of being sent again, and
// an at_least_once one is retried marked maybe delivered.
func TestLaneExecUnconfirmedSubmitMayHaveDelivered(t *testing.T) {
	for _, tc := range []struct {
		name      string
		guarantee opjournal.Guarantee
		status    opjournal.Status
		arm       opjournal.Arm
		reason    string
		failures  int
	}{
		{name: "at most once", guarantee: opjournal.GuaranteeAtMostOnce, status: opjournal.StatusClosed, arm: opjournal.ArmFailed, reason: opjournal.ReasonAmbiguousDelivery},
		{name: "at least once", guarantee: opjournal.GuaranteeAtLeastOnce, status: opjournal.StatusAccepted, failures: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			f.sp.NudgeErrors = map[string]error{f.name: fmt.Errorf("%w: session %q", tmux.ErrNudgeSubmitUnconfirmed, f.name)}
			st := f.laneStore(t)
			op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi", func(r *opjournal.Request) { r.Policy.Guarantee = tc.guarantee }))
			if res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil); err != nil || res.Done != opjournal.DoneReleased {
				t.Fatalf("Pass = %+v, %v; want released", res, err)
			}
			if calls := f.nudges(); len(calls) != 1 {
				t.Fatalf("runtime nudges = %+v, want the one unconfirmed submit", calls)
			}
			got := f.get(t, st, op.ID)
			if got.Status != tc.status || got.Outcome.Arm != tc.arm || got.Outcome.Reason != tc.reason ||
				got.Failures != tc.failures || got.MaybeDelivered != 1 {
				t.Fatalf("op = %s %+v failures %d maybe_delivered %d; want %s %s{%s} failures %d maybe_delivered 1",
					got.Status, got.Outcome, got.Failures, got.MaybeDelivered, tc.status, tc.arm, tc.reason, tc.failures)
			}
		})
	}
}

// TestLaneExecProtocolModeWaitsNotLive proves a protocol-mode runtime that
// does not report its turns has no boundary to deliver at, so it is not
// live: content waits not_live and nothing is typed.
func TestLaneExecProtocolModeWaitsNotLive(t *testing.T) {
	f := newLaneFixture(t)
	f.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol
	st := f.laneStore(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
	res, err := f.pass(context.Background(), t, l, st, nil)
	if err != nil || res.Took != opjournal.TakeNone {
		t.Fatalf("Pass = %+v, %v; want nothing taken", res, err)
	}
	if !laneWaitsFor(res, opjournal.WaitNotLive) {
		t.Fatalf("waits = %+v, want not_live", res.Waits)
	}
	if len(f.nudges()) != 0 || f.get(t, st, op.ID).Status != opjournal.StatusSubmitted {
		t.Fatal("a protocol-mode runtime received text")
	}
}

// stampHost writes a runtime row whose host stamp names gen and kinds.
func stampHost(t *testing.T, f *laneFixture, st *opjournal.Store, gen int64, kinds ...opjournal.Kind) {
	t.Helper()
	row := opjournal.RuntimeRow{
		Conversation: f.id, Generation: 1, Epoch: 1,
		Host: opjournal.HostStamp{Host: "box:b1#n1", Generation: gen, Kinds: kinds, Protocol: "v1", Build: "test", Since: laneT0},
	}
	b, err := opjournal.EncodeRuntimeRow(st.Prefix(), row)
	if err != nil {
		t.Fatalf("EncodeRuntimeRow: %v", err)
	}
	if _, err := f.store.Create(b); err != nil {
		t.Fatalf("creating the runtime row: %v", err)
	}
}

// TestLaneExecDefersToHostStamp proves a current-generation stamp listing
// message makes content not live while rule closes still run, and a stamp
// for an older generation is ignored.
func TestLaneExecDefersToHostStamp(t *testing.T) {
	t.Run("current generation", func(t *testing.T) {
		f := newLaneFixture(t)
		st := f.laneStore(t)
		stampHost(t, f, st, 1, opjournal.KindMessage)
		l := f.lane(t, st)
		waiting := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
		expired := f.propose(t, st, f.msg("k2", opjournal.RoleNotice, "late", func(r *opjournal.Request) {
			r.StartBy = laneT0.Add(time.Minute)
		}))
		f.clk.Advance(2 * time.Minute)
		res, err := f.pass(context.Background(), t, l, st, nil)
		if err != nil || res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitNotLive) {
			t.Fatalf("Pass = %+v, %v; want a not_live wait and nothing taken", res, err)
		}
		if got := f.get(t, st, expired.ID); got.Outcome.Arm != opjournal.ArmFailed || got.Outcome.Reason != opjournal.ReasonExpiredBeforeStart {
			t.Fatalf("expired op = %+v, want the rule close failed{expired_before_start}", got.Outcome)
		}
		if len(f.nudges()) != 0 || f.get(t, st, waiting.ID).Status != opjournal.StatusSubmitted {
			t.Fatal("a stamped conversation received text")
		}
	})
	t.Run("older generation", func(t *testing.T) {
		f := newLaneFixture(t)
		st := f.laneStore(t)
		stampHost(t, f, st, 0, opjournal.KindMessage)
		l := f.lane(t, st)
		f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
		if res, err := f.pass(context.Background(), t, l, st, nil); err != nil || res.Done != opjournal.DoneAccepted {
			t.Fatalf("Pass = %+v, %v; want the older stamp ignored", res, err)
		}
	})
	t.Run("other kinds", func(t *testing.T) {
		f := newLaneFixture(t)
		st := f.laneStore(t)
		stampHost(t, f, st, 1, opjournal.KindInterrupt)
		l := f.lane(t, st)
		f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
		if res, err := f.pass(context.Background(), t, l, st, nil); err != nil || res.Done != opjournal.DoneAccepted {
			t.Fatalf("Pass = %+v, %v; want a stamp without message ignored", res, err)
		}
	})
}

// TestLaneExecProbeGates proves the probe's content gates: a row's content
// gate and a pending interaction each block, and a busy runtime waits.
func TestLaneExecProbeGates(t *testing.T) {
	t.Run("restart pending", func(t *testing.T) {
		f := newLaneFixture(t, func(meta map[string]string) { meta["restart_requested"] = "true" })
		st := f.laneStore(t)
		f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
		res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
		if err != nil || res.Took != opjournal.TakeNone || len(res.Waits) != 1 ||
			res.Waits[0].Reason != opjournal.WaitContentBlock || res.Waits[0].Detail != session.ContentGateRestartPending {
			t.Fatalf("Pass = %+v, %v; want content_block restart_pending", res, err)
		}
	})
	t.Run("pending interaction", func(t *testing.T) {
		f := newLaneFixture(t)
		f.sp.PendingInteractions = map[string]*runtime.PendingInteraction{f.name: {RequestID: "req-1"}}
		st := f.laneStore(t)
		f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
		res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
		if err != nil || res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitPendingInteraction) {
			t.Fatalf("Pass = %+v, %v; want pending_interaction", res, err)
		}
	})
	t.Run("busy", func(t *testing.T) {
		f := newLaneFixture(t)
		f.sp.SetActivity(f.name, time.Now())
		st := f.laneStore(t)
		f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
		res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
			e.idle = func(nudgeTarget, worker.LiveObservation) bool { return false }
			return e
		})
		if err != nil || res.Took != opjournal.TakeNone || !res.Busy {
			t.Fatalf("Pass = %+v, %v; want busy", res, err)
		}
	})
	t.Run("not running", func(t *testing.T) {
		f := newLaneFixture(t)
		if err := f.sp.Stop(f.name); err != nil {
			t.Fatal(err)
		}
		st := f.laneStore(t)
		f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi", func(r *opjournal.Request) { r.Policy.Message.Wake = opjournal.WakeLiveOnly }))
		res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
		if err != nil || res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitNotRunning) {
			t.Fatalf("Pass = %+v, %v; want not_running", res, err)
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		f := newLaneFixture(t)
		st := f.laneStore(t)
		f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
		res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
			e.observe = func(nudgeTarget) (worker.LiveObservation, error) {
				return worker.LiveObservation{}, fmt.Errorf("probe: %w", runtime.ErrRuntimeUnavailable)
			}
			return e
		})
		if err != nil || res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitUnavailable) {
			t.Fatalf("Pass = %+v, %v; want unavailable", res, err)
		}
	})
}

// laneIdentifiers are the strings no delivered text may carry (I14).
func laneIdentifiers(st *opjournal.Store, ops []opjournal.Op) []string {
	var out []string
	for _, op := range ops {
		cur, err := st.Get(op.ID)
		if err != nil {
			continue
		}
		out = append(out, op.ID, op.ClientKey, op.Producer.Principal, cur.DeliveryKey, "gc:"+cur.DeliveryKey)
		if !cur.Lease.IsZero() {
			out = append(out, cur.Lease.String())
		}
	}
	return append(out, "conversation@", "#"+laneHostName+".", "lane_seq", "attempt")
}

// TestLaneExecDeliveredTextCarriesNoMarker proves D-NOMARK (I14): the text
// the runtime receives equals RenderDelivery's for a prompt, a notice batch
// and a mixed batch, and carries no op id, client key, delivery key, lease,
// lane seq, attempt or principal.
func TestLaneExecDeliveredTextCarriesNoMarker(t *testing.T) {
	for _, tc := range []struct {
		name  string
		style opjournal.TextStyle
		opts  []laneFixtureOpt
		reqs  func(f *laneFixture) []opjournal.Request
	}{
		{"prompt", opjournal.TextReminder, nil, func(f *laneFixture) []opjournal.Request {
			return []opjournal.Request{f.msg("k-prompt", opjournal.RolePrompt, "fix the build")}
		}},
		{"notice batch", opjournal.TextReminder, nil, func(f *laneFixture) []opjournal.Request {
			return []opjournal.Request{f.msg("k-n1", opjournal.RoleNotice, "mail arrived"), f.msg("k-n2", opjournal.RoleNotice, "work slung")}
		}},
		{"mixed batch", opjournal.TextReminder, nil, func(f *laneFixture) []opjournal.Request {
			return []opjournal.Request{f.msg("k-n1", opjournal.RoleNotice, "mail arrived"), f.msg("k-p", opjournal.RolePrompt, "now review it")}
		}},
		{"plain style", opjournal.TextPlain, []laneFixtureOpt{laneTransport("acp")}, func(f *laneFixture) []opjournal.Request {
			return []opjournal.Request{f.msg("k-n1", opjournal.RoleNotice, "mail arrived")}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t, tc.opts...)
			st := f.laneStore(t)
			var ops []opjournal.Op
			for _, r := range tc.reqs(f) {
				ops = append(ops, f.propose(t, st, r))
			}
			var leases []string
			res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
				return beforeDeliverExec{conversationLaneExec: e, before: func() {
					for _, op := range ops {
						if cur, err := st.Get(op.ID); err == nil && !cur.Lease.IsZero() {
							leases = append(leases, cur.Lease.String())
						}
					}
				}}
			})
			if err != nil || res.Done != opjournal.DoneAccepted || len(res.Taken) != len(ops) {
				t.Fatalf("Pass = %+v, %v; want one accepted batch of %d", res, err, len(ops))
			}
			want, err := opjournal.RenderDelivery(ops, tc.style, false)
			if err != nil {
				t.Fatalf("RenderDelivery: %v", err)
			}
			calls := f.nudges()
			if len(calls) != 1 || calls[0].Message != want {
				t.Fatalf("runtime text = %+v, want %q", calls, want)
			}
			if len(leases) == 0 {
				t.Fatal("no lease was seen during the effect")
			}
			for _, id := range append(laneIdentifiers(st, ops), leases...) {
				if id != "" && strings.Contains(calls[0].Message, id) {
					t.Fatalf("delivered text %q carries identifier %q", calls[0].Message, id)
				}
			}
		})
	}
}

// dyingStore models a host process that dies: once dead, every guarded
// batch it applies fails.
type dyingStore struct {
	laneTestStore
	dead *atomic.Bool
}

func (s dyingStore) GuardedBatcherHandle() (beads.GuardedBatcher, bool) {
	return dyingBatcher{inner: s.MemStore, dead: s.dead}, true
}

type dyingBatcher struct {
	inner beads.GuardedBatcher
	dead  *atomic.Bool
}

func (b dyingBatcher) ApplyGuarded(ctx context.Context, batch beads.GuardedBatch) (beads.GuardedResult, error) {
	if b.dead.Load() {
		// An invalid-batch answer, which Done does not retry, so the dead
		// host gives up at once instead of retrying out its budget.
		return beads.GuardedResult{}, fmt.Errorf("%w: the host process died", beads.ErrGuardedBatchUnsupported)
	}
	return b.inner.ApplyGuarded(ctx, batch)
}

// TestLaneExecRedeliveryAfterHostDies proves the redelivery path: the first
// host's ctx is canceled mid-effect and its Done never lands; a second host
// over the same store, once the fake clock passes the lease, re-takes the op,
// types the text again with the D-NOTE line, and the op records the attempt
// and maybe_delivered.
func TestLaneExecRedeliveryAfterHostDies(t *testing.T) {
	base := newLaneTestStore()
	dead := new(atomic.Bool)
	f := newLaneFixtureOver(t, dyingStore{laneTestStore: base, dead: dead})
	first := f.laneStore(t)
	op := f.propose(t, first, f.msg("k1", opjournal.RolePrompt, "deploy it"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := f.pass(ctx, t, f.lane(t, first), first, func(e *conversationLaneExec) opjournal.Executor {
		return afterDeliverExec{conversationLaneExec: e, after: func() {
			dead.Store(true)
			cancel()
		}}
	})
	if err == nil {
		t.Fatal("the dying host's pass reported no error")
	}
	if got := f.get(t, first, op.ID); got.Status != opjournal.StatusExecuting {
		t.Fatalf("op after the crash = %s, want still executing", got.Status)
	}

	// The second host runs over the same rows, without the dying wrapper.
	f.store = base
	second := f.laneStore(t)
	f.clk.Advance(opjournal.DefaultLeaseTTL + opjournal.LeaseSkew + time.Second)
	res, err := f.pass(context.Background(), t, f.lane(t, second), second, nil)
	if err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("second host Pass = %+v, %v; want accepted", res, err)
	}
	calls := f.nudges()
	if len(calls) != 2 || calls[0].Message != "deploy it" {
		t.Fatalf("runtime texts = %+v, want the prompt twice", calls)
	}
	cur := f.get(t, second, op.ID)
	want, err := opjournal.RenderDelivery([]opjournal.Op{cur}, opjournal.TextReminder, true)
	if err != nil {
		t.Fatalf("RenderDelivery: %v", err)
	}
	if calls[1].Message != want || !strings.HasSuffix(calls[1].Message, opjournal.RedeliveryNote) {
		t.Fatalf("redelivered text = %q, want %q ending with the D-NOTE line", calls[1].Message, want)
	}
	if cur.Attempt != 2 || cur.MaybeDelivered != 1 || cur.Outcome.Arm != opjournal.ArmSucceeded {
		t.Fatalf("op = attempt %d maybe_delivered %d outcome %+v; want attempt 2, maybe_delivered 1, succeeded", cur.Attempt, cur.MaybeDelivered, cur.Outcome)
	}
	for _, id := range laneIdentifiers(second, []opjournal.Op{cur}) {
		if id != "" && strings.Contains(calls[1].Message, id) {
			t.Fatalf("redelivered text carries identifier %q", id)
		}
	}
}

// afterDeliverExec runs after once its executor's Deliver returns.
type afterDeliverExec struct {
	*conversationLaneExec
	after func()
}

func (e afterDeliverExec) Deliver(ctx context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	r := e.conversationLaneExec.Deliver(ctx, d)
	e.after()
	return r
}

// TestLaneExecIncarnationMismatchBlocksContent proves a running runtime whose
// live GC_SESSION_ID names another session is a content block
// (incarnation): the op waits on it, asks for no wake, and nothing is typed.
func TestLaneExecIncarnationMismatchBlocksContent(t *testing.T) {
	f := newLaneFixture(t)
	if err := f.sp.SetMeta(f.name, "GC_SESSION_ID", "gc-someone-else"); err != nil {
		t.Fatal(err)
	}
	st := f.laneStore(t)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi", func(r *opjournal.Request) {
		r.Policy.Message.Wake = opjournal.WakeIfNeeded
	}))
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
	if err != nil || res.Took != opjournal.TakeNone || res.Wake || len(res.Waits) != 1 ||
		res.Waits[0].Reason != opjournal.WaitContentBlock || res.Waits[0].Detail != laneContentBlockIncarnation {
		t.Fatalf("Pass = %+v, %v; want a content_block incarnation wait and no wake", res, err)
	}
	if len(f.nudges()) != 0 || f.get(t, st, op.ID).Status != opjournal.StatusSubmitted {
		t.Fatal("a stale incarnation received text")
	}
}

// TestLaneExecAdmitsTheDrainNoticeIntoADrainingRow pins the executor's half
// of I7's exemption: a draining row's probe blocks content as draining but
// admits the drain reminder's notice class, so the lane delivers that notice
// and nothing else; a stale incarnation blocks the notice too.
func TestLaneExecAdmitsTheDrainNoticeIntoADrainingRow(t *testing.T) {
	draining := func(meta map[string]string) {
		meta["state"] = string(session.StateDraining)
		meta["state_reason"] = session.DrainAckStopPendingReason
	}
	drainNotice := func(r *opjournal.Request) {
		r.Producer = opjournal.Producer{Principal: laneDrainNoticeClass.Principal, Actor: opjournal.ActorController}
		r.Policy.Message = opjournal.MessagePolicy{Role: opjournal.RoleNotice, When: opjournal.WhenBoundary, Wake: opjournal.WakeLiveOnly}
	}
	t.Run("delivers the drain notice, holds the rest", func(t *testing.T) {
		f := newLaneFixture(t, draining)
		st := f.laneStore(t)
		prompt := f.propose(t, st, f.msg("prompt", opjournal.RolePrompt, "work"))
		other := f.propose(t, st, f.msg("other", opjournal.RoleNotice, "claim", func(r *opjournal.Request) {
			drainNotice(r)
			r.Producer.Principal = plannerNoticePrincipalPrefix + "idle-claim-nudge"
		}))
		warmBind := f.propose(t, st, f.msg("warm", opjournal.RoleNotice, "bind", func(r *opjournal.Request) {
			drainNotice(r)
			r.Producer.Principal = plannerNoticePrincipalPrefix + warmBindNoticePurpose
		}))
		drainPrompt := f.propose(t, st, f.msg("drain-prompt", opjournal.RolePrompt, "work", func(r *opjournal.Request) {
			drainNotice(r)
			r.Policy.Message.Role = opjournal.RolePrompt
		}))
		notice := f.propose(t, st, f.msg("drain", opjournal.RoleNotice, "ack your drain", drainNotice))
		for range 3 {
			if _, err := f.pass(context.Background(), t, f.lane(t, st), st, nil); err != nil {
				t.Fatalf("Pass: %v", err)
			}
		}
		if got := f.get(t, st, notice.ID); got.Status != opjournal.StatusClosed || got.Outcome.Arm != opjournal.ArmSucceeded {
			t.Fatalf("drain notice = %s %+v, want closed succeeded", got.Status, got.Outcome)
		}
		for _, id := range []string{prompt.ID, other.ID, warmBind.ID, drainPrompt.ID} {
			if got := f.get(t, st, id); got.Status != opjournal.StatusSubmitted {
				t.Fatalf("op %s = %s, want still submitted behind the draining block", id, got.Status)
			}
		}
		nudges := f.nudges()
		if len(nudges) != 1 || !strings.Contains(nudges[0].Message, "ack your drain") {
			t.Fatalf("nudges = %+v, want only the drain notice", nudges)
		}
	})
	t.Run("probe admits only the drain notice class", func(t *testing.T) {
		for name, opts := range map[string][]laneFixtureOpt{
			"ready row":    nil,
			"draining row": {draining},
		} {
			f := newLaneFixture(t, opts...)
			st := f.laneStore(t)
			v, info := f.view(t)
			p, err := newConversationLaneExec(f.deps(st), info).Probe(context.Background(), v)
			if err != nil {
				t.Fatalf("%s: Probe: %v", name, err)
			}
			var want []opjournal.ContentClass
			if name == "draining row" {
				want = []opjournal.ContentClass{laneDrainNoticeClass}
			}
			if !slices.Equal(p.ContentAdmit, want) {
				t.Fatalf("%s: ContentAdmit = %+v, want %+v", name, p.ContentAdmit, want)
			}
		}
	})
	// A draining row with a later reason in the gate's order holds the drain
	// notice too: only "draining" is exempt.
	for name, later := range map[string]laneFixtureOpt{
		"idle stop latch":   func(meta map[string]string) { meta["sleep_intent"] = "idle-stop-pending" },
		"restart requested": func(meta map[string]string) { meta["restart_requested"] = "true" },
		"reset pending":     func(meta map[string]string) { meta["continuation_reset_pending"] = "true" },
		"create claim":      func(meta map[string]string) { meta["pending_create_claim"] = "true" },
	} {
		t.Run("draining row, "+name, func(t *testing.T) {
			f := newLaneFixture(t, draining, later)
			st := f.laneStore(t)
			v, info := f.view(t)
			p, err := newConversationLaneExec(f.deps(st), info).Probe(context.Background(), v)
			if err != nil || p.ContentBlock == "" || len(p.ContentAdmit) != 0 {
				t.Fatalf("Probe = block %q admit %+v, %v; want a block that admits nothing", p.ContentBlock, p.ContentAdmit, err)
			}
			notice := f.propose(t, st, f.msg("drain", opjournal.RoleNotice, "ack your drain", drainNotice))
			res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
			if err != nil || res.Took != opjournal.TakeNone || len(res.Waits) != 1 || res.Waits[0].Reason != opjournal.WaitContentBlock {
				t.Fatalf("Pass = %+v, %v; want a content_block wait", res, err)
			}
			if len(f.nudges()) != 0 || f.get(t, st, notice.ID).Status != opjournal.StatusSubmitted {
				t.Fatal("the drain notice passed a reason other than draining")
			}
		})
	}
	t.Run("stale incarnation", func(t *testing.T) {
		f := newLaneFixture(t, draining)
		if err := f.sp.SetMeta(f.name, "GC_SESSION_ID", "gc-someone-else"); err != nil {
			t.Fatal(err)
		}
		st := f.laneStore(t)
		f.propose(t, st, f.msg("drain", opjournal.RoleNotice, "ack your drain", drainNotice))
		res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
		if err != nil || res.Took != opjournal.TakeNone || len(res.Waits) != 1 || res.Waits[0].Reason != opjournal.WaitContentBlock {
			t.Fatalf("Pass = %+v, %v; want a content_block wait", res, err)
		}
		if len(f.nudges()) != 0 {
			t.Fatal("a stale incarnation received the drain notice")
		}
	})
	// A hold blocks a consume only off a running runtime (HoldVerdict), so a
	// held draining row is one whose runtime has stopped.
	t.Run("held draining row", func(t *testing.T) {
		f := newLaneFixture(t, draining, func(meta map[string]string) {
			meta["sleep_intent"] = string(session.SleepReasonUserHold)
			meta["held_until"] = laneT0.Add(time.Hour).Format(time.RFC3339)
		})
		if err := f.sp.Stop(f.name); err != nil {
			t.Fatal(err)
		}
		st := f.laneStore(t)
		f.propose(t, st, f.msg("drain", opjournal.RoleNotice, "ack your drain", drainNotice))
		res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
		if err != nil || res.Took != opjournal.TakeNone || res.Wake || len(res.Waits) != 1 || res.Waits[0].Reason != opjournal.WaitContentBlock {
			t.Fatalf("Pass = %+v, %v; want a content_block wait and no wake", res, err)
		}
		if len(f.nudges()) != 0 {
			t.Fatal("a held row received the drain notice")
		}
	})
	t.Run("kill pending", func(t *testing.T) {
		f := newLaneFixture(t, func(meta map[string]string) {
			meta["state"] = string(session.StateAsleep)
			meta["state_reason"] = session.KillPendingReason
			meta["sleep_reason"] = string(session.SleepReasonKilled)
			meta["slept_at"] = laneT0.Format(time.RFC3339)
		})
		st := f.laneStore(t)
		f.propose(t, st, f.msg("drain", opjournal.RoleNotice, "ack your drain", drainNotice))
		res, err := f.pass(context.Background(), t, f.lane(t, st), st, nil)
		// The kill's asleep row is dormant to the lane before the gate is
		// asked; either way the notice waits.
		if err != nil || res.Took != opjournal.TakeNone || len(res.Waits) != 1 || len(f.nudges()) != 0 {
			t.Fatalf("Pass = %+v, %v; want the drain notice to wait", res, err)
		}
	})
}

// laneSeamCounts counts the executor's handle and idle calls.
type laneSeamCounts struct {
	handles, idles atomic.Int32
}

func countSeams(e *conversationLaneExec, c *laneSeamCounts) {
	handle, idle := e.handle, e.idle
	e.handle = func(t nudgeTarget) (worker.Handle, error) {
		c.handles.Add(1)
		return handle(t)
	}
	e.idle = func(t nudgeTarget, obs worker.LiveObservation) bool {
		c.idles.Add(1)
		return idle(t, obs)
	}
}

// TestLaneExecBlockedProbeSkipsLooks proves a probe that a content gate or a
// not-live runtime already decides resolves no handle and reads neither the
// pending interaction nor the idle boundary.
func TestLaneExecBlockedProbeSkipsLooks(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  []laneFixtureOpt
		setup func(f *laneFixture)
	}{
		{name: "content block", opts: []laneFixtureOpt{func(meta map[string]string) { meta["restart_requested"] = "true" }}},
		{name: "not live", setup: func(f *laneFixture) { f.sp.TurnInterruptModeValue = runtime.TurnInterruptProtocol }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t, tc.opts...)
			if tc.setup != nil {
				tc.setup(f)
			}
			st := f.laneStore(t)
			f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
			var c laneSeamCounts
			res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
				countSeams(e, &c)
				return e
			})
			if err != nil || res.Took != opjournal.TakeNone {
				t.Fatalf("Pass = %+v, %v; want nothing taken", res, err)
			}
			if c.handles.Load() != 0 || c.idles.Load() != 0 {
				t.Fatalf("handles %d idles %d, want none for a decided probe", c.handles.Load(), c.idles.Load())
			}
		})
	}
}

// TestLaneExecResolvesTheHandleOnce proves a delivering pass resolves the
// runtime's handle once: the probe's look and the delivery share it.
func TestLaneExecResolvesTheHandleOnce(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
	var c laneSeamCounts
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		countSeams(e, &c)
		return e
	})
	if err != nil || res.Done != opjournal.DoneAccepted {
		t.Fatalf("Pass = %+v, %v; want accepted", res, err)
	}
	if n := c.handles.Load(); n != 1 {
		t.Fatalf("handle resolutions = %d, want 1", n)
	}
}

// wedgedPendingHandle is a handle whose pending-interaction look never
// answers until released.
type wedgedPendingHandle struct {
	worker.Handle
	release chan struct{}
}

func (h wedgedPendingHandle) PendingStatus(context.Context) (*worker.PendingInteraction, bool, error) {
	<-h.release
	return nil, false, nil
}

// TestLaneExecProbeBoundsEveryLook proves a runtime call that wedges after
// the first observation is bounded too: the probe answers unavailable within
// its budget instead of holding the pass.
func TestLaneExecProbeBoundsEveryLook(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	start := time.Now()
	res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
		e.budget = 50 * time.Millisecond
		inner := e.handle
		e.handle = func(nt nudgeTarget) (worker.Handle, error) {
			h, err := inner(nt)
			if err != nil {
				return nil, err
			}
			return wedgedPendingHandle{Handle: h, release: release}, nil
		}
		return e
	})
	if err != nil || res.Took != opjournal.TakeNone || !laneWaitsFor(res, opjournal.WaitUnavailable) {
		t.Fatalf("Pass = %+v, %v; want an unavailable wait", res, err)
	}
	if d := time.Since(start); d > laneWaitBudget/2 {
		t.Fatalf("the probe took %s, want it bounded by its budget", d)
	}
}

// failingPendingHandle is a handle whose pending-interaction look fails.
type failingPendingHandle struct {
	worker.Handle
	err error
}

func (h failingPendingHandle) PendingStatus(context.Context) (*worker.PendingInteraction, bool, error) {
	return nil, false, h.err
}

// TestLaneExecFailedLookIsNotAPendingInteraction proves a look that cannot
// resolve the handle, or cannot read the pending interaction, fails the
// probe: nothing is taken and no op waits on a pending interaction the
// runtime never reported.
func TestLaneExecFailedLookIsNotAPendingInteraction(t *testing.T) {
	boom := errors.New("look failed")
	for _, tc := range []struct {
		name string
		wrap func(inner func(nudgeTarget) (worker.Handle, error)) func(nudgeTarget) (worker.Handle, error)
	}{
		{name: "handle", wrap: func(func(nudgeTarget) (worker.Handle, error)) func(nudgeTarget) (worker.Handle, error) {
			return func(nudgeTarget) (worker.Handle, error) { return nil, boom }
		}},
		{name: "pending status", wrap: func(inner func(nudgeTarget) (worker.Handle, error)) func(nudgeTarget) (worker.Handle, error) {
			return func(nt nudgeTarget) (worker.Handle, error) {
				h, err := inner(nt)
				if err != nil {
					return nil, err
				}
				return failingPendingHandle{Handle: h, err: boom}, nil
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newLaneFixture(t)
			st := f.laneStore(t)
			f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "hi"))
			res, err := f.pass(context.Background(), t, f.lane(t, st), st, func(e *conversationLaneExec) opjournal.Executor {
				e.handle = tc.wrap(e.handle)
				return e
			})
			if laneWaitsFor(res, opjournal.WaitPendingInteraction) {
				t.Fatalf("Pass = %+v; a failed look waits on a pending interaction", res)
			}
			if !errors.Is(err, boom) || res.Took != opjournal.TakeNone {
				t.Fatalf("Pass = %+v, %v; want the look's error and nothing taken", res, err)
			}
			if n := len(f.nudges()); n != 0 {
				t.Fatalf("nudges = %d, want none", n)
			}
		})
	}
}

// laneReadLog records every read of a store.
type laneReadLog struct {
	laneTestStore
	mu    *sync.Mutex
	gets  *[]string
	lists *[]beads.ListQuery
}

func (s laneReadLog) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	*s.gets = append(*s.gets, id)
	s.mu.Unlock()
	return s.laneTestStore.Get(id)
}

func (s laneReadLog) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	*s.lists = append(*s.lists, q)
	s.mu.Unlock()
	return s.laneTestStore.List(q)
}

// TestLaneExecReprobeReadsNoRow proves a re-probe of a busy runtime reads no
// op row and no runtime row: it reuses the last probe's caps.
func TestLaneExecReprobeReadsNoRow(t *testing.T) {
	var mu sync.Mutex
	var gets []string
	var lists []beads.ListQuery
	f := newLaneFixtureOver(t, laneReadLog{laneTestStore: newLaneTestStore(), mu: &mu, gets: &gets, lists: &lists})
	st := f.laneStore(t)
	stampHost(t, f, st, 0, opjournal.KindMessage)
	v, info := f.view(t)
	e := newConversationLaneExec(f.deps(st), info)
	e.idle = func(nudgeTarget, worker.LiveObservation) bool { return false }
	p, err := e.Probe(context.Background(), v)
	if err != nil || !p.Running || p.Boundary != opjournal.TriNo {
		t.Fatalf("Probe = %+v, %v; want running and busy", p, err)
	}
	runtimeRow, err := opjournal.RuntimeRowID(st.Prefix(), f.id)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	gets, lists = nil, nil
	mu.Unlock()
	p, err = e.Reprobe(context.Background(), v)
	if err != nil || !p.Running || p.Boundary != opjournal.TriNo || !p.Caps.Live {
		t.Fatalf("Reprobe = %+v, %v; want the busy runtime with the remembered caps", p, err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, id := range gets {
		if id == runtimeRow {
			t.Fatalf("the re-probe read the runtime row %s", id)
		}
	}
	for _, q := range lists {
		if q.Label == opjournal.LabelOp {
			t.Fatalf("the re-probe listed op rows: %+v", q)
		}
	}
}

// laneCachedFixture is a fixture whose sessions store is a primed cache over
// a backing that logs every read, the executor wired as the host wires it:
// lookups through the cache, the view and the op rows live.
type laneCachedFixture struct {
	*laneFixture
	cache *beads.CachingStore
	mu    *sync.Mutex
	gets  *[]string
	lists *[]beads.ListQuery
}

func newLaneCachedFixture(t *testing.T) *laneCachedFixture {
	t.Helper()
	cf := &laneCachedFixture{mu: &sync.Mutex{}, gets: &[]string{}, lists: &[]beads.ListQuery{}}
	backing := laneReadLog{laneTestStore: newLaneTestStore(), mu: cf.mu, gets: cf.gets, lists: cf.lists}
	cf.cache = beads.NewCachingStore(backing, nil)
	if err := cf.cache.Prime(context.Background()); err != nil {
		t.Fatalf("priming the cache: %v", err)
	}
	cf.laneFixture = newLaneFixtureOver(t, cf.cache)
	return cf
}

// deps are the executor's dependencies as the host builds them.
func (cf *laneCachedFixture) deps(st *opjournal.Store) laneExecDeps {
	d := cf.laneFixture.deps(st)
	d.sessions = cf.cache
	return d
}

// backingReads drains and returns the reads that reached the backing.
func (cf *laneCachedFixture) backingReads() (gets []string, lists []beads.ListQuery) {
	cf.mu.Lock()
	defer cf.mu.Unlock()
	gets, lists = *cf.gets, *cf.lists
	*cf.gets, *cf.lists = nil, nil
	return gets, lists
}

// TestLaneExecReprobeOverCacheReadsNoBacking proves a re-probe of a busy
// runtime, with the production executor over the controller's primed cache,
// reads nothing from the backing store (DES-O1 §4.3).
func TestLaneExecReprobeOverCacheReadsNoBacking(t *testing.T) {
	cf := newLaneCachedFixture(t)
	st := cf.laneStore(t)
	v, info := cf.view(t)
	e := newConversationLaneExec(cf.deps(st), info)
	e.idle = func(nudgeTarget, worker.LiveObservation) bool { return false }
	if p, err := e.Probe(context.Background(), v); err != nil || !p.Running || p.Boundary != opjournal.TriNo {
		t.Fatalf("Probe = %+v, %v; want running and busy", p, err)
	}
	cf.backingReads()
	for range 3 {
		p, err := e.Reprobe(context.Background(), v)
		if err != nil || !laneProbeStillBusy(p) {
			t.Fatalf("Reprobe = %+v, %v; want the runtime still busy", p, err)
		}
	}
	if gets, lists := cf.backingReads(); len(gets) != 0 || len(lists) != 0 {
		t.Fatalf("re-probes reached the backing: gets %q, lists %+v", gets, lists)
	}
}

// TestLaneExecBusyPassOverCacheReads proves a busy full pass over the
// controller's primed cache reads the backing only for what it reads live:
// the view's session row, the op list, the lane's runtime row, and the
// runtime row's host stamp once for the executor's caps. Resolving the
// runtime goes through the cache.
func TestLaneExecBusyPassOverCacheReads(t *testing.T) {
	cf := newLaneCachedFixture(t)
	st := cf.laneStore(t)
	l := cf.lane(t, st)
	cf.propose(t, st, cf.msg("k1", opjournal.RolePrompt, "hi"))
	runtimeRow, err := opjournal.RuntimeRowID(st.Prefix(), cf.id)
	if err != nil {
		t.Fatal(err)
	}
	cf.backingReads()
	v, info := cf.view(t)
	e := newConversationLaneExec(cf.deps(st), info)
	e.idle = func(nudgeTarget, worker.LiveObservation) bool { return false }
	res, err := l.Pass(context.Background(), v, e)
	if err != nil || !res.Busy {
		t.Fatalf("Pass = %+v, %v; want a busy pass", res, err)
	}
	gets, lists := cf.backingReads()
	if want := []string{cf.id, runtimeRow, runtimeRow}; !slices.Equal(gets, want) {
		t.Fatalf("backing gets = %q, want %q", gets, want)
	}
	if len(lists) != 1 || lists[0].Metadata["gc.op.open"] != cf.id {
		t.Fatalf("backing lists = %+v, want the conversation's op list once", lists)
	}
}

// TestLaneExecDeliverWithoutProbedHandle proves Deliver types only through
// the handle its probe resolved: with none it sends nothing, resolves
// nothing, and answers a transient not-delivered.
func TestLaneExecDeliverWithoutProbedHandle(t *testing.T) {
	f := newLaneFixture(t)
	st := f.laneStore(t)
	v, info := f.view(t)
	e := newConversationLaneExec(f.deps(st), info)
	var c laneSeamCounts
	countSeams(e, &c)
	out := e.Deliver(context.Background(), opjournal.Delivery{Key: "k", Text: "hi", Fence: laneFence(v)})
	nd, ok := out.(opjournal.NotDelivered)
	if !ok || !nd.Transient || !errors.Is(nd.Cause, errLaneNoProbedHandle) {
		t.Fatalf("Deliver = %#v, want a transient not-delivered for the missing probe handle", out)
	}
	if len(f.nudges()) != 0 || c.handles.Load() != 0 {
		t.Fatalf("nudges %d, handle resolutions %d; want none", len(f.nudges()), c.handles.Load())
	}
}
