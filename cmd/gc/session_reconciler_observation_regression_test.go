package main

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// sequencedRuntimeObservationProvider makes a secondary observation fail while
// leaving the primary liveness probe authoritative. This exercises the actual
// reconciler boundaries rather than a helper-only approximation.
type sequencedRuntimeObservationProvider struct {
	*runtime.Fake
	mu                    sync.Mutex
	livenessCalls         int
	activityCalls         int
	livenessUnavailableAt map[int]bool
	activityUnavailableAt map[int]bool
	activityError         error
}

func (p *sequencedRuntimeObservationProvider) ObserveLivenessWithError(name string, _ []string) (runtime.Liveness, error) {
	p.mu.Lock()
	p.livenessCalls++
	call := p.livenessCalls
	unavailable := p.livenessUnavailableAt[call]
	p.mu.Unlock()
	if unavailable {
		return runtime.Liveness{}, fmt.Errorf("liveness observation %d for %s: %w", call, name, runtime.ErrRuntimeUnavailable)
	}
	running := p.IsRunning(name)
	return runtime.Liveness{Running: running, Alive: running}, nil
}

func (p *sequencedRuntimeObservationProvider) GetLastActivity(name string) (time.Time, error) {
	p.mu.Lock()
	p.activityCalls++
	call := p.activityCalls
	unavailable := p.activityUnavailableAt[call]
	p.mu.Unlock()
	if unavailable {
		observationErr := p.activityError
		if observationErr == nil {
			observationErr = runtime.ErrRuntimeUnavailable
		}
		return time.Time{}, fmt.Errorf("activity observation %d for %s: %w", call, name, observationErr)
	}
	return p.Fake.GetLastActivity(name)
}

func (p *sequencedRuntimeObservationProvider) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.livenessCalls, p.activityCalls
}

func reconcileWithSequencedRuntimeObservation(
	env *reconcilerTestEnv,
	sessions []beads.Bead,
	sp runtime.Provider,
	poolDesired map[string]int,
) int {
	return reconcileSessionBeads(
		context.Background(), sessions, env.desiredState,
		configuredSessionNames(env.cfg, "", env.store), env.cfg, sp,
		env.store, nil, nil, nil, env.dt, poolDesired, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		env.startOptions...,
	)
}

func assertObservationDeferralPreservedSession(t *testing.T, env *reconcilerTestEnv, before beads.Bead, sessionName string, startsBefore, stopsBefore int) {
	t.Helper()
	after, err := env.store.Get(before.ID)
	if err != nil {
		t.Fatalf("Get(%s) after reconcile: %v", before.ID, err)
	}
	if after.Status != before.Status || !maps.Equal(after.Metadata, before.Metadata) {
		t.Fatalf("runtime-observation deferral mutated session:\n before: status=%q metadata=%#v\n  after: status=%q metadata=%#v", before.Status, before.Metadata, after.Status, after.Metadata)
	}
	if got := env.sp.CountCalls("Start", sessionName); got != startsBefore {
		t.Fatalf("Start calls = %d, want preserved %d", got, startsBefore)
	}
	if got := env.sp.CountCalls("Stop", sessionName); got != stopsBefore {
		t.Fatalf("Stop calls = %d, want preserved %d", got, stopsBefore)
	}
}

func TestNamedSessionActiveUseReasonInfoKeepsNonSentinelActivityErrorsBestEffort(t *testing.T) {
	env := newReconcilerTestEnv()
	if err := env.sp.Start(context.Background(), "worker", runtime.Config{}); err != nil {
		t.Fatalf("Start(worker): %v", err)
	}
	sp := &sequencedRuntimeObservationProvider{
		Fake:                  env.sp,
		activityUnavailableAt: map[int]bool{1: true},
		activityError:         errors.New("legacy provider activity parse error"),
	}

	reason, active, err := shouldDeferNamedSessionConfigDrift(
		env.createSessionInfo("worker", "worker"), nil, sp, "worker", env.clk, "drift-key",
	)
	if err != nil || active || reason != "" {
		t.Fatalf("non-sentinel activity error = (%q, %v, %v), want legacy best-effort inactive", reason, active, err)
	}
}

