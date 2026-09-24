package main

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func reconcileIdleRespawnTestTick(
	t *testing.T,
	env *reconcilerTestEnv,
	session beads.Bead,
	work beads.Bead,
	ready bool,
) {
	t.Helper()
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeads(
		context.Background(), []beads.Bead{session}, env.desiredState, cfgNames, env.cfg, env.sp,
		env.store, nil, []beads.Bead{work}, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		withReadyAssignedFlags([]bool{ready}),
	)
}

func newIdleRespawnReconcilerTest(t *testing.T, workStatus string, detachedAgo time.Duration) (*reconcilerTestEnv, beads.Bead, beads.Bead) {
	t.Helper()
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"},
		Agents:       []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	detachedAt := env.clk.Now().Add(-detachedAgo).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{
		"state":        "active",
		"last_woke_at": detachedAt,
		"detached_at":  detachedAt,
	})
	work, err := env.store.Create(beads.Bead{
		Title:    "assigned work",
		Type:     "task",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("create assigned work: %v", err)
	}
	if workStatus != "open" {
		if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &workStatus}); err != nil {
			t.Fatalf("set assigned work status: %v", err)
		}
		work.Status = workStatus
	}
	return env, session, work
}

func completeIdleRespawnProbe(t *testing.T, env *reconcilerTestEnv, session beads.Bead, work beads.Bead, ready bool) {
	t.Helper()
	idleGate := make(chan struct{})
	env.sp.WaitForIdleErrors["worker"] = nil
	env.sp.WaitForIdleGates["worker"] = idleGate
	reconcileIdleRespawnTestTick(t, env, session, work, ready)
	close(idleGate)
	waitForIdleProbeReady(t, env.dt, session.ID)
	fresh, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	reconcileIdleRespawnTestTick(t, env, fresh, work, ready)
}

func idleRespawnUnitStore(t *testing.T, info sessionpkg.Info) *sessionpkg.Store {
	t.Helper()
	store := beads.NewMemStore()
	store.HonorExplicitIDs = true
	if _, err := store.Create(beads.Bead{
		ID:   info.ID,
		Type: sessionBeadType,
		Metadata: map[string]string{
			"session_name": info.SessionNameMetadata,
			"generation":   info.Generation,
			"detached_at":  info.DetachedAt,
		},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	return sessionFrontDoor(store)
}

func TestReconcileSessionBeads_IdleRespawnNeverDrainsClaimHolder(t *testing.T) {
	env, session, work := newIdleRespawnReconcilerTest(t, "in_progress", 2*time.Minute)

	reconcileIdleRespawnTestTick(t, env, session, work, false)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("claimed in-progress work must veto idle-respawn, got drain %+v", ds)
	}
	if _, ok := env.dt.idleProbe(session.ID); ok {
		t.Fatal("claimed in-progress work must not launch an idle-respawn probe")
	}
}

func TestReconcileSessionBeads_IdleRespawnHonorsConfiguredDuration(t *testing.T) {
	env, session, work := newIdleRespawnReconcilerTest(t, "open", 30*time.Second)

	reconcileIdleRespawnTestTick(t, env, session, work, true)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("session inside sleep_after_idle must not drain, got %+v", ds)
	}
	if _, ok := env.dt.idleProbe(session.ID); ok {
		t.Fatal("session inside sleep_after_idle must not launch an idle probe")
	}
}

func TestReconcileSessionBeads_IdleRespawnIsBoundedPerAssignedBead(t *testing.T) {
	env, session, work := newIdleRespawnReconcilerTest(t, "open", 2*time.Minute)

	completeIdleRespawnProbe(t, env, session, work, true)
	if ds := env.dt.get(session.ID); ds == nil || ds.reason != idleRespawnDrainReason {
		t.Fatalf("first idle-respawn cycle did not begin: %+v", ds)
	}

	// Model the replacement incarnation reaching the same idle state while the
	// same ready bead remains assigned. A second reconcile cycle must leave it
	// running rather than starting an unbounded stop/respawn loop.
	env.dt.remove(session.ID)
	env.stdout = bytes.Buffer{}
	env.stderr = bytes.Buffer{}
	fresh, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("reload session after first cycle: %v", err)
	}
	reconcileIdleRespawnTestTick(t, env, fresh, work, true)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("second idle-respawn cycle for the same bead must be suppressed, got %+v", ds)
	}
	if _, ok := env.dt.idleProbe(session.ID); ok {
		t.Fatal("second idle-respawn cycle for the same bead must not launch another probe")
	}
}

