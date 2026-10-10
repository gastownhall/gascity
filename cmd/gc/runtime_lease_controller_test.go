package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// TestLockRuntimeNameTakesTheNameFlock: a v2 effect's name lock excludes a
// holder of the name's flock in another process.
func TestLockRuntimeNameTakesTheNameFlock(t *testing.T) {
	city := t.TempDir()
	flock, err := sessionpkg.TryRuntimeLease(nil, sessionpkg.RuntimeLeaseRequest{City: city, Name: "named-1"})
	if err != nil {
		t.Fatal(err)
	}
	w := &World{CityPath: city}
	if _, unlock, ok := lockRuntimeName(w, sessionpkg.Info{SessionName: "named-1"}); ok || unlock != nil {
		t.Fatal("lockRuntimeName ignored the name's flock")
	}
	flock.Release()
	_, unlock, ok := lockRuntimeName(w, sessionpkg.Info{SessionName: "named-1"})
	if !ok {
		t.Fatal("lockRuntimeName refused a free name")
	}
	if _, err := sessionpkg.TryRuntimeLease(nil, sessionpkg.RuntimeLeaseRequest{City: city, Name: "named-1"}); !errors.Is(err, sessionpkg.ErrRuntimeLeaseBusy) {
		t.Fatalf("flock under an effect's lock = %v, want busy", err)
	}
	unlock()
	unlock()
}

// TestControllerKillsNeverWait (M1): the controller's kill takes the runtime
// lease without waiting and reports a busy one as ErrRuntimeLeaseBusy for the
// caller to defer; an operator's kill waits, then fails retryably.
func TestControllerKillsNeverWait(t *testing.T) {
	store, bead, city := newKillPokeSession(t, "s-gc-ctl-kill")
	defer sessionpkg.SetOperatorLeaseWaitForTest(400 * time.Millisecond)()
	inner := wrapKillPokeProvider(t, &killHookProvider{})
	held, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(store), sessionpkg.RuntimeLeaseRequest{
		City: city, Name: "s-gc-ctl-kill", ID: bead.ID, TTL: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	sp, err := newSessionProvider()
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	err = controllerKillSessionRow(city, store, sp, nil, sessionInfoFromBead(mustGetBead(t, store, bead.ID)))
	if !errors.Is(err, sessionpkg.ErrRuntimeLeaseBusy) || errors.Is(err, sessionpkg.ErrSessionStarting) || time.Since(began) > 300*time.Millisecond {
		t.Fatalf("controller kill = %v after %v, want ErrRuntimeLeaseBusy at once", err, time.Since(began))
	}
	began = time.Now()
	if err := workerKillSessionTargetWithConfig(city, store, sp, nil, bead.ID); !errors.Is(err, sessionpkg.ErrSessionStarting) || time.Since(began) < 150*time.Millisecond {
		t.Fatalf("operator kill = %v after %v, want ErrSessionStarting after the wait", err, time.Since(began))
	}
	if !inner.IsRunning("s-gc-ctl-kill") {
		t.Fatal("a kill stopped the runtime under another holder's lease")
	}
}

// TestKillsRunUnderTheCitysTTL pins the worker factory's lease TTL: the
// city's startup timeout plus the margin.
func TestKillsRunUnderTheCitysTTL(t *testing.T) {
	store, bead, city := newKillPokeSession(t, "s-gc-kill-ttl")
	var ttl string
	wrapKillPokeProvider(t, &killHookProvider{beforeStop: func() { ttl = mustGetBead(t, store, bead.ID).Metadata[sessionpkg.RuntimeLeaseTTLKey] }})
	sp, err := newSessionProvider()
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Session: config.SessionConfig{StartupTimeout: "3m"}}
	if err := controllerKillSessionRow(city, store, sp, cfg, sessionInfoFromBead(mustGetBead(t, store, bead.ID))); err != nil {
		t.Fatal(err)
	}
	if ttl != "240" {
		t.Fatalf("lease TTL at the stop = %q, want 240 (3m + the margin)", ttl)
	}
}

