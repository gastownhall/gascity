package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/opjournal"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// handbackNudge is the startup nudge the tests' leaves hand back.
const handbackNudge = "start working"

// wantQueuedHandbacks fails unless n copies of handbackNudge are queued for
// info's session in cityPath at now, each a user's submit delivered verbatim.
func wantQueuedHandbacks(t *testing.T, cityPath string, cfg *config.City, info session.Info, now time.Time, n int) {
	t.Helper()
	if cfg == nil {
		cfg = &config.City{}
	}
	target := resolveNudgeTargetFromSessionInfo(cityPath, cfg, info)
	pending, _, _, err := listQueuedNudgesForTarget(cityPath, target, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != n {
		t.Fatalf("pending %+v; want %d startup handback(s)", pending, n)
	}
	for _, got := range pending {
		if got.Message != handbackNudge || got.Source != nudgequeue.SourceSubmit || got.SessionID != info.ID {
			t.Fatalf("queued %+v; want the startup nudge for %s as a user's submit", got, info.ID)
		}
	}
}

// Kills a start whose startup nudge its program held back behind a dialog
// (design §10.2 row A20, program_status = "on") and that drops the nudge,
// queues one the leaf never handed back, or queues one from a start that
// failed: the handed-back nudge of a start that succeeded is queued for the
// session once, as a user's submit; a failed start's is taken and dropped.
func TestStartupNudgeHandbackIsQueuedForTheSession(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	info := session.Info{ID: "gc-41", SessionNameMetadata: "worker-session", Template: "worker", AgentName: "worker"}
	fake := runtime.NewFake()
	sp := runtime.FullFake{Fake: fake}
	now := time.Now()

	if err := queueStartupNudgeHandback(dir, nil, sp, info, "worker-session", "", true, now); err != nil {
		t.Fatalf("no handback: %v", err)
	}
	wantQueuedHandbacks(t, dir, nil, info, now, 0)

	fake.SetStartupNudgeHandback("worker-session", handbackNudge)
	if err := queueStartupNudgeHandback(dir, nil, sp, info, "worker-session", "", false, now); err != nil {
		t.Fatalf("failed start: %v", err)
	}
	wantQueuedHandbacks(t, dir, nil, info, now, 0)
	if _, ok := sp.TakeStartupNudgeHandback("worker-session"); ok {
		t.Fatal("a failed start's handback was left to take")
	}

	fake.SetStartupNudgeHandback("worker-session", handbackNudge)
	if err := queueStartupNudgeHandback(dir, nil, sp, info, "worker-session", "", true, now); err != nil {
		t.Fatalf("handback: %v", err)
	}
	wantQueuedHandbacks(t, dir, nil, info, now, 1)
	if err := queueStartupNudgeHandback(dir, nil, sp, info, "worker-session", "", true, now); err != nil {
		t.Fatalf("second take: %v", err)
	}
	wantQueuedHandbacks(t, dir, nil, info, now, 1)
}

// Kills a v2 launch-drift relaunch that drops the startup nudge its leaf
// handed back, or queues one from a relaunch that failed.
func TestV2RelaunchQueuesTheHandedBackStartupNudge(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	tp := TemplateParams{TemplateName: "worker", Command: "run", FPExtra: map[string]string{"pool.max": "2"}}
	probe, _ := baselineCase(t, livenessAlive, desireKeep, nil, "state", "active")
	cfg := sessionCoreConfigForHashInfo(tp, probe.w.Census.Rows[probe.k].Info)
	launchOnly := []string{"session_key", "k-1", "started_config_hash", runtime.FingerprintVersion + ":stale", "started_provision_hash", runtime.ProvisionFingerprint(cfg), "started_launch_hash", runtime.FingerprintVersion + ":old-launch"}
	for _, tc := range []struct {
		name   string
		refuse bool
		queued int
	}{{"relaunched", false, 1}, {"refused", true, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := baselineCase(t, livenessAlive, desireKeep, nil, append([]string{"state", "active"}, launchOnly...)...)
			row := c.w.Census.Rows[c.k].Info
			fake := runtime.NewFake()
			if err := fake.Start(t.Context(), "s-heal", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			if tc.refuse {
				fake.RelaunchErrors = map[string]error{"s-heal": errors.New("box gone")}
			}
			fake.SetStartupNudgeHandback("s-heal", handbackNudge)
			c.w.CityPath = t.TempDir()
			c.w.Env = &reconcileEnv{Cfg: workerCity(1)}
			caps := txCaps{start: startCaps{sp: runtime.NewFake()}, reads: effectReads{city: c.store}}
			out, err := relaunchCall(t.Context(), caps, relaunchPlan{w: c.w, row: row, tp: tp, hash: sessionCoreConfigForHashInfo(tp, row), name: "s-heal", leaf: runtime.FullFake{Fake: fake}, now: gatherNow})
			if err != nil || out.OK == tc.refuse {
				t.Fatalf("relaunch %+v (%v); want landed %v", out, err, !tc.refuse)
			}
			wantQueuedHandbacks(t, c.w.CityPath, c.w.Env.Cfg, row, gatherNow, tc.queued)
			wantNoTurnBoundToTheConfig(t, c.w.CityPath)
		})
	}
}

// wantNoTurnBoundToTheConfig fails if a turn a warm relaunch handed back is
// bound to a launch: the box publishes no token, and a warm relaunch keeps
// the box's token, never the config's, so the turn is queued untagged.
func wantNoTurnBoundToTheConfig(t *testing.T, dir string) {
	t.Helper()
	state, err := nudgequeue.LoadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range state.Pending {
		if launch, ok := nudgequeue.StartupTurnLaunch(item); ok {
			t.Fatalf("queued %+v bound to launch %q; want it untagged", item, launch)
		}
	}
}

// Kills a legacy launch-drift relaunch that drops the startup nudge its
// runtime handed back.
func TestLegacyRelaunchQueuesTheHandedBackStartupNudge(t *testing.T) {
	env, tp, sess := setupLaunchDriftResumeEnv(t)
	info := env.sessionInfo(sess.ID)
	env.sp.SetStartupNudgeHandback("worker", handbackNudge)
	b, _ := env.store.Get(sess.ID)
	stored := b.Metadata["started_config_hash"]
	current := runtime.CoreFingerprint(sessionCoreConfigForHashInfo(tp, info))
	relaunched, _ := relaunchAgentForLaunchDrift(
		context.Background(), runtime.FullFake{Fake: env.sp}, sessionFrontDoor(env.store), info, "worker",
		tp, env.city, env.cfg, env.store, dispatchOptionSources{}, stored, current, b.Metadata["started_provision_hash"], b.Metadata["started_launch_hash"],
		[]string{"Command"}, env.rec, nil, &env.stdout, &env.stderr,
	)
	if !relaunched {
		t.Fatalf("not relaunched; stderr=%s", env.stderr.String())
	}
	wantQueuedHandbacks(t, env.city, env.cfg, env.sessionInfo(sess.ID), time.Now(), 1)
	wantNoTurnBoundToTheConfig(t, env.city)
}

// handbackQueueHarness is a started session whose leaf handed back its
// startup nudge after a Start, as an exec pack's un-welded launch hands back
// every first turn, and the poller's view of it once its pane is idle.
type handbackQueueHarness struct {
	dir    string
	store  beads.NudgesStore
	fake   *runtime.Fake
	info   session.Info
	target nudgeTarget
	obs    worker.LiveObservation
	launch string // the started launch's id
}

// newHandbackQueueHarness starts a session whose Start handed nudge back,
// and queues it as every start path's caller does.
func newHandbackQueueHarness(t *testing.T, nudge runtime.StartupNudge) handbackQueueHarness {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := mgr.Start(context.Background(), testOperatorIn(t, dir), info.ID, "", runtime.Config{WorkDir: dir}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// The launch's identity, as the exec leaf publishes it: the turn is
	// bound to it, and the poller's claim and pre-send read find it live.
	var env map[string]string
	for _, call := range fake.SnapshotCalls() {
		if call.Method == "Start" && call.Name == info.SessionName {
			env = call.Config.Env
		}
	}
	if env[runtime.InstanceTokenEnv] == "" {
		t.Fatal("the session started with no instance token")
	}
	for _, key := range []string{runtime.InstanceTokenEnv, runtime.RuntimeEpochEnv, runtime.ContinuationEpochEnv} {
		if err := fake.SetMeta(info.SessionName, key, env[key]); err != nil {
			t.Fatal(err)
		}
	}
	fake.SetStartupNudgeHandbackAt(info.SessionName, nudge)
	if err := queueStartupNudgeHandback(dir, nil, runtime.FullFake{Fake: fake}, info, info.SessionName, "", true, time.Now()); err != nil {
		t.Fatalf("queueStartupNudgeHandback: %v", err)
	}
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{info.SessionName: idleSince}
	return handbackQueueHarness{
		dir:    dir,
		store:  store,
		fake:   fake,
		info:   info,
		target: nudgeTarget{cityPath: dir, agent: config.Agent{Name: "worker"}, sessionID: info.ID, resolved: &config.ResolvedProvider{Name: "codex"}, sessionName: info.SessionName},
		obs:    worker.LiveObservation{Running: true, Alive: true, LastActivity: &idleSince},
		launch: runtime.LaunchID(env[runtime.InstanceTokenEnv]),
	}
}

// pass runs one poller pass and returns whether it delivered and the texts
// the runtime was sent.
func (h handbackQueueHarness) pass(t *testing.T) (bool, []string) {
	t.Helper()
	before := len(h.fake.SnapshotCalls())
	delivered, err := tryDeliverQueuedNudgesByPoller(h.target, h.store, h.store, h.fake, 3*time.Second, h.obs)
	if err != nil {
		t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
	}
	var sent []string
	for _, call := range h.fake.SnapshotCalls()[before:] {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			sent = append(sent, call.Message)
		}
	}
	return delivered, sent
}

// wantQueueEmpty fails unless h's queue holds nothing for its session.
func (h handbackQueueHarness) wantQueueEmpty(t *testing.T) {
	t.Helper()
	pending, inFlight, dead, err := listQueuedNudgesForTarget(h.dir, h.target, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending)+len(inFlight)+len(dead) != 0 {
		t.Fatalf("queue = %d pending, %d in flight, %d dead; want empty: the handed-back turn was acked once", len(pending), len(inFlight), len(dead))
	}
}

// Kills a handed-back first turn the queue never delivers, delivers twice,
// or sends before the earliest send time its runtime gave it (an exec pack
// without an idle boundary holds the turn ready_delay_ms past the launch):
// the poller holds it until NotBefore, then sends it once, verbatim, and
// acks it.
func TestQueueDeliversHandedBackFirstTurnOnceAfterReadiness(t *testing.T) {
	notBefore := time.Now().Add(time.Second)
	h := newHandbackQueueHarness(t, runtime.StartupNudge{Text: handbackNudge, NotBefore: notBefore})
	pending, _, _, err := listQueuedNudgesForTarget(h.dir, h.target, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !pending[0].DeliverAfter.Equal(notBefore.UTC()) {
		t.Fatalf("queued %+v; want the turn once, due at its NotBefore %s", pending, notBefore)
	}
	if launch, ok := nudgequeue.StartupTurnLaunch(pending[0]); !ok || launch != h.launch {
		t.Fatalf("queued turn bound to %q (%v); want the launch the runtime reads", launch, ok)
	}

	if delivered, sent := h.pass(t); delivered || len(sent) != 0 {
		t.Fatalf("pass before NotBefore = delivered %v, sent %q; want the turn held until ready_delay_ms has passed", delivered, sent)
	}
	time.Sleep(time.Until(notBefore))
	delivered, sent := h.pass(t)
	if !delivered || len(sent) != 1 || sent[0] != handbackNudge {
		t.Fatalf("pass at NotBefore = delivered %v, sent %q; want the first turn once, verbatim", delivered, sent)
	}
	if delivered, sent := h.pass(t); delivered || len(sent) != 0 {
		t.Fatalf("pass after delivery = delivered %v, sent %q; want nothing: the turn is delivered once", delivered, sent)
	}
	h.wantQueueEmpty(t)
}

// Kills a live-only nudge that types into a running session ahead of the
// first turn its queue still holds: a mail or sling notice, which builds its
// handle as gc session nudge and the queue's pass do
// (workerHandleForNudgeTarget, the conversation lane's runtime handle
// alike), is undelivered and types nothing while the turn waits, so it
// falls back to the queue behind it. The queue's own pass still delivers the
// turn through the same handle, under its batch key, and the notice types
// once the turn is delivered.
func TestALiveOnlyNudgeWaitsBehindTheQueuedFirstTurn(t *testing.T) {
	h := newHandbackQueueHarness(t, runtime.StartupNudge{Text: handbackNudge})
	notice := func(what string) bool {
		t.Helper()
		handle, err := workerHandleForNudgeTarget(h.target, h.store, h.fake)
		if err != nil {
			t.Fatalf("%s: workerHandleForNudgeTarget: %v", what, err)
		}
		before := len(h.fake.SnapshotCalls())
		result, err := handle.Nudge(context.Background(), sessionActor(session.ActorAgent, h.dir), worker.NudgeRequest{
			Text: "you have mail", Delivery: worker.NudgeDeliveryDefault, Source: "mail", Wake: worker.NudgeWakeLiveOnly,
		})
		if err != nil {
			t.Fatalf("%s: Nudge: %v", what, err)
		}
		var typed []string
		for _, call := range h.fake.SnapshotCalls()[before:] {
			if call.Method == "Nudge" || call.Method == "NudgeNow" {
				typed = append(typed, call.Message)
			}
		}
		if result.Delivered != (len(typed) == 1) {
			t.Fatalf("%s: Nudge = %+v, typed %q; want it typed exactly when delivered", what, result, typed)
		}
		return result.Delivered
	}
	if notice("behind the queued first turn") {
		t.Fatal("a live-only notice typed ahead of the session's queued first turn")
	}
	delivered, sent := h.pass(t)
	if !delivered || len(sent) != 1 || sent[0] != handbackNudge {
		t.Fatalf("pass = delivered %v, sent %q; want the queue's delivery of the first turn, not held by its own turn", delivered, sent)
	}
	h.wantQueueEmpty(t)
	if !notice("after the first turn") {
		t.Fatal("a live-only notice was held after the first turn was delivered")
	}
}

// laneDeliveryRecorder is a lane executor that records the result of each
// delivery it makes.
type laneDeliveryRecorder struct {
	*conversationLaneExec
	results *[]opjournal.DeliveryResult
}

func (e laneDeliveryRecorder) Deliver(ctx context.Context, d opjournal.Delivery) opjournal.DeliveryResult {
	res := e.conversationLaneExec.Deliver(ctx, d)
	*e.results = append(*e.results, res)
	return res
}

// Kills a conversation-lane message that types ahead of the first turn its
// session's queue still holds. The lane builds its runtime handle in its own
// executor (conversationLaneExec.handle), not through
// workerHandleForNudgeTarget, and a handle built there without the
// session's id asks no startup-turn barrier. While the turn is pending, and
// while the queue delivers it (in flight, pinned to its claim's batch key,
// which the lane's own delivery key never matches), the lane's delivery is
// a transient NotDelivered and types nothing; once the queue has acked the
// turn, the lane delivers the message.
func TestLaneMessageWaitsBehindTheQueuedFirstTurn(t *testing.T) {
	f := newLaneFixture(t)
	info := f.rowInfo(t)
	f.sp.SetStartupNudgeHandbackAt(f.name, runtime.StartupNudge{Text: handbackNudge})
	if err := queueStartupNudgeHandback(f.dir, f.cfg, runtime.FullFake{Fake: f.sp}, info, f.name, f.token, true, time.Now()); err != nil {
		t.Fatalf("queueStartupNudgeHandback: %v", err)
	}
	pending, _, _, err := listQueuedNudgesForTarget(f.dir, resolveNudgeTargetFromSessionInfo(f.dir, f.cfg, info), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || !nudgequeue.IsStartupTurn(pending[0]) {
		t.Fatalf("queued %+v; want the session's first turn, bound to its launch", pending)
	}
	st := f.laneStore(t)
	l := f.lane(t, st)
	op := f.propose(t, st, f.msg("k1", opjournal.RolePrompt, "please run the tests"))
	var results []opjournal.DeliveryResult
	pass := func(when string) opjournal.DeliveryResult {
		t.Helper()
		if retry := f.get(t, st, op.ID).RetryAt; retry.After(f.clk.Now()) {
			f.clk.Advance(retry.Sub(f.clk.Now()) + time.Second)
		}
		before := len(results)
		if _, err := f.pass(context.Background(), t, l, st, func(e *conversationLaneExec) opjournal.Executor {
			return laneDeliveryRecorder{conversationLaneExec: e, results: &results}
		}); err != nil {
			t.Fatalf("%s: Pass: %v", when, err)
		}
		if len(results) != before+1 {
			t.Fatalf("%s: the pass made %d deliveries, want 1", when, len(results)-before)
		}
		return results[len(results)-1]
	}
	held := func(when string) {
		t.Helper()
		res := pass(when)
		if nd, ok := res.(opjournal.NotDelivered); !ok || !nd.Transient {
			t.Fatalf("%s: lane delivery = %#v, want a transient NotDelivered behind the first turn", when, res)
		}
		if calls := f.nudges(); len(calls) != 0 {
			t.Fatalf("%s: runtime nudges = %+v, want none: the lane typed ahead of the first turn", when, calls)
		}
	}
	held("with the first turn pending")

	const batch = "nudge-batch-first-turn"
	if err := nudgequeue.WithState(f.dir, func(state *nudgequeue.State) error {
		for _, item := range state.Pending {
			item.DeliveryBatch, item.ClaimedAt, item.LeaseUntil = batch, time.Now().UTC(), time.Now().Add(time.Minute)
			state.InFlight = append(state.InFlight, item)
		}
		state.Pending = nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	held("with the queue delivering the first turn")

	if err := nudgequeue.WithState(f.dir, func(state *nudgequeue.State) error {
		state.InFlight = nil // the queue acked its delivery
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	res := pass("after the first turn")
	if _, ok := res.(opjournal.Delivered); !ok {
		t.Fatalf("lane delivery after the first turn = %#v, want delivered", res)
	}
	if calls := f.nudges(); len(calls) != 1 || calls[0].Message != "please run the tests" {
		t.Fatalf("runtime nudges after the first turn = %+v, want the lane's message once", calls)
	}
}

// Kills a handed-back first turn that a runtime refusal drops, dead-letters
// or acks as delivered: the queue releases a turn the runtime refused
// (pending interaction) and a later pass delivers it once.
func TestQueueRetriesRefusedHandedBackFirstTurn(t *testing.T) {
	h := newHandbackQueueHarness(t, runtime.StartupNudge{Text: handbackNudge})
	h.fake.NudgeErrors = map[string]error{h.info.SessionName: fmt.Errorf("%w: a dialog is up", runtime.ErrNudgeRefusedPendingInteraction)}
	if delivered, _ := h.pass(t); delivered {
		t.Fatal("a refused first turn was acked as delivered")
	}
	h.fake.NudgeErrors = nil
	if delivered, sent := h.pass(t); delivered || len(sent) != 0 {
		t.Fatalf("pass right after the refusal = delivered %v, sent %q; want nothing until the retry delay runs out", delivered, sent)
	}
	pending, _, dead, err := listQueuedNudgesForTarget(h.dir, h.target, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || len(dead) != 0 || pending[0].Attempts != 0 || !pending[0].DeliverAfter.After(time.Now()) {
		t.Fatalf("after the refusal: pending %+v, dead %d; want the turn pending, uncharged, due after the retry delay", pending, len(dead))
	}
	makeQueuedNudgesDue(t, h.dir)
	delivered, sent := h.pass(t)
	if !delivered || len(sent) != 1 || sent[0] != handbackNudge {
		t.Fatalf("retry = delivered %v, sent %q; want the first turn delivered once", delivered, sent)
	}
	if delivered, sent := h.pass(t); delivered || len(sent) != 0 {
		t.Fatalf("pass after delivery = delivered %v, sent %q; want nothing", delivered, sent)
	}
	h.wantQueueEmpty(t)
}