func TestReconcileSessionBeads_IdleRespawnCancelsWhenActivityResumesBeforeAck(t *testing.T) {
	clk := &clock.Fake{Time: time.Now().UTC()}
	sp := runtime.NewFake()
	name := "worker-1"
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	info := sessionpkg.Info{
		ID:                  "session-1",
		SessionNameMetadata: name,
		Generation:          "1",
		DetachedAt:          clk.Now().Add(-2 * time.Minute).Format(time.RFC3339),
	}
	eval := wakeEvaluation{
		Reason:             "assigned-work",
		Reasons:            []WakeReason{WakeWork},
		AssignedWorkBeadID: "work-1",
		Policy: resolvedSessionSleepPolicy{
			Class:      config.SessionSleepInteractiveResume,
			Effective:  "60s",
			Capability: runtime.SessionSleepCapabilityFull,
			Duration:   time.Minute,
		},
	}
	dt := newDrainTracker()
	if !beginSessionDrainInfo(info, sp, dt, idleRespawnDrainReason, clk, defaultDrainTimeout) {
		t.Fatal("begin idle-respawn drain")
	}
	sp.SetActivity(name, clk.Now().Add(time.Second))

	advanceSessionDrainsWithSessionsTraced(
		dt,
		sp,
		nil,
		func(id string) (sessionpkg.Info, bool) { return info, id == info.ID },
		map[string]wakeEvaluation{info.ID: eval},
		&config.City{},
		clk,
		nil,
	)

	if ds := dt.get(info.ID); ds != nil {
		t.Fatalf("activity after the idle probe must cancel idle-respawn, got %+v", ds)
	}
	ack, err := sp.GetMeta(name, "GC_DRAIN_ACK")
	if err != nil {
		t.Fatalf("get runtime metadata: %v", err)
	}
	if ack == "1" {
		t.Fatal("idle-respawn published a drain acknowledgement after activity resumed")
	}
}

type unavailableIdleActivityProvider struct {
	*runtime.Fake
}

func (p *unavailableIdleActivityProvider) GetLastActivity(string) (time.Time, error) {
	return time.Time{}, runtime.ErrRuntimeUnavailable
}

// A session that is awake ONLY because it owns ready assigned work is the
// sleep-and-respawn case; anything with another (or no) wake reason is not.
func TestIdleAssignedWorkOnly(t *testing.T) {
	if !idleAssignedWorkOnly(wakeEvaluation{Reason: "assigned-work", Reasons: []WakeReason{WakeWork}}) {
		t.Fatal("assigned-work-only eval should qualify")
	}
	if idleAssignedWorkOnly(wakeEvaluation{Reason: "assigned-work", Reasons: []WakeReason{WakeWork, WakePending}}) {
		t.Fatal("eval with an extra wake reason must not qualify")
	}
	if idleAssignedWorkOnly(wakeEvaluation{Reason: "min-active", Reasons: []WakeReason{WakeConfig}}) {
		t.Fatal("non-assigned-work eval must not qualify")
	}
	if idleAssignedWorkOnly(wakeEvaluation{}) {
		t.Fatal("empty eval must not qualify")
	}
}

// The idle-respawn drain must survive the per-tick cancel checks so the
// persistent assigned-work demand cannot undo it before the session sleeps.
func TestDrainReasonCancelable_IdleRespawnNotCancelable(t *testing.T) {
	if drainReasonCancelable(idleRespawnDrainReason) {
		t.Fatalf("%q must be non-cancelable (drainReasonCancelable)", idleRespawnDrainReason)
	}
	if assignedWorkDrainReasonCancelable(idleRespawnDrainReason) {
		t.Fatalf("%q must not be cancelable by the assigned-work cancel path", idleRespawnDrainReason)
	}
	if !drainReasonCancelable("idle") {
		t.Fatal("ordinary idle drain should remain cancelable")
	}
}

