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
	delivered, sent := h.pass(t)
	if !delivered || len(sent) != 1 || sent[0] != handbackNudge {
		t.Fatalf("retry = delivered %v, sent %q; want the first turn delivered once", delivered, sent)
	}
	if delivered, sent := h.pass(t); delivered || len(sent) != 0 {
		t.Fatalf("pass after delivery = delivered %v, sent %q; want nothing", delivered, sent)
	}
	h.wantQueueEmpty(t)
}