// TestControllerStopSequencesDeferOnABusyLease (M1): the async drain-ack stop
// and the escalation take the lease once for all their kills; a busy lease
// defers them before any stop, and the escalation never reaches its
// process-table kill.
func TestControllerStopSequencesDeferOnABusyLease(t *testing.T) {
	for _, seq := range []string{"async stop", "escalation"} {
		t.Run(seq, func(t *testing.T) {
			city := t.TempDir()
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "x"}); err != nil {
				t.Fatal(err)
			}
			flock, err := sessionpkg.TryRuntimeLease(nil, sessionpkg.RuntimeLeaseRequest{City: city, Name: "worker"})
			if err != nil {
				t.Fatal(err)
			}
			defer flock.Release()
			var stderr synchronizedBuffer
			tracker := &asyncStartTracker{}
			if seq == "async stop" {
				queueDrainAckAsyncStop(city, beads.NewMemStore(), sp, &config.City{}, "sess-1", "worker", "", "", nil, tracker, nil, &stderr)
			} else {
				done, _ := tracker.startDrainAckStop("escalate:sess-1")
				queueDrainAckForcedTermination(city, beads.NewMemStore(), sp, &config.City{}, sessionpkg.Info{ID: "sess-1"}, "worker",
					"agent_acked_runtime_survived", 1, time.Now(), nil, 0, done, nil, &stderr)
			}
			if !tracker.wait(5 * time.Second) {
				t.Fatal("the stop sequence never finished")
			}
			if !strings.Contains(stderr.String(), "deferred") || sp.CountCalls("Stop", "worker") != 0 || !sp.IsRunning("worker") {
				t.Fatalf("stderr %q, stops %d; want deferred with no stop", stderr.String(), sp.CountCalls("Stop", "worker"))
			}
			if strings.Contains(stderr.String(), "terminat") {
				t.Fatalf("stderr %q: the escalation reached its process-table kill", stderr.String())
			}
		})
	}
}