func TestReconcileSessionBeads_AttachmentUnavailableDoesNotRecordDetachedAt(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"},
		Agents:       []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{"state": "awake"})
	before, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get before reconcile: %v", err)
	}
	startsBefore := env.sp.CountCalls("Start", "worker")
	stopsBefore := env.sp.CountCalls("Stop", "worker")
	sp := &sequencedRuntimeObservationProvider{Fake: env.sp, livenessUnavailableAt: map[int]bool{2: true}}

	if woken := reconcileWithSequencedRuntimeObservation(env, []beads.Bead{before}, sp, map[string]int{"worker": 1}); woken != 0 {
		t.Fatalf("woken = %d, want 0 while attachment is unavailable", woken)
	}
	assertObservationDeferralPreservedSession(t, env, before, "worker", startsBefore, stopsBefore)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("attachment uncertainty started a drain: %+v", ds)
	}
	if livenessCalls, _ := sp.counts(); livenessCalls != 2 {
		t.Fatalf("liveness observations = %d, want primary success then attachment failure", livenessCalls)
	}
	if !strings.Contains(env.stderr.String(), "attachment observation") {
		t.Fatalf("stderr = %q, want attachment-observation deferral", env.stderr.String())
	}
}

func TestReconcileSessionBeads_AwakeAttachmentUnavailableDoesNotStartNoWakeDrain(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker", SleepAfterIdle: config.SessionSleepOff}}}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"state":                      "awake",
		"session_key":                "keep-session",
		"continuation_reset_pending": "keep-reset",
		"wake_attempts":              "2",
	})
	before, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get before reconcile: %v", err)
	}
	startsBefore := env.sp.CountCalls("Start", "worker")
	stopsBefore := env.sp.CountCalls("Stop", "worker")
	sp := &sequencedRuntimeObservationProvider{Fake: env.sp, livenessUnavailableAt: map[int]bool{2: true}}

	if woken := reconcileWithSequencedRuntimeObservation(env, []beads.Bead{before}, sp, map[string]int{}); woken != 0 {
		t.Fatalf("woken = %d, want 0 while awake-input attachment is unavailable", woken)
	}
	assertObservationDeferralPreservedSession(t, env, before, "worker", startsBefore, stopsBefore)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("awake-input attachment uncertainty started a no-wake drain: %+v", ds)
	}
	if livenessCalls, _ := sp.counts(); livenessCalls != 2 {
		t.Fatalf("liveness observations = %d, want primary success then awake-input failure", livenessCalls)
	}
	if !strings.Contains(env.stderr.String(), "deferring lifecycle") {
		t.Fatalf("stderr = %q, want lifecycle deferral diagnostic", env.stderr.String())
	}
}

func TestReconcileSessionBeads_ActivityUnavailableDoesNotStartIdleDrain(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		SessionSleep: config.SessionSleepConfig{InteractiveResume: "60s"},
		Agents:       []config.Agent{{Name: "worker"}},
	}
	env.addDesired("worker", "worker", true)
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	idleSince := env.clk.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339)
	env.setSessionMetadata(&session, map[string]string{"state": "awake", "detached_at": idleSince, "last_woke_at": idleSince})
	env.sp.SetActivity("worker", env.clk.Now().Add(-2*time.Minute))
	probe := env.dt.startIdleProbe(session.ID)
	env.dt.finishIdleProbe(session.ID, probe, true, env.clk.Now())
	before, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get before reconcile: %v", err)
	}
	startsBefore := env.sp.CountCalls("Start", "worker")
	stopsBefore := env.sp.CountCalls("Stop", "worker")
	sp := &sequencedRuntimeObservationProvider{Fake: env.sp, activityUnavailableAt: map[int]bool{4: true}}

	if woken := reconcileWithSequencedRuntimeObservation(env, []beads.Bead{before}, sp, map[string]int{"worker": 1}); woken != 0 {
		t.Fatalf("woken = %d, want 0 while idle activity is unavailable", woken)
	}
	assertObservationDeferralPreservedSession(t, env, before, "worker", startsBefore, stopsBefore)
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("activity uncertainty started an idle drain: %+v", ds)
	}
	if got, ok := env.dt.idleProbe(session.ID); !ok || !got.ready || !got.success {
		t.Fatalf("ready idle probe consumed during uncertainty: probe=%+v ok=%v", got, ok)
	}
	if _, activityCalls := sp.counts(); activityCalls != 4 {
		t.Fatalf("activity observations = %d, want three healthy reads then lifecycle failure", activityCalls)
	}
	if !strings.Contains(env.stderr.String(), "runtime observation failure") {
		t.Fatalf("stderr = %q, want activity-observation deferral", env.stderr.String())
	}
}

