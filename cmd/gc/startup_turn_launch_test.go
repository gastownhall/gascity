package main

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

// launchHarness is a started session whose runtime publishes its launch
// environment (instance token, generation, continuation epoch) as session
// metadata, as tmux does, and the poller's view of it once its pane is idle.
type launchHarness struct {
	dir    string
	store  beads.NudgesStore
	fake   *runtime.Fake
	sp     runtime.Provider
	info   session.Info
	target nudgeTarget
	obs    worker.LiveObservation
}

func newLaunchHarness(t *testing.T) launchHarness {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	stubStartupTurnPoller(t)
	dir := t.TempDir()
	store := openNudgeBeadStore(dir)
	fake := runtime.NewFake()
	mgr := newSessionManagerWithConfig(dir, store.Store, fake, nil)
	info, err := mgr.CreateSession(context.Background(), session.CreateOptions{Template: "worker", Title: "Worker", Command: "codex", WorkDir: dir, Provider: "codex", Hints: runtime.Config{WorkDir: dir}, ExtraMeta: map[string]string{"session_origin": "manual"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	idleSince := time.Now().Add(-10 * time.Second)
	fake.Activity = map[string]time.Time{info.SessionName: idleSince}
	h := launchHarness{
		dir: dir, store: store, fake: fake, sp: runtime.FullFake{Fake: fake}, info: info,
		target: nudgeTarget{cityPath: dir, agent: config.Agent{Name: "worker"}, sessionID: info.ID, resolved: &config.ResolvedProvider{Name: "codex"}, sessionName: info.SessionName},
		obs:    worker.LiveObservation{Running: true, Alive: true, LastActivity: &idleSince},
	}
	h.publish(t, h.startEnv(t))
	return h
}

// stubStartupTurnPoller keeps a handback from starting a real poller.
func stubStartupTurnPoller(t *testing.T) *[]string {
	t.Helper()
	var started []string
	prev := startNudgePoller
	startNudgePoller = func(_, _, sessionName string) error {
		started = append(started, sessionName)
		return nil
	}
	t.Cleanup(func() { startNudgePoller = prev })
	return &started
}

// startEnv is the environment the session's last Start carried.
func (h launchHarness) startEnv(t *testing.T) map[string]string {
	t.Helper()
	var env map[string]string
	for _, call := range h.fake.SnapshotCalls() {
		if call.Method == "Start" && call.Name == h.info.SessionName {
			env = call.Config.Env
		}
	}
	if env[runtime.InstanceTokenEnv] == "" {
		t.Fatalf("the session started with no instance token")
	}
	return env
}

// publish sets the launch metadata env names, as the pane's environment.
func (h launchHarness) publish(t *testing.T, env map[string]string) {
	t.Helper()
	for _, key := range []string{runtime.InstanceTokenEnv, runtime.RuntimeEpochEnv, runtime.ContinuationEpochEnv} {
		if err := h.fake.SetMeta(h.info.SessionName, key, env[key]); err != nil {
			t.Fatal(err)
		}
	}
}

// relaunch replaces the session's process with one launched under token.
func (h launchHarness) relaunch(t *testing.T, token string) {
	t.Helper()
	env := map[string]string{}
	for key, value := range h.startEnv(t) {
		env[key] = value
	}
	env[runtime.InstanceTokenEnv] = token
	if err := h.fake.Stop(h.info.SessionName); err != nil {
		t.Fatal(err)
	}
	if err := h.fake.Start(context.Background(), h.info.SessionName, runtime.Config{Env: env}); err != nil {
		t.Fatal(err)
	}
	h.publish(t, env)
}

// handBack queues text as the startup nudge the live launch handed back.
func (h launchHarness) handBack(t *testing.T, text string) {
	t.Helper()
	h.fake.SetStartupNudgeHandback(h.info.SessionName, text)
	if err := queueStartupNudgeHandback(h.dir, nil, h.sp, h.info, h.info.SessionName, "", true, time.Now()); err != nil {
		t.Fatalf("queueStartupNudgeHandback: %v", err)
	}
}

// pass runs one poller pass and returns whether it delivered and the nudge
// calls the runtime took.
func (h launchHarness) pass(t *testing.T, sp runtime.Provider) (bool, []runtime.Call) {
	t.Helper()
	before := len(h.fake.SnapshotCalls())
	delivered, err := tryDeliverQueuedNudgesByPoller(h.target, h.store.Store, h.store.Store, sp, 3*time.Second, h.obs)
	if err != nil {
		t.Fatalf("tryDeliverQueuedNudgesByPoller: %v", err)
	}
	var sent []runtime.Call
	for _, call := range h.fake.SnapshotCalls()[before:] {
		if call.Method == "Nudge" || call.Method == "NudgeNow" {
			sent = append(sent, call)
		}
	}
	return delivered, sent
}

// state is the queue as stored.
func (h launchHarness) state(t *testing.T) nudgequeue.State {
	t.Helper()
	state, err := nudgequeue.LoadState(h.dir)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// deadReason is id's LastError in the dead bucket, or "" when it is not dead.
func (h launchHarness) deadReason(t *testing.T, id string) string {
	t.Helper()
	for _, item := range h.state(t).Dead {
		if item.ID == id {
			return item.LastError
		}
	}
	return ""
}

// turnID is the id of the queued startup turn carrying text.
func (h launchHarness) turnID(t *testing.T, text string) string {
	t.Helper()
	state := h.state(t)
	for _, bucket := range [][]nudgequeue.Item{state.Pending, state.InFlight, state.Dead} {
		for _, item := range bucket {
			if item.Message == text {
				return item.ID
			}
		}
	}
	t.Fatalf("no queued item carries %q", text)
	return ""
}

// Kills the duplicate first turn: a turn its launch claimed and released is
// typed into the launch that replaced it, beside that launch's own turn. One
// pass types only the replacement's turn, the released one is dead as
// stale-launch, and further passes type nothing.
func TestAReplacedLaunchsReleasedTurnIsNeverTyped(t *testing.T) {
	h := newLaunchHarness(t)
	h.handBack(t, "first launch's turn")
	t1 := h.turnID(t, "first launch's turn")
	claimed, err := claimDueQueuedNudgeBatchForTarget(h.dir, h.target, nudgeLaunchReader(h.target, h.sp), time.Now())
	if err != nil || len(claimed) != 1 || claimed[0].ID != t1 {
		t.Fatalf("claim = %v, %v; want the first launch's turn", queuedNudgeIDs(claimed), err)
	}
	if err := releaseQueuedNudgeClaims(h.dir, []string{t1}); err != nil {
		t.Fatal(err)
	}
	h.relaunch(t, "tok-second-launch")
	h.handBack(t, "second launch's turn")
	delivered, sent := h.pass(t, h.sp)
	if !delivered || len(sent) != 1 || sent[0].Message != "second launch's turn" {
		t.Fatalf("pass = delivered %v, sent %+v; want only the live launch's turn", delivered, sent)
	}
	if reason := h.deadReason(t, t1); reason != nudgequeue.StaleLaunchReason {
		t.Fatalf("the replaced launch's turn is dead with %q, want %q", reason, nudgequeue.StaleLaunchReason)
	}
	for range 2 {
		if delivered, sent := h.pass(t, h.sp); delivered || len(sent) != 0 {
			t.Fatalf("a later pass = delivered %v, sent %+v; want nothing", delivered, sent)
		}
	}
}

// Kills a busy session's closed quiescence gate keeping a stale turn alive:
// a claim that would only dead-letter it types nothing, so the pass runs it
// past the gate, and the turn stops reading as due work on every tick.
func TestABusySessionStillDropsAStaleTurn(t *testing.T) {
	h := newLaunchHarness(t)
	h.handBack(t, "first launch's turn")
	t1 := h.turnID(t, "first launch's turn")
	h.relaunch(t, "tok-second-launch")
	busy := time.Now()
	h.fake.Activity = map[string]time.Time{h.info.SessionName: busy}
	h.obs.LastActivity = &busy
	if delivered, sent := h.pass(t, h.sp); delivered || len(sent) != 0 {
		t.Fatalf("pass = delivered %v, sent %+v; want nothing typed into the busy session", delivered, sent)
	}
	if reason := h.deadReason(t, t1); reason != nudgequeue.StaleLaunchReason {
		t.Fatalf("the replaced launch's turn is dead with %q, want %q", reason, nudgequeue.StaleLaunchReason)
	}
}

// Kills an unfenced startup turn send, a send into a launch the pre-send read
// did not prove, and a fence miss that retries: the send carries the live
// fence; a pre-send read that shows another launch hands the turn back
// unsent and uncharged; a miss dead-letters it as stale-launch.
func TestAStartupTurnIsSentFencedToItsLaunch(t *testing.T) {
	t.Run("fenced send", func(t *testing.T) {
		h := newLaunchHarness(t)
		h.handBack(t, "the turn")
		fence, ok := runtime.ReadInputFence(h.sp, h.info.SessionName)
		if !ok {
			t.Fatal("no live fence")
		}
		delivered, sent := h.pass(t, h.sp)
		if !delivered || len(sent) != 1 || sent[0].Fence.Digest() != fence.Digest() {
			t.Fatalf("pass = delivered %v, sent %+v; want the turn sent under the live fence %s", delivered, sent, fence)
		}
	})
	t.Run("a pre-send read of another launch", func(t *testing.T) {
		h := newLaunchHarness(t)
		h.handBack(t, "the turn")
		id := h.turnID(t, "the turn")
		flip := &tokenFlipProvider{Provider: h.sp, name: h.info.SessionName, after: 1, token: "tok-replacement"}
		delivered, sent := h.pass(t, flip)
		if delivered || len(sent) != 0 {
			t.Fatalf("pass = delivered %v, sent %+v; want nothing typed", delivered, sent)
		}
		state := h.state(t)
		if len(state.Pending) != 1 || state.Pending[0].ID != id || state.Pending[0].Attempts != 0 {
			t.Fatalf("pending = %+v; want the turn handed back uncharged", state.Pending)
		}
	})
	t.Run("a fence miss", func(t *testing.T) {
		h := newLaunchHarness(t)
		h.handBack(t, "the turn")
		id := h.turnID(t, "the turn")
		// The runtime reports the turn's launch, but another process runs
		// in its place: the send's own check misses.
		env := h.startEnv(t)
		h.relaunch(t, "tok-replacement")
		h.publish(t, env)
		if delivered, sent := h.pass(t, h.sp); delivered || len(sent) != 0 {
			t.Fatalf("pass = delivered %v, sent %+v; want the miss to type nothing", delivered, sent)
		}
		if reason := h.deadReason(t, id); reason != nudgequeue.StaleLaunchReason {
			t.Fatalf("the missed turn is dead with %q, want %q", reason, nudgequeue.StaleLaunchReason)
		}
	})
	t.Run("an unproven miss", func(t *testing.T) {
		h := newLaunchHarness(t)
		h.handBack(t, "the turn")
		id := h.turnID(t, "the turn")
		// The runtime reports the turn's launch, but its send-time check
		// cannot tell which incarnation runs: the miss proves nothing.
		env := h.startEnv(t)
		unproven := map[string]string{}
		for key, value := range env {
			if key != runtime.InstanceTokenEnv {
				unproven[key] = value
			}
		}
		if err := h.fake.Stop(h.info.SessionName); err != nil {
			t.Fatal(err)
		}
		if err := h.fake.Start(context.Background(), h.info.SessionName, runtime.Config{Env: unproven}); err != nil {
			t.Fatal(err)
		}
		h.publish(t, env)
		before := time.Now()
		if delivered, sent := h.pass(t, h.sp); delivered || len(sent) != 0 {
			t.Fatalf("pass = delivered %v, sent %+v; want the miss to type nothing", delivered, sent)
		}
		if reason := h.deadReason(t, id); reason != "" {
			t.Fatalf("the unproven miss dead-lettered the turn with %q; want it handed back", reason)
		}
		state := h.state(t)
		if len(state.Pending) != 1 || state.Pending[0].ID != id || state.Pending[0].Attempts != 0 || !state.Pending[0].DeliverAfter.After(before) {
			t.Fatalf("pending = %+v; want the turn handed back uncharged, after the retry delay", state.Pending)
		}
	})
}

// tokenFlipProvider reports token as name's instance token once its token has
// been read after times.
type tokenFlipProvider struct {
	runtime.Provider
	name  string
	after int
	token string
	mu    sync.Mutex
	reads int
}

func (p *tokenFlipProvider) GetMeta(name, key string) (string, error) {
	if name == p.name && key == runtime.InstanceTokenEnv {
		p.mu.Lock()
		p.reads++
		flipped := p.reads > p.after
		p.mu.Unlock()
		if flipped {
			return p.token, nil
		}
	}
	return p.Provider.GetMeta(name, key)
}

// Kills a binding that trusts the start config over the runtime, a turn lost
// when no token is known, a one-shot initial message carried in a droppable
// turn, and a handback no poller delivers: the turn binds to the live token,
// else the start's, else none; the initial message is split off; the wake
// socket is pinged; and only a legacy dispatcher gets a poller.
func TestStartupNudgeHandbackBindsSplitsAndWakes(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	info := session.Info{ID: "gc-41", SessionNameMetadata: "worker-session", Template: "worker", AgentName: "worker"}
	queued := func(t *testing.T, dir string) []nudgequeue.Item {
		t.Helper()
		state, err := nudgequeue.LoadState(dir)
		if err != nil {
			t.Fatal(err)
		}
		return state.Pending
	}
	for _, tc := range []struct {
		name, live, start, want string
	}{
		{"the live token", "tok-live", "tok-start", runtime.LaunchID("tok-live")},
		{"the start's token", "", "tok-start", runtime.LaunchID("tok-start")},
		{"no token", "", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubStartupTurnPoller(t)
			dir := t.TempDir()
			fake := runtime.NewFake()
			if tc.live != "" {
				if err := fake.SetMeta("worker-session", runtime.InstanceTokenEnv, tc.live); err != nil {
					t.Fatal(err)
				}
			}
			fake.SetStartupNudgeHandback("worker-session", handbackNudge)
			if err := queueStartupNudgeHandback(dir, nil, runtime.FullFake{Fake: fake}, info, "worker-session", tc.start, true, time.Now()); err != nil {
				t.Fatal(err)
			}
			items := queued(t, dir)
			if len(items) != 1 || items[0].Message != handbackNudge {
				t.Fatalf("queued %+v; want the turn", items)
			}
			launch, tagged := nudgequeue.StartupTurnLaunch(items[0])
			if launch != tc.want || tagged != (tc.want != "") {
				t.Fatalf("turn bound to %q (tagged %v), want %q", launch, tagged, tc.want)
			}
		})
	}
	t.Run("the initial message is split off", func(t *testing.T) {
		stubStartupTurnPoller(t)
		dir := t.TempDir()
		withMessage := info
		withMessage.TemplateOverrides = `{"initial_message":"hello"}`
		fake := runtime.NewFake()
		fake.SetStartupNudgeHandback("worker-session", session.AppendInitialMessage(handbackNudge, "hello"))
		if err := queueStartupNudgeHandback(dir, nil, runtime.FullFake{Fake: fake}, withMessage, "worker-session", "tok-start", true, time.Now()); err != nil {
			t.Fatal(err)
		}
		items := queued(t, dir)
		if len(items) != 2 || items[0].Message != handbackNudge || !nudgequeue.IsStartupTurn(items[0]) || items[1].Message != "hello" || nudgequeue.IsStartupTurn(items[1]) {
			t.Fatalf("queued %+v; want the turn, then the initial message unbound", items)
		}
	})
	for _, tc := range []struct {
		mode    string
		pollers int
	}{{"legacy", 1}, {"supervisor", 0}} {
		t.Run("the wake socket and the "+tc.mode+" poller", func(t *testing.T) {
			started := stubStartupTurnPoller(t)
			var pinged []string
			prevPing := pingNudgeWakeSocket
			pingNudgeWakeSocket = func(cityPath string) { pinged = append(pinged, cityPath) }
			t.Cleanup(func() { pingNudgeWakeSocket = prevPing })
			dir := t.TempDir()
			cfg := &config.City{Daemon: config.DaemonConfig{NudgeDispatcher: tc.mode}}
			fake := runtime.NewFake()
			fake.SetStartupNudgeHandback("worker-session", handbackNudge)
			if err := queueStartupNudgeHandback(dir, cfg, runtime.FullFake{Fake: fake}, info, "worker-session", "", true, time.Now()); err != nil {
				t.Fatal(err)
			}
			if len(pinged) != 1 || pinged[0] != dir {
				t.Fatalf("wake pings %v; want one for the city", pinged)
			}
			if len(*started) != tc.pollers || (tc.pollers == 1 && (*started)[0] != "worker-session") {
				t.Fatalf("pollers started for %v; want %d in %s dispatcher mode", *started, tc.pollers, tc.mode)
			}
		})
	}
}

// Kills a dispatcher that observes a session whose items wait behind a turn
// not yet due, or skips the pass that would drop a stale turn: DueKeys leaves
// the held key out, and the preview answers yes for a pending stale drop.
func TestDispatcherJudgesStartupTurns(t *testing.T) {
	h := newLaunchHarness(t)
	agent := h.target.agentKey()
	reminder := newQueuedNudgeWithOptions(agent, "check your hook", "sling", time.Now().Add(-time.Minute), queuedNudgeOptionsFromTarget(h.target))
	if err := openNudgeQueue(h.dir, beads.NudgesStore{}).Queue.EnqueueDeferred(reminder); err != nil {
		t.Fatal(err)
	}
	turn := newQueuedNudgeWithOptions(agent, "the turn", nudgequeue.SourceSubmit, time.Now(), queuedNudgeOptionsFromTarget(h.target))
	turn.Reference = nudgequeue.StartupTurnReference(runtime.LaunchID("tok-gone"))
	turn.DeliverAfter = time.Now().Add(time.Hour)
	if err := openNudgeQueue(h.dir, beads.NudgesStore{}).Queue.EnqueueDeferred(turn); err != nil {
		t.Fatal(err)
	}
	queue := openNudgeQueue(h.dir, beads.NudgesStore{})
	keys, err := queue.Queue.DueKeys(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(keys, agent) {
		t.Fatalf("DueKeys = %v; the session's reminder waits behind a turn not yet due", keys)
	}
	census := func() (map[string]struct{}, bool) { return map[string]struct{}{h.info.ID: {}}, true }
	due := time.Now().Add(2 * time.Hour)
	if keys, _ := queue.Queue.DueKeys(due); !slices.Contains(keys, agent) {
		t.Fatalf("DueKeys once the turn is due = %v; want the session's key", keys)
	}
	if !nudgeTargetMayClaimPerSnapshot(queue, h.target, census, nudgeLaunchReader(h.target, h.sp), due) {
		t.Fatal("the preview answered no for a claim that drops a stale turn and frees the reminder")
	}
	claimed, err := claimDueQueuedNudgeBatchForTarget(h.dir, h.target, nudgeLaunchReader(h.target, h.sp), due)
	if err != nil || len(claimed) != 1 || claimed[0].ID != reminder.ID {
		t.Fatalf("claim = %v, %v; want the reminder the stale turn held", queuedNudgeIDs(claimed), err)
	}
	if reason := h.deadReason(t, turn.ID); reason != nudgequeue.StaleLaunchReason {
		t.Fatalf("the stale turn is dead with %q, want %q", reason, nudgequeue.StaleLaunchReason)
	}
}

// Kills a poll gate that reads the runtime on every idle tick: while every
// startup turn is not yet due, the gate makes no GetMeta call for the token.
func TestPollGateReadsNoLaunchWhileNoTurnIsDue(t *testing.T) {
	h := newLaunchHarness(t)
	turn := newQueuedNudgeWithOptions(h.target.agentKey(), "the turn", nudgequeue.SourceSubmit, time.Now(), queuedNudgeOptionsFromTarget(h.target))
	turn.Reference = nudgequeue.StartupTurnReference(runtime.LaunchID("tok-any"))
	turn.DeliverAfter = time.Now().Add(time.Hour)
	if err := openNudgeQueue(h.dir, beads.NudgesStore{}).Queue.EnqueueDeferred(turn); err != nil {
		t.Fatal(err)
	}
	before := len(h.fake.SnapshotCalls())
	if newNudgePollGate().hasDue(h.target, nudgeLaunchReader(h.target, h.sp), time.Now()) {
		t.Fatal("the gate reports due work behind a turn not yet due")
	}
	for _, call := range h.fake.SnapshotCalls()[before:] {
		if call.Method == "GetMeta" && call.Key == runtime.InstanceTokenEnv {
			t.Fatalf("the gate read the launch with no turn due: %+v", call)
		}
	}
}

// Kills a drain that drops a startup turn on the strength of its own
// GC_INSTANCE_TOKEN: a hook of a replaced launch would kill the
// replacement's turn. The drain's claim supplies no launch, so it holds the
// session's turns and the reminder behind them, and drops nothing.
func TestADrainNeverDropsAStartupTurn(t *testing.T) {
	h := newLaunchHarness(t)
	t.Setenv(runtime.InstanceTokenEnv, "tok-replaced-launch")
	agent := h.target.agentKey()
	turn := newQueuedNudgeWithOptions(agent, "the replacement's turn", nudgequeue.SourceSubmit, time.Now().Add(-time.Minute), queuedNudgeOptionsFromTarget(h.target))
	turn.Reference = nudgequeue.StartupTurnReference(runtime.LaunchID("tok-replacement"))
	reminder := newQueuedNudgeWithOptions(agent, "a reminder", "sling", time.Now().Add(-time.Minute), queuedNudgeOptionsFromTarget(h.target))
	for _, item := range []queuedNudge{turn, reminder} {
		if err := openNudgeQueue(h.dir, beads.NudgesStore{}).Queue.EnqueueDeferred(item); err != nil {
			t.Fatal(err)
		}
	}
	claimed, err := claimDueQueuedNudgesForTarget(h.dir, h.target, nil, time.Now())
	if err != nil || len(claimed) != 0 {
		t.Fatalf("drain claim = %v, %v; want the session held", queuedNudgeIDs(claimed), err)
	}
	if dead := h.state(t).Dead; len(dead) != 0 {
		t.Fatalf("dead = %+v; want nothing dropped", dead)
	}
}

// Kills a file queue that asks the live launch while it holds the queue's
// flock: the reader takes the lock without waiting, which fails inside it.
func TestFileQueueAsksTheLaunchOutsideTheLock(t *testing.T) {
	h := newLaunchHarness(t)
	h.handBack(t, "the turn")
	asked := 0
	reader := func() (string, bool) {
		asked++
		if err := nudgequeue.WithStateWithin(h.dir, "probe", 0, func(*nudgequeue.State) error { return nil }); err != nil {
			t.Errorf("the reader could not take the queue lock: %v", err)
		}
		return runtime.ReadLaunchID(h.sp, h.info.SessionName)
	}
	for _, claim := range []func(string, nudgeTarget, func() (string, bool), time.Time) ([]queuedNudge, error){claimDueQueuedNudgesForTarget, claimDueQueuedNudgeBatchForTarget} {
		claimed, err := claim(h.dir, h.target, reader, time.Now())
		if err != nil || len(claimed) != 1 {
			t.Fatalf("claim = %v, %v; want the turn", queuedNudgeIDs(claimed), err)
		}
		if err := releaseQueuedNudgeClaims(h.dir, queuedNudgeIDs(claimed)); err != nil {
			t.Fatal(err)
		}
	}
	if asked != 2 {
		t.Fatalf("asked %d times by two claims, want 2", asked)
	}
}