// TestConfigDriftResetDefersOnABusyLease: a config-drift reset whose stop is
// refused by another holder's lease decides nothing this tick: no patch, the
// runtime and the row as they were.
func TestConfigDriftResetDefersOnABusyLease(t *testing.T) {
	env := newReconcilerTestEnv()
	session := env.createSessionBead("mayor", "mayor")
	if err := env.sp.Start(context.Background(), "mayor", runtime.Config{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	city := t.TempDir()
	held, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(env.store), sessionpkg.RuntimeLeaseRequest{City: city, Name: "mayor", ID: session.ID, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	before := mustGetBead(t, env.store, session.ID).Metadata["state"]
	patch := resetConfiguredNamedSessionForConfigDriftInfo(city, env.sessionInfo(session.ID), driftResumeSessionIDCapableTemplateParams(),
		env.store, env.sp, "mayor", true, "creating", time.Now().UTC(), &env.stderr)
	if patch != nil || !env.sp.IsRunning("mayor") || mustGetBead(t, env.store, session.ID).Metadata["state"] != before {
		t.Fatalf("drift reset under a held lease = %v (running %v); want nothing decided", patch, env.sp.IsRunning("mayor"))
	}
}

// TestVerifiedStopNeverWaits: the drain's verified stop is the controller's,
// and defers on a busy lease at once.
func TestVerifiedStopNeverWaits(t *testing.T) {
	store, bead, city := newKillPokeSession(t, "s-gc-verified")
	defer sessionpkg.SetOperatorLeaseWaitForTest(2 * time.Second)()
	wrapKillPokeProvider(t, &killHookProvider{})
	held, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(store), sessionpkg.RuntimeLeaseRequest{City: city, Name: "s-gc-verified", ID: bead.ID, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	sp, err := newSessionProvider()
	if err != nil {
		t.Fatal(err)
	}
	info, err := sessionFrontDoor(store).Get(bead.ID)
	if err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := verifiedStop(city, info, store, sp, nil); !errors.Is(err, sessionpkg.ErrRuntimeLeaseBusy) || time.Since(began) > time.Second {
		t.Fatalf("verifiedStop under a held lease = %v after %v, want busy at once", err, time.Since(began))
	}
}

// TestChatAutoSuspendNeverWaits: the tick's auto-suspend of an idle chat
// session under another holder's runtime lease is refused at once, and the
// session stays up for a later tick.
func TestChatAutoSuspendNeverWaits(t *testing.T) {
	defer sessionpkg.SetOperatorLeaseWaitForTest(2 * time.Second)()
	city := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	now := time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)
	info, err := sessionpkg.NewManagerWithOptions(store, sp).CreateSession(context.Background(), sessionpkg.CreateOptions{
		Template: "default", Title: "S1", Command: "echo s1", WorkDir: t.TempDir(), Provider: "test",
		ExtraMeta: map[string]string{"session_origin": "manual"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sp.SetActivity(info.SessionName, now.Add(-2*time.Hour))
	sp.SetAttached(info.SessionName, false)
	held, err := sessionpkg.TryRuntimeLease(sessionFrontDoor(store), sessionpkg.RuntimeLeaseRequest{City: city, Name: info.SessionName, ID: info.ID, TTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	var stdout, stderr strings.Builder
	began := time.Now()
	autoSuspendChatSessions(city, nil, store, sp, 30*time.Minute, &clock.Fake{Time: now}, &stdout, &stderr)
	if time.Since(began) > time.Second || !sp.IsRunning(info.SessionName) || !strings.Contains(stderr.String(), "busy") {
		t.Fatalf("auto-suspend under a held lease took %v (running %v, stderr %q); want refused at once", time.Since(began), sp.IsRunning(info.SessionName), stderr.String())
	}
}

// TestDeadCorpseCleanupSkipsAHeldName: the dead-corpse cleanup checks and
// stops a name only under its flock; a name another holder has (a start
// recycling the dead pane) is left for a later tick.
func TestDeadCorpseCleanupSkipsAHeldName(t *testing.T) {
	city := t.TempDir()
	sp := newDeadRuntimeArtifactProvider()
	sp.visible["dead-worker"] = true
	sp.dead["dead-worker"] = true
	snapshot := newSessionBeadSnapshot([]beads.Bead{{ID: "s1", Status: "open", Metadata: map[string]string{"session_name": "dead-worker", "template": "worker"}}})
	held, err := sessionpkg.TryRuntimeLease(nil, sessionpkg.RuntimeLeaseRequest{City: city, Name: "dead-worker"})
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	if got := cleanupDeadRuntimeSessionCorpses(city, nil, nil, nil, snapshot, nil, sp, nil, nil, &stderr); got != 0 || len(sp.stopped) != 0 {
		t.Fatalf("cleanup under a held name = %d (stopped %v), want none", got, sp.stopped)
	}
	held.Release()
	if got := cleanupDeadRuntimeSessionCorpses(city, nil, nil, nil, snapshot, nil, sp, nil, nil, &stderr); got != 1 {
		t.Fatalf("cleanup of a free name = %d, want 1; stderr %q", got, stderr.String())
	}
}

// TestTickPoolRuntimeReleaseTakesTheFlock: a tick's teardown of a bead-scoped
// pool runtime stops only under the name's flock, and a busy name holds the
// row open.
func TestTickPoolRuntimeReleaseTakesTheFlock(t *testing.T) {
	city := t.TempDir()
	sp := runtime.NewFake()
	info := sessionpkg.Info{ID: "gc-7", PoolManaged: true, Template: "worker", SessionNameMetadata: "worker-gc-7"}
	if err := sp.Start(context.Background(), "worker-gc-7", runtime.Config{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	held, err := sessionpkg.TryRuntimeLease(nil, sessionpkg.RuntimeLeaseRequest{City: city, Name: "worker-gc-7"})
	if err != nil {
		t.Fatal(err)
	}
	if releaseBeadScopedPoolRuntimeLeased(city, info, sp, io.Discard) || !sp.IsRunning("worker-gc-7") {
		t.Fatal("the tick released a pool runtime under another holder's flock")
	}
	held.Release()
	if !releaseBeadScopedPoolRuntimeLeased(city, info, sp, io.Discard) || sp.IsRunning("worker-gc-7") {
		t.Fatal("the tick did not release a free pool runtime")
	}
}

// drainAckStopPendingForTest makes row id in store drain-ack stop-pending at
// token, creating it (runtime name) when absent, and returns its generation:
// an async drain-ack stop kills only such a row (drainAckStopStillPending).
func drainAckStopPendingForTest(t *testing.T, store beads.Store, id, name, token string) string {
	t.Helper()
	patch := map[string]string{"state": string(sessionpkg.StateDraining), "state_reason": sessionpkg.DrainAckStopPendingReason, "instance_token": token}
	if b, err := store.Get(id); err == nil {
		if err := store.SetMetadataBatch(id, patch); err != nil {
			t.Fatal(err)
		}
		return b.Metadata["generation"]
	}
	patch["session_name"], patch["generation"] = name, "1"
	if mem, ok := store.(*beads.MemStore); ok {
		prev := mem.HonorExplicitIDs
		mem.HonorExplicitIDs = true
		defer func() { mem.HonorExplicitIDs = prev }()
	}
	if _, err := store.Create(beads.Bead{ID: id, Title: name, Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: patch}); err != nil {
		t.Fatal(err)
	}
	return "1"
}

// TestOperatorResumeOfAStopPendingRowIsRefused is the two-tick attach race
// (CONTRACT v5.9a): a drain-ack stop is queued on a stop-pending row; an
// operator's resume through the real Manager path before the stop's goroutine
// runs is refused retryably and writes nothing, so the queued stop still finds
// the row stop-pending and stops the runtime.
func TestOperatorResumeOfAStopPendingRowIsRefused(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	gen := drainAckStopPendingForTest(t, store, "gc-worker", "worker", "tok-1")
	mgr := newSessionManagerWithConfig(city, store, sp, nil)
	err := mgr.Start(context.Background(), "gc-worker", "resume-cmd", runtime.Config{}, sessionpkg.ResumeOperator)
	if !errors.Is(err, sessionpkg.ErrSessionStopping) {
		t.Fatalf("operator resume of a stop-pending row = %v, want ErrSessionStopping", err)
	}
	if got := mustGetBead(t, store, "gc-worker").Metadata; !isDrainAckStopPendingInfo(sessionInfoFromBead(beads.Bead{ID: "gc-worker", Metadata: got})) {
		t.Fatalf("row after the refused resume = %v, want still stop-pending", got)
	}
	var stderr synchronizedBuffer
	tracker := &asyncStartTracker{}
	queueDrainAckAsyncStop(city, store, sp, &config.City{}, "gc-worker", "worker", "tok-1", gen, nil, tracker, nil, &stderr)
	if !tracker.wait(5 * time.Second) {
		t.Fatal("the async stop never finished")
	}
	if sp.CountCalls("Stop", "worker") == 0 || sp.IsRunning("worker") {
		t.Fatalf("stops %d, running %v, stderr %q; want the queued stop to stop the runtime", sp.CountCalls("Stop", "worker"), sp.IsRunning("worker"), stderr.String())
	}
}

// TestAsyncDrainAckStopSkipsAMovedRow: a stop queued on a stop-pending row
// that a newer incarnation replaced before the goroutine took the lease
// stops nothing.
func TestAsyncDrainAckStopSkipsAMovedRow(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	gen := drainAckStopPendingForTest(t, store, "gc-worker", "worker", "tok-1")
	if err := store.SetMetadataBatch("gc-worker", map[string]string{"generation": "2", "instance_token": "tok-2"}); err != nil {
		t.Fatal(err)
	}
	var stderr synchronizedBuffer
	tracker := &asyncStartTracker{}
	queueDrainAckAsyncStop(city, store, sp, &config.City{}, "gc-worker", "worker", "tok-1", gen, nil, tracker, nil, &stderr)
	if !tracker.wait(5 * time.Second) {
		t.Fatal("the async stop never finished")
	}
	if sp.CountCalls("Stop", "worker") != 0 || !strings.Contains(stderr.String(), "no longer stop-pending") {
		t.Fatalf("stops %d, stderr %q; want the moved row spared", sp.CountCalls("Stop", "worker"), stderr.String())
	}
}

// TestControllerKillDecidesAgainUnderTheLease: a controller kill decided on
// a snapshot stops nothing once the row's hold moved (an operator's resume
// consumed or set one in between), and stops a row that did not.
func TestControllerKillDecidesAgainUnderTheLease(t *testing.T) {
	store, bead, city := newKillPokeSession(t, "s-gc-premise")
	inner := wrapKillPokeProvider(t, &killHookProvider{})
	sp, err := newSessionProvider()
	if err != nil {
		t.Fatal(err)
	}
	decided := sessionInfoFromBead(mustGetBead(t, store, bead.ID))
	setKillFixtureMetadata(t, store, bead.ID, map[string]string{"held_until": "2099-01-01T00:00:00Z", "sleep_intent": "operator-hold"})
	if err := controllerKillSessionRow(city, store, sp, nil, decided); !errors.Is(err, sessionpkg.ErrKillPremiseMoved) || !inner.IsRunning("s-gc-premise") {
		t.Fatalf("kill of a moved row = %v (running %v), want ErrKillPremiseMoved", err, inner.IsRunning("s-gc-premise"))
	}
	if err := controllerKillSessionRow(city, store, sp, nil, sessionInfoFromBead(mustGetBead(t, store, bead.ID))); err != nil || inner.IsRunning("s-gc-premise") {
		t.Fatalf("kill of an unmoved row = %v (running %v)", err, inner.IsRunning("s-gc-premise"))
	}
}

// rowMovingStopProvider moves the row once its first Stop lands, leaving
// the runtime alive: a confirm-dead re-kill must then decide again.
type rowMovingStopProvider struct {
	*runtime.Fake
	store beads.Store
	moved bool
}

func (p *rowMovingStopProvider) Stop(name string) error {
	err := p.Fake.Stop(name)
	if !p.moved {
		p.moved = true
		_ = p.store.SetMetadataBatch("gc-worker", map[string]string{"state": string(sessionpkg.StateActive), "state_reason": "creation_complete"})
	}
	return err
}

// TestAsyncDrainAckReKillDecidesAgain: each confirm-dead re-kill of the
// async drain-ack stop re-reads the row under the lease; a row that moved
// after the first kill gets no second one.
func TestAsyncDrainAckReKillDecidesAgain(t *testing.T) {
	oldTimeout, oldPoll := drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll
	drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll = 300*time.Millisecond, 10*time.Millisecond
	defer func() { drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll = oldTimeout, oldPoll }()
	city := t.TempDir()
	store := beads.NewMemStore()
	sp := &rowMovingStopProvider{Fake: runtime.NewFake(), store: store}
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	sp.StopLeavesRunning["worker"] = true
	gen := drainAckStopPendingForTest(t, store, "gc-worker", "worker", "")
	var stderr synchronizedBuffer
	tracker := &asyncStartTracker{}
	queueDrainAckAsyncStop(city, store, sp, &config.City{}, "gc-worker", "worker", "", gen, []string{"claude"}, tracker, nil, &stderr)
	if !tracker.wait(5 * time.Second) {
		t.Fatal("the async stop never finished")
	}
	if n := sp.CountCalls("Stop", "worker"); n != 1 {
		t.Fatalf("Stop calls = %d, want 1: the re-kills must see the moved row; stderr %q", n, stderr.String())
	}
}

// TestDrainAckEscalationSparesAMovedRow: an escalation queued on a
// stop-pending row that an operator resumed meanwhile neither stops it nor
// reaches its process-table kill.
func TestDrainAckEscalationSparesAMovedRow(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	gen := drainAckStopPendingForTest(t, store, "sess-1", "worker", "")
	if err := store.SetMetadataBatch("sess-1", map[string]string{"state": string(sessionpkg.StateActive)}); err != nil {
		t.Fatal(err)
	}
	var stderr synchronizedBuffer
	tracker := &asyncStartTracker{}
	done, _ := tracker.startDrainAckStop("escalate:sess-1")
	queueDrainAckForcedTermination(city, store, sp, &config.City{}, sessionpkg.Info{ID: "sess-1", Generation: gen}, "worker",
		"agent_acked_runtime_survived", 1, time.Now(), nil, 0, done, nil, &stderr)
	if !tracker.wait(5 * time.Second) {
		t.Fatal("the escalation never finished")
	}
	if sp.CountCalls("Stop", "worker") != 0 || !strings.Contains(stderr.String(), "no longer stop-pending") {
		t.Fatalf("stops %d, stderr %q; want the moved row spared", sp.CountCalls("Stop", "worker"), stderr.String())
	}
}

// TestDrainAckEscalationRechecksBeforeItsProcessKill: a row that moved while
// the escalation's ordinary stop and confirm loop ran gets no process-table
// kill.
func TestDrainAckEscalationRechecksBeforeItsProcessKill(t *testing.T) {
	oldTimeout, oldPoll := drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll
	drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll = 100*time.Millisecond, 10*time.Millisecond
	defer func() { drainAckStopConfirmDeadTimeout, drainAckStopConfirmDeadPoll = oldTimeout, oldPoll }()
	city := t.TempDir()
	store := beads.NewMemStore()
	sp := &rowMovingStopProvider{Fake: runtime.NewFake(), store: store}
	if err := sp.Start(context.Background(), "worker", runtime.Config{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	sp.StopLeavesRunning["worker"] = true
	gen := drainAckStopPendingForTest(t, store, "gc-worker", "worker", "")
	var stderr synchronizedBuffer
	tracker := &asyncStartTracker{}
	done, _ := tracker.startDrainAckStop("escalate:gc-worker")
	queueDrainAckForcedTermination(city, store, sp, &config.City{}, sessionpkg.Info{ID: "gc-worker", Generation: gen}, "worker",
		"agent_acked_runtime_survived", 1, time.Now(), []string{"claude"}, 0, done, nil, &stderr)
	if !tracker.wait(5 * time.Second) {
		t.Fatal("the escalation never finished")
	}
	if !strings.Contains(stderr.String(), "skipped its process kill") {
		t.Fatalf("stderr %q; want the process-table kill skipped for the moved row", stderr.String())
	}
}

// TestControllerStopsWorkWithoutTheTestOptOut drives the controller's stop
// paths with this package's opt-out off (session.RefuseManagersWithoutCityForTest),
// as production runs them: a path that lost its city path would fail with
// ErrRuntimeLeaseNoCity and leave its runtime running.
func TestControllerStopsWorkWithoutTheTestOptOut(t *testing.T) {
	defer sessionpkg.RefuseManagersWithoutCityForTest()()
	active := func(t *testing.T, env *reconcilerTestEnv, cfg *config.City) beads.Bead {
		t.Helper()
		env.cfg = cfg
		env.addDesired("worker", "worker", true)
		b := env.createSessionBead("worker", "worker")
		env.markSessionActive(&b)
		if err := env.sp.SetMeta("worker", "GC_SESSION_ID", b.ID); err != nil {
			t.Fatal(err)
		}
		return b
	}
	stopped := func(t *testing.T, env *reconcilerTestEnv) {
		t.Helper()
		if env.sp.IsRunning("worker") || strings.Contains(env.stderr.String(), sessionpkg.ErrRuntimeLeaseNoCity.Error()) {
			t.Fatalf("running %v, stderr %q; want the runtime stopped under the city's lease", env.sp.IsRunning("worker"), env.stderr.String())
		}
	}
	reconcileAt := func(env *reconcilerTestEnv, city string, b beads.Bead, it idleTracker, opts ...startExecutionOption) {
		cfgNames := configuredSessionNames(env.cfg, "", env.store)
		reconcileSessionBeadsTraced(context.Background(), city, []beads.Bead{b}, env.desiredState, cfgNames, env.cfg, env.sp,
			env.store, nil, nil, nil, nil, env.dt, map[string]int{}, false, nil, "", it, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, nil, opts...)
	}

	t.Run("idle", func(t *testing.T) {
		env := newReconcilerTestEnv()
		b := active(t, env, &config.City{Agents: []config.Agent{{Name: "worker"}}})
		env.setSessionMetadata(&b, map[string]string{"sleep_intent": "idle-stop-pending"})
		it := newFakeIdleTracker()
		it.idle["worker"] = true
		reconcileAt(env, t.TempDir(), b, it)
		stopped(t, env)
	})
	t.Run("max-age", func(t *testing.T) {
		env := newReconcilerTestEnv()
		b := active(t, env, &config.City{Agents: []config.Agent{{Name: "worker", MaxSessionAge: "5h"}}})
		env.setSessionMetadata(&b, map[string]string{"creation_complete_at": env.clk.Now().Add(-6 * time.Hour).UTC().Format(time.RFC3339)})
		tr := newMaxSessionAgeTracker()
		tr.setConfig("worker", 5*time.Hour, 0)
		reconcileAt(env, t.TempDir(), b, nil, withMaxSessionAgeTracker(tr))
		stopped(t, env)
	})
	t.Run("restart-requested", func(t *testing.T) {
		env := newReconcilerTestEnv()
		b := active(t, env, &config.City{Agents: []config.Agent{{Name: "worker"}}})
		env.setSessionMetadata(&b, map[string]string{"restart_requested": "true"})
		reconcileAt(env, t.TempDir(), b, nil)
		if env.sp.CountCalls("Stop", "worker") == 0 || strings.Contains(env.stderr.String(), sessionpkg.ErrRuntimeLeaseNoCity.Error()) {
			t.Fatalf("stops %d, stderr %q; want the restart's stop under the city's lease", env.sp.CountCalls("Stop", "worker"), env.stderr.String())
		}
	})
	t.Run("verifiedStop", func(t *testing.T) {
		env := newReconcilerTestEnv()
		b := active(t, env, &config.City{Agents: []config.Agent{{Name: "worker"}}})
		if err := verifiedStop(t.TempDir(), sessionInfoFromBead(mustGetBead(t, env.store, b.ID)), env.store, env.sp, env.cfg); err != nil {
			t.Fatalf("verifiedStop: %v", err)
		}
		stopped(t, env)
	})
	t.Run("retire", func(t *testing.T) {
		env := newReconcilerTestEnv()
		b := active(t, env, &config.City{Agents: []config.Agent{{Name: "worker"}}})
		if !stopRuntimeBeforeSessionBeadMutation(t.TempDir(), env.store, env.sp, env.cfg, mustGetBead(t, env.store, b.ID), "retired", &env.stderr) {
			t.Fatalf("retire stop refused; stderr %q", env.stderr.String())
		}
		stopped(t, env)
	})
}

// TestVerifiedStopDecidesAgainUnderTheLease: the drain's timeout kill spares
// a row whose hold moved since the tick read it.
func TestVerifiedStopDecidesAgainUnderTheLease(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addDesired("worker", "worker", true)
	b := env.createSessionBead("worker", "worker")
	env.markSessionActive(&b)
	decided := sessionInfoFromBead(mustGetBead(t, env.store, b.ID))
	env.setSessionMetadata(&b, map[string]string{"held_until": "2099-01-01T00:00:00Z"})
	if err := verifiedStop(t.TempDir(), decided, env.store, env.sp, env.cfg); !errors.Is(err, sessionpkg.ErrKillPremiseMoved) || !env.sp.IsRunning("worker") {
		t.Fatalf("verifiedStop of a moved row = %v (running %v), want ErrKillPremiseMoved", err, env.sp.IsRunning("worker"))
	}
}

// TestControllerKillResolvesStrictlyByRowID: a controller kill whose row does
// not resolve defers with an error, and never stops a runtime named after the
// row's ID.
func TestControllerKillResolvesStrictlyByRowID(t *testing.T) {
	store := beads.NewMemStore()
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "gc-missing", runtime.Config{Command: "x"}); err != nil {
		t.Fatal(err)
	}
	err := controllerKillSessionRow(t.TempDir(), store, sp, &config.City{}, sessionpkg.Info{ID: "gc-missing"})
	if err == nil || sp.CountCalls("Stop", "gc-missing") != 0 {
		t.Fatalf("kill of an unresolvable row = %v, stops %d; want an error and no stop", err, sp.CountCalls("Stop", "gc-missing"))
	}
}