func TestReconcileSessionBeads_ConfigDriftActivityUnavailableDoesNotRestartNamedSession(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "new-cmd"}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "new-cmd",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
		ResolvedProvider:        &config.ResolvedProvider{Name: "fake", Command: "new-cmd"},
		Hints:                   agent.StartupHints{},
	}
	oldRuntime := runtime.Config{Command: "old-cmd"}
	if err := env.sp.Start(context.Background(), sessionName, oldRuntime); err != nil {
		t.Fatalf("Start(%s): %v", sessionName, err)
	}
	env.sp.SetActivity(sessionName, env.clk.Now().Add(-2*time.Hour))
	session := env.createSessionBead(sessionName, "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"state":                      "awake",
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"session_key":                "keep-session",
		"started_config_hash":        runtime.CoreFingerprint(oldRuntime),
		"started_live_hash":          runtime.LiveFingerprint(oldRuntime),
		"wake_attempts":              "2",
	})
	before, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get before reconcile: %v", err)
	}
	startsBefore := env.sp.CountCalls("Start", sessionName)
	stopsBefore := env.sp.CountCalls("Stop", sessionName)
	sp := &sequencedRuntimeObservationProvider{Fake: env.sp, activityUnavailableAt: map[int]bool{4: true}}

	if woken := reconcileWithSequencedRuntimeObservation(env, []beads.Bead{before}, sp, map[string]int{"worker": 1}); woken != 0 {
		t.Fatalf("woken = %d, want 0 while config-drift activity is unavailable", woken)
	}
	assertObservationDeferralPreservedSession(t, env, before, sessionName, startsBefore, stopsBefore)
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("named session %q stopped while activity was unavailable", sessionName)
	}
	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("config-drift uncertainty started a drain: %+v", ds)
	}
	if _, activityCalls := sp.counts(); activityCalls != 4 {
		t.Fatalf("activity observations = %d, want primary and attachment reads before config-drift failure", activityCalls)
	}
	if !strings.Contains(env.stderr.String(), "config-drift") {
		t.Fatalf("stderr = %q, want config-drift observation deferral", env.stderr.String())
	}
}

func TestReconcileSessionBeads_ConfigDriftDrainAckRuntimeUnavailablePreservesDrainState(t *testing.T) {
	env := newReconcilerTestEnv()
	env.cfg = &config.City{Agents: []config.Agent{{Name: "worker"}}}
	env.addRunningWorkerDesiredWithNewConfig()
	session := env.createSessionBead("worker", "worker")
	env.markSessionActive(&session)
	env.setSessionMetadata(&session, map[string]string{
		"started_config_hash": runtime.CoreFingerprint(runtime.Config{Command: "test-cmd"}),
	})
	dops := newDrainOps(env.sp)
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 1}, dops)
	drain := env.dt.get(session.ID)
	if drain == nil || drain.reason != "config-drift" || !drain.ackSet {
		t.Fatalf("queued config-drift drain = %+v, want acknowledged tracker", drain)
	}
	drainBefore := *drain
	beadBefore, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get before uncertain reconcile: %v", err)
	}
	metaKeys := []string{"GC_DRAIN", "GC_DRAIN_ACK", reconcilerDrainAckSourceKey, reconcilerDrainAckReasonKey, reconcilerDrainAckGenerationKey}
	runtimeMetaBefore := make(map[string]string, len(metaKeys))
	for _, key := range metaKeys {
		runtimeMetaBefore[key], _ = env.sp.GetMeta("worker", key)
	}
	startsBefore := env.sp.CountCalls("Start", "worker")
	stopsBefore := env.sp.CountCalls("Stop", "worker")
	sp := &sequencedRuntimeObservationProvider{Fake: env.sp, activityUnavailableAt: map[int]bool{3: true}}

	reconcileSessionBeads(
		context.Background(), []beads.Bead{beadBefore}, env.desiredState,
		configuredSessionNames(env.cfg, "", env.store), env.cfg, sp,
		env.store, newDrainOps(sp), nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
		env.startOptions...,
	)

	afterDrain := env.dt.get(session.ID)
	if afterDrain == nil || *afterDrain != drainBefore {
		livenessCalls, activityCalls := sp.counts()
		t.Fatalf("runtime uncertainty mutated queued drain: before=%+v after=%+v liveness=%d activity=%d", drainBefore, afterDrain, livenessCalls, activityCalls)
	}
	beadAfter, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get after uncertain reconcile: %v", err)
	}
	if beadAfter.Status != beadBefore.Status || !maps.Equal(beadAfter.Metadata, beadBefore.Metadata) {
		t.Fatalf("runtime uncertainty mutated bead: before=%+v after=%+v", beadBefore, beadAfter)
	}
	for _, key := range metaKeys {
		got, _ := env.sp.GetMeta("worker", key)
		if got != runtimeMetaBefore[key] {
			t.Fatalf("runtime metadata %s = %q, want %q", key, got, runtimeMetaBefore[key])
		}
	}
	if got := env.sp.CountCalls("Start", "worker"); got != startsBefore {
		t.Fatalf("Start calls = %d, want %d", got, startsBefore)
	}
	if got := env.sp.CountCalls("Stop", "worker"); got != stopsBefore {
		t.Fatalf("Stop calls = %d, want %d", got, stopsBefore)
	}
	if _, activityCalls := sp.counts(); activityCalls < 3 {
		t.Fatalf("activity observations = %d, want queued drain-ack observation", activityCalls)
	}
}