// An idle session awake solely for assigned work must be eligible for an idle
// probe (so it can sleep-and-respawn) even though it carries a wake reason and
// is not ConfigSuppressed — the original gate skipped it.
func TestSelectIdleProbeTargets_IncludesAssignedWorkOnly(t *testing.T) {
	policy := resolvedSessionSleepPolicy{
		Class:      config.SessionSleepInteractiveResume,
		Effective:  "60s",
		Capability: runtime.SessionSleepCapabilityFull,
		Duration:   time.Minute,
	}
	now := time.Now().UTC()
	info := sessionpkg.Info{ID: "s1", SessionNameMetadata: "run-operator-1", DetachedAt: now.Add(-2 * time.Minute).Format(time.RFC3339)}
	target := wakeTarget{
		info:  info,
		alive: true,
	}
	wakeEvals := map[string]wakeEvaluation{
		"s1": {Reason: "assigned-work", Reasons: []WakeReason{WakeWork}, Policy: policy, AssignedWorkBeadID: "work-1"},
	}
	infoByID := map[string]sessionpkg.Info{"s1": info}
	dt := newDrainTracker()
	got := selectIdleProbeTargets([]wakeTarget{target}, wakeEvals, dt, infoByID, now)
	if !got["s1"] {
		t.Fatalf("assigned-work-only idle session must be idle-probe-eligible, got %v", got)
	}
}

// A non-assigned-work (or no-reason) session must still be skipped unless it is
// the classic ConfigSuppressed-with-no-reasons idle case.
func TestSelectIdleProbeTargets_SkipsOtherWakeReasons(t *testing.T) {
	policy := resolvedSessionSleepPolicy{
		Class:      config.SessionSleepInteractiveResume,
		Effective:  "60s",
		Capability: runtime.SessionSleepCapabilityFull,
	}
	info := sessionpkg.Info{ID: "s1", SessionNameMetadata: "worker-1"}
	target := wakeTarget{
		info:  info,
		alive: true,
	}
	// A pending wake reason (not assigned-work-only) must NOT be probe-eligible.
	wakeEvals := map[string]wakeEvaluation{
		"s1": {Reason: "pending", Reasons: []WakeReason{WakePending}, Policy: policy},
	}
	infoByID := map[string]sessionpkg.Info{"s1": info}
	got := selectIdleProbeTargets([]wakeTarget{target}, wakeEvals, newDrainTracker(), infoByID, time.Now())
	if got["s1"] {
		t.Fatalf("a pending-wake session must not be idle-probe-eligible, got %v", got)
	}
}

// With a COMPLETED idle probe proving the agent idle, an alive assigned-work
// session begins an idle-respawn drain (→ asleep, resume-on-ready re-spawns it).
// Without a completed probe, or for a non-assigned-work session, it does not.
func TestBeginIdleRespawnDrainIfIdle(t *testing.T) {
	clk := &clock.Fake{Time: time.Now().UTC()}
	sp := runtime.NewFake()
	name := "run-operator-1"
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info := sessionpkg.Info{ID: "s1", SessionNameMetadata: name, Generation: "1", DetachedAt: clk.Now().Add(-2 * time.Minute).Format(time.RFC3339)}
	policy := resolvedSessionSleepPolicy{Class: config.SessionSleepInteractiveResume, Effective: "60s", Capability: runtime.SessionSleepCapabilityFull, Duration: time.Minute}
	eval := wakeEvaluation{Reason: "assigned-work", Reasons: []WakeReason{WakeWork}, Policy: policy, AssignedWorkBeadID: "work-1"}
	sessFront := idleRespawnUnitStore(t, info)

	// Positive: completed, successful idle probe + idle agent.
	dt := newDrainTracker()
	probe := dt.startIdleProbe(info.ID)
	dt.finishIdleProbe(info.ID, probe, true, clk.Now().Add(-time.Second))
	began, _, err := beginIdleRespawnDrainIfIdle(info, eval, dt, sp, sessFront, clk)
	if err != nil {
		t.Fatalf("begin idle-respawn drain: %v", err)
	}
	if !began {
		t.Fatal("idle assigned-work session with a completed idle probe should begin an idle-respawn drain")
	}
	if ds := dt.get(info.ID); ds == nil || ds.reason != idleRespawnDrainReason {
		t.Fatalf("expected an idle-respawn drain, got %+v", ds)
	}

	// Negative: not assigned-work-only (a different wake reason) → no drain.
	dt2 := newDrainTracker()
	p2 := dt2.startIdleProbe(info.ID)
	dt2.finishIdleProbe(info.ID, p2, true, clk.Now().Add(-time.Second))
	other := wakeEvaluation{Reason: "min-active", Reasons: []WakeReason{WakeConfig}, Policy: policy}
	began, _, err = beginIdleRespawnDrainIfIdle(info, other, dt2, sp, sessFront, clk)
	if err != nil {
		t.Fatalf("evaluate other wake reason: %v", err)
	}
	if began {
		t.Fatal("non-assigned-work session must not begin an idle-respawn drain")
	}

	// Negative: no completed idle probe → no drain (guards against false sleep).
	dt3 := newDrainTracker()
	began, _, err = beginIdleRespawnDrainIfIdle(info, eval, dt3, sp, sessFront, clk)
	if err != nil {
		t.Fatalf("evaluate incomplete probe: %v", err)
	}
	if began {
		t.Fatal("without a completed idle probe, no idle-respawn drain should begin")
	}
}