func TestAdvanceSessionDrains_LivenessUnavailableAfterVerifiedStopDefersCompletion(t *testing.T) {
	now := time.Date(2026, 8, 4, 12, 0, 0, 0, time.UTC)
	clk := &clock.Fake{Time: now}
	sp := &sequencedRuntimeObservationProvider{Fake: runtime.NewFake(), livenessUnavailableAt: map[int]bool{2: true}}
	store := beads.NewMemStore()
	dt := newDrainTracker()
	if err := sp.Start(context.Background(), "test-session", runtime.Config{}); err != nil {
		t.Fatalf("start runtime: %v", err)
	}
	if err := sp.SetMeta("test-session", "GC_DRAIN_ACK", "1"); err != nil {
		t.Fatalf("set drain ack: %v", err)
	}
	b, err := store.Create(beads.Bead{
		Title: "test", Type: sessionBeadType, Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "test-session", "template": "worker", "provider": "claude", "work_dir": t.TempDir(),
			"generation": "3", "state": "active", "wake_mode": "fresh", "session_key": "keep-session",
			"started_config_hash": "keep-hash", "continuation_reset_pending": "", "pending_create_claim": "true",
			"last_woke_at": now.Add(-time.Minute).Format(time.RFC3339),
		},
	})
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	ds := &drainState{
		startedAt: now.Add(-time.Minute), deadline: now.Add(-10 * time.Second),
		reason: "pool-excess", generation: 3, ackSet: true,
	}
	dt.set(b.ID, ds)
	before, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get before drain advance: %v", err)
	}

	advanceSessionDrainsWithSessionsTraced(dt, sp, store, infoLookupFromBeadLookup(func(id string) *beads.Bead {
		got, _ := store.Get(id)
		return &got
	}), map[string]wakeEvaluation{}, &config.City{}, clk, nil)

	if got := dt.get(b.ID); got != ds {
		t.Fatalf("drain tracker entry = %p, want retained %p", got, ds)
	}
	after, err := store.Get(b.ID)
	if err != nil {
		t.Fatalf("Get after drain advance: %v", err)
	}
	if !maps.Equal(after.Metadata, before.Metadata) {
		t.Fatalf("metadata mutated after unavailable post-stop probe: before=%#v after=%#v", before.Metadata, after.Metadata)
	}
	if got := sp.CountCalls("Stop", "test-session"); got != 1 {
		t.Fatalf("Stop calls = %d, want 1 verified stop", got)
	}
	if sp.IsRunning("test-session") {
		t.Fatal("verified stop did not stop underlying fake runtime")
	}
	if livenessCalls, _ := sp.counts(); livenessCalls != 2 {
		t.Fatalf("liveness observations = %d, want initial plus post-stop", livenessCalls)
	}
}