func TestBeginIdleRespawnDrainIfIdle_PropagatesUnavailableActivity(t *testing.T) {
	clk := &clock.Fake{Time: time.Now().UTC()}
	sp := &unavailableIdleActivityProvider{Fake: runtime.NewFake()}
	name := "worker-1"
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info := sessionpkg.Info{ID: "s1", SessionNameMetadata: name, Generation: "1", DetachedAt: clk.Now().Add(-2 * time.Minute).Format(time.RFC3339)}
	eval := wakeEvaluation{
		Reason:  "assigned-work",
		Reasons: []WakeReason{WakeWork},
		Policy: resolvedSessionSleepPolicy{
			Class:      config.SessionSleepInteractiveResume,
			Effective:  "60s",
			Capability: runtime.SessionSleepCapabilityFull,
			Duration:   time.Minute,
		},
		AssignedWorkBeadID: "work-1",
	}
	dt := newDrainTracker()
	probe := dt.startIdleProbe(info.ID)
	dt.finishIdleProbe(info.ID, probe, true, clk.Now().Add(-time.Second))

	began, _, err := beginIdleRespawnDrainIfIdle(info, eval, dt, sp, idleRespawnUnitStore(t, info), clk)
	if began {
		t.Fatal("activity observation failure must not begin an idle-respawn drain")
	}
	if !errors.Is(err, runtime.ErrRuntimeUnavailable) {
		t.Fatalf("error = %v, want runtime unavailable", err)
	}
}

// Non-interactive sessions short-circuit shouldBeginIdleDrainInfo to true without a
// probe; they must be excluded from idle-respawn so an assigned non-interactive
// (e.g. named) session is not wrongly drained.
func TestBeginIdleRespawnDrainIfIdle_SkipsNonInteractive(t *testing.T) {
	clk := &clock.Fake{Time: time.Now().UTC()}
	sp := runtime.NewFake()
	name := "ni-1"
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	info := sessionpkg.Info{ID: "ni", SessionNameMetadata: name, Generation: "1", DetachedAt: clk.Now().Add(-2 * time.Minute).Format(time.RFC3339)}
	eval := wakeEvaluation{
		Reason:             "assigned-work",
		Reasons:            []WakeReason{WakeWork},
		Policy:             resolvedSessionSleepPolicy{Class: config.SessionSleepNonInteractive, Effective: "60s", Duration: time.Minute},
		AssignedWorkBeadID: "work-1",
	}
	dt := newDrainTracker()
	probe := dt.startIdleProbe(info.ID)
	dt.finishIdleProbe(info.ID, probe, true, clk.Now().Add(-time.Second))
	began, _, err := beginIdleRespawnDrainIfIdle(info, eval, dt, sp, idleRespawnUnitStore(t, info), clk)
	if err != nil {
		t.Fatalf("evaluate non-interactive session: %v", err)
	}
	if began {
		t.Fatal("a non-interactive assigned-work session must not be idle-respawn-drained")
	}
}
