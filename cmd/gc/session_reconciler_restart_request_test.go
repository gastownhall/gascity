package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

type restartRequestTestEnv struct {
	store        beads.Store
	sp           *runtime.Fake
	dt           *drainTracker
	clk          *clock.Fake
	rec          events.Recorder
	cfg          *config.City
	desiredState map[string]TemplateParams
	stdout       bytes.Buffer
	stderr       bytes.Buffer
	startOptions []startExecutionOption
	city         string              // the reconcile helpers' city path; only a test of the no-city refusal sets ""
	stops        *asyncStartTracker  // the drain-ack's async stops the reconcile helpers queue
	cache        *beads.CachingStore // the reconciler's session cache, nil in the single-store envs
}

func newRestartRequestTestEnv(t testing.TB) *restartRequestTestEnv {
	return newRestartRequestEnvWithStore(t, beads.NewMemStore())
}

// newRestartRequestSplitStoreEnv is the restart-request harness over the
// #5843 store shape: the reconciler reads and writes through a session
// CachingStore, and the returned handle is the shared backing alone — the
// CLI's one-shot store, whose writes land on the backing around that cache
// with no event, feed or re-scan delivering them.
func newRestartRequestSplitStoreEnv(t testing.TB) (*restartRequestTestEnv, beads.Store) {
	return newRestartRequestSplitStoreEnvOverBacking(t, beads.NewMemStore())
}

// newRestartRequestSplitStoreEnvOverBacking is the split-store harness over a
// caller-supplied backing: a test that needs to observe or interpose on the
// backing's own operations (the #5843 race windows) wraps the memory store and
// passes the wrapper here.
func newRestartRequestSplitStoreEnvOverBacking(t testing.TB, backing beads.Store) (*restartRequestTestEnv, beads.Store) {
	cache := beads.NewCachingStoreForTest(backing, nil)
	env := newRestartRequestEnvWithStore(t, cache)
	env.cache = cache
	return env, backing
}

// primeCache primes the reconciler's session cache: its snapshot reads are
// served from the primed rows, so a backing write that follows stays
// invisible to it until a refresh installs the row.
func (e *restartRequestTestEnv) primeCache(t testing.TB) {
	t.Helper()
	if err := e.cache.Prime(context.Background()); err != nil {
		t.Fatalf("priming the session cache: %v", err)
	}
}

func newRestartRequestEnvWithStore(t testing.TB, store beads.Store) *restartRequestTestEnv {
	setSessionCircuitBreakerForTest(t, newSessionCircuitBreaker(sessionCircuitBreakerConfig{}))
	env := &restartRequestTestEnv{
		store:        store,
		sp:           runtime.NewFake(),
		dt:           newDrainTracker(),
		clk:          &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)},
		rec:          events.Discard,
		cfg:          &config.City{},
		desiredState: make(map[string]TemplateParams),
		startOptions: []startExecutionOption{
			withStartStabilityWaiter(immediateStartStabilityWaiter),
			withSessionStaleKeyDetectionWaiter(immediateSessionStaleKeyDetectionWaiter),
		},
		city:  t.TempDir(),
		stops: &asyncStartTracker{},
	}
	// The drain-ack's async stops finish before the city is removed.
	env.startOptions = append(env.startOptions, withAsyncDrainAckStopTracker(env.stops))
	t.Cleanup(func() { env.stops.wait(5 * time.Second) })
	return env
}

func (e *restartRequestTestEnv) createSessionBead(name string) beads.Bead {
	b, err := e.store.Create(beads.Bead{
		Title:  name,
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name":   name,
			"agent_name":     name,
			"template":       "worker",
			"generation":     "1",
			"instance_token": "test-token",
			"state":          "asleep",
		},
	})
	if err != nil {
		panic("creating test bead: " + err.Error())
	}
	return b
}

func (e *restartRequestTestEnv) setSessionMetadata(session *beads.Bead, kvs map[string]string) {
	for key, value := range kvs {
		_ = e.store.SetMetadata(session.ID, key, value)
		session.Metadata[key] = value
	}
}

func (e *restartRequestTestEnv) reconcile(sessions []beads.Bead) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	e.reconcileWithPoolDesiredAndDrainOps(sessions, poolDesired, nil)
}

// reconcileWithDrainOps is reconcile with the controller's drain ops dops,
// through which the reconciler reads the runtime restart hint.
func (e *restartRequestTestEnv) reconcileWithDrainOps(sessions []beads.Bead, dops drainOps) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	e.reconcileWithPoolDesiredAndDrainOps(sessions, poolDesired, dops)
}

func (e *restartRequestTestEnv) reconcileWithPoolDesiredAndDrainOps(sessions []beads.Bead, poolDesired map[string]int, dops drainOps) {
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	_ = reconcileSessionBeadsAtPath(
		context.Background(),
		e.city,
		sessions,
		e.desiredState,
		cfgNames,
		e.cfg,
		e.sp,
		e.store,
		dops,
		nil,
		nil,
		nil,
		e.dt,
		poolDesired,
		false,
		nil,
		"",
		nil,
		e.clk,
		e.rec,
		0,
		0,
		&e.stdout,
		&e.stderr,
		e.startOptions...,
	)
}

type clearRestartErrorDrainOps struct {
	*fakeDrainOps
	err error
}

func (d *clearRestartErrorDrainOps) clearRestartRequested(string) error {
	return d.err
}

func newLiveRestartRequestScenario(t *testing.T) (*restartRequestTestEnv, beads.Bead, string) {
	t.Helper()

	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_RESTART_REQUESTED", "1"); err != nil {
		t.Fatalf("SetMeta(GC_RESTART_REQUESTED): %v", err)
	}

	return env, session, sessionName
}

func TestReconcileSessionBeads_RestartRequestRotatesKeyForSessionIDProviders(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["session_key"] == "" {
		t.Fatal("session_key = empty, want rotated key for SessionIDFlag provider")
	}
	if got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want empty", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

func TestReconcileSessionBeads_RestartRequestClearsKeyForResumeOnlyProviders(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			ResumeFlag:  "--resume",
			ResumeStyle: "flag",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["session_key"] != "" {
		t.Fatalf("session_key = %q, want empty for resume-only provider", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want empty", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
}

func TestReconcileSessionBeads_RestartRequestPreservesLiveHashesDuringHandoff(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
		"started_live_hash":          "live-before-restart",
		"live_hash":                  "live-before-restart",
		"startup_dialog_verified":    "true",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	got, _ := env.store.Get(session.ID)
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want empty", got.Metadata["started_config_hash"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["started_live_hash"] != "live-before-restart" {
		t.Fatalf("started_live_hash = %q, want preserved until next successful start", got.Metadata["started_live_hash"])
	}
	if got.Metadata["live_hash"] != "live-before-restart" {
		t.Fatalf("live_hash = %q, want preserved until next successful start", got.Metadata["live_hash"])
	}
	if got.Metadata["startup_dialog_verified"] != "true" {
		t.Fatalf("startup_dialog_verified = %q, want preserved until next successful start", got.Metadata["startup_dialog_verified"])
	}
}

func TestReconcileSessionBeads_RestartRequestReportsClearRestartRequestedError(t *testing.T) {
	env, session, sessionName := newLiveRestartRequestScenario(t)
	env.sp.RemoveMetaErrors[sessionName] = map[string]error{
		"GC_RESTART_REQUESTED": errors.New("permission denied"),
	}

	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 0}, newDrainOps(env.sp))

	got := env.stderr.String()
	if !strings.Contains(got, "clearing restart-requested marker") ||
		!strings.Contains(got, sessionName) ||
		!strings.Contains(got, session.ID) ||
		!strings.Contains(got, "permission denied") {
		t.Fatalf("stderr = %q, want contextual clearRestartRequested diagnostic", got)
	}
}

func TestReconcileSessionBeads_RestartRequestSuppressesGoneClearRestartRequestedError(t *testing.T) {
	env, session, sessionName := newLiveRestartRequestScenario(t)
	dops := &clearRestartErrorDrainOps{
		fakeDrainOps: newFakeDrainOps(),
		err:          fmt.Errorf("%w: %s", runtime.ErrSessionNotFound, sessionName),
	}
	dops.restartRequested[sessionName] = true

	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{session}, map[string]int{"worker": 0}, dops)

	if got := env.stderr.String(); got != "" {
		t.Fatalf("stderr = %q, want no diagnostic for gone clearRestartRequested error", got)
	}
}

func TestReconcileSessionBeads_RestartRequestPreservesIntentWhenKillFails(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	env.sp.StopErrors[sessionName] = errors.New("kill denied")

	env.reconcile([]beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatal("session should remain running when kill fails")
	}
	got, _ := env.store.Get(session.ID)
	if got.Metadata["restart_requested"] != "true" {
		t.Fatalf("restart_requested = %q, want preserved", got.Metadata["restart_requested"])
	}
	if got.Metadata["session_key"] != "original-key" {
		t.Fatalf("session_key = %q, want original-key", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "hash-before-restart" {
		t.Fatalf("started_config_hash = %q, want preserved", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty until kill succeeds", got.Metadata["continuation_reset_pending"])
	}
	if got := env.stderr.String(); !strings.Contains(got, "stopping restart-requested") || !strings.Contains(got, "kill denied") {
		t.Fatalf("stderr = %q, want kill failure diagnostic", got)
	}
}

func TestReconcileSessionBeads_RestartRequestClearsCircuitBreakerForNextWake(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Daemon: config.DaemonConfig{
			SessionCircuitBreaker:            true,
			SessionCircuitBreakerMaxRestarts: restartRequestTestIntPtr(3),
			SessionCircuitBreakerWindow:      "30m",
		},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	const identity = "worker"
	cb := breakerAt(30*time.Minute, 3)
	base := env.clk.Now().UTC()
	for i := 0; i < 4; i++ {
		cb.RecordRestart(identity, base.Add(time.Duration(i)*time.Second))
	}
	if !cb.IsOpen(identity, base.Add(time.Minute)) {
		t.Fatalf("precondition: breaker should be OPEN for %q", identity)
	}
	setSessionCircuitBreakerForTest(t, cb)

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: identity,
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := persistSessionCircuitBreakerMetadata(sessionFrontDoor(env.store), session.ID, cb, identity, base); err != nil {
		t.Fatalf("persist circuit metadata: %v", err)
	}
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running after restart-requested kill", sessionName)
	}
	if cb.IsOpen(identity, base.Add(time.Minute)) {
		t.Fatalf("breaker still OPEN for %q after restart-requested kill", identity)
	}
	stopped, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got := stopped.Metadata[sessionCircuitStateMetadata]; got != "" {
		t.Fatalf("persisted circuit state = %q, want cleared", got)
	}
	if got := stopped.Metadata[sessionCircuitRestartsMetadata]; got != "" {
		t.Fatalf("persisted restart history = %q, want cleared", got)
	}
	if got := stopped.Metadata[sessionCircuitResetGenerationMetadata]; got == "" {
		t.Fatal("persisted reset generation is empty, want explicit reset generation")
	}

	env.stdout.Reset()
	env.stderr.Reset()
	env.reconcile([]beads.Bead{stopped})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q did not wake after explicit reset cleared the circuit breaker", sessionName)
	}
	if got := env.stderr.String(); strings.Contains(got, "CIRCUIT_OPEN") {
		t.Fatalf("stderr = %q, want no circuit-open block after explicit reset", got)
	}
}

func TestReconcileSessionBeads_RestartRequestPreemptsRateLimitGate(t *testing.T) {
	env, session, sessionName := newRestartRequestedZombieSession(t)
	env.sp.SetPeekOutput(sessionName, "You've hit your limit, Pro plan\n\n/rate-limit-options")
	env.setSessionMetadata(&session, map[string]string{
		"last_woke_at": env.clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
	})

	env.reconcile([]beads.Bead{session})

	assertRestartRequestStoppedBeforeAutonomousGate(t, env, session.ID, sessionName)
	got, _ := env.store.Get(session.ID)
	if got.Metadata["sleep_reason"] == "rate_limit" {
		t.Fatalf("sleep_reason = %q, want explicit reset to preempt rate-limit gate", got.Metadata["sleep_reason"])
	}
}

func TestReconcileSessionBeads_RestartRequestPreemptsStabilityGate(t *testing.T) {
	env, session, sessionName := newRestartRequestedZombieSession(t)
	env.setSessionMetadata(&session, map[string]string{
		"last_woke_at": env.clk.Now().Add(-10 * time.Second).UTC().Format(time.RFC3339),
	})

	env.reconcile([]beads.Bead{session})

	assertRestartRequestStoppedBeforeAutonomousGate(t, env, session.ID, sessionName)
	got, _ := env.store.Get(session.ID)
	if got.Metadata["last_woke_at"] != "" {
		t.Fatalf("last_woke_at = %q, want restart patch to clear crash-tracker lease", got.Metadata["last_woke_at"])
	}
	if got.Metadata["sleep_reason"] == "rate_limit" {
		t.Fatalf("sleep_reason = %q, want explicit reset to preempt stability gate", got.Metadata["sleep_reason"])
	}
}

func TestReconcileSessionBeads_RestartRequestPreemptsChurnGate(t *testing.T) {
	env, session, sessionName := newRestartRequestedZombieSession(t)
	env.setSessionMetadata(&session, map[string]string{
		"last_woke_at": env.clk.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339),
	})

	env.reconcile([]beads.Bead{session})

	assertRestartRequestStoppedBeforeAutonomousGate(t, env, session.ID, sessionName)
	got, _ := env.store.Get(session.ID)
	if got.Metadata["churn_count"] != "" {
		t.Fatalf("churn_count = %q, want explicit reset to preempt churn gate", got.Metadata["churn_count"])
	}
}

func newRestartRequestedZombieSession(t *testing.T) (*restartRequestTestEnv, beads.Bead, string) {
	t.Helper()
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		Hints:        agent.StartupHints{ProcessNames: []string{"agent-cli"}},
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}
	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true", ProcessNames: []string{"agent-cli"}}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}
	env.sp.Zombies[sessionName] = true
	return env, session, sessionName
}

func assertRestartRequestStoppedBeforeAutonomousGate(t *testing.T, env *restartRequestTestEnv, sessionID, sessionName string) {
	t.Helper()
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; explicit reset should stop it before autonomous gates", sessionName)
	}
	got, _ := env.store.Get(sessionID)
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared after explicit reset patch", got.Metadata["restart_requested"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "" {
		t.Fatalf("started_config_hash = %q, want cleared", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if got := env.stdout.String(); !strings.Contains(got, "Stopped restart-requested session") {
		t.Fatalf("stdout = %q, want stop diagnostic", got)
	}
}

// TestReconcileSessionBeads_RestartRequestNamedAlwaysWakesSameTick guards the
// fix for gastownhall/gascity#2345. A `mode = "always"` named session whose
// tmux was killed out of band (for example, by `gc handoff --target`) before
// the bead's restart_requested flag was processed must wake on the SAME
// reconciler tick, not on the next patrol interval. Before this fix the
// restart_requested branch unconditionally continued past the wake decision,
// imposing a patrol_interval-sized post-handoff wake delay (and, combined
// with watchdog-driven `gc session reset` calls during the gap, sometimes
// multiple patrol cycles).
func TestReconcileSessionBeads_RestartRequestNamedAlwaysWakesSameTick(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})

	// Runtime is NOT started — the tmux session was killed externally
	// (e.g., gc handoff --target) before this reconciler tick. The bead's
	// restart_requested flag was set by `gc session reset` afterwards.
	if env.sp.IsRunning(sessionName) {
		t.Fatal("test fixture wrong: session should not be running")
	}

	env.reconcile([]beads.Bead{session})

	// Same-tick wake: the wake decision must have fired and started the
	// runtime on this same reconcile pass. Before the #2345 fix the
	// restart_requested branch continued past the wake loop, so the fake
	// provider would NOT be running here.
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q did not wake on the same reconciler tick; restart_requested branch skipped the wake decision", sessionName)
	}

	got, _ := env.store.Get(session.ID)
	// The RestartRequestPatch must still run: session_key rotates,
	// restart_requested clears. PreWakePatch (applied by the same-tick wake)
	// subsequently writes last_woke_at and clears continuation_reset_pending,
	// so we assert on the post-wake observable state.
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared after patch applied", got.Metadata["restart_requested"])
	}
	if got.Metadata["last_woke_at"] == "" {
		t.Fatal("last_woke_at = empty, want timestamp from same-tick wake")
	}
}

func restartRequestTestIntPtr(n int) *int { return &n }

func TestReconcileSessionBeads_RestartRequestSkipsCollateralKillForPinnedNamedSession(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"pin_awake":                  "true",
		"restart_requested":          "true",
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("pinned named session %q was killed by collateral restart request", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared after deferring collateral kill", got.Metadata["restart_requested"])
	}
	if got.Metadata["session_key"] != "original-key" {
		t.Fatalf("session_key = %q, want preserved", got.Metadata["session_key"])
	}
	if got.Metadata["started_config_hash"] != "hash-before-restart" {
		t.Fatalf("started_config_hash = %q, want preserved", got.Metadata["started_config_hash"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want untouched", got.Metadata["continuation_reset_pending"])
	}
	if got := env.stderr.String(); !strings.Contains(got, "skipping abrupt restart-requested kill for pinned named session") {
		t.Fatalf("stderr = %q, want pinned restart deferral diagnostic", got)
	}
}

func TestReconcileSessionBeads_RestartRequestAllowsExplicitResetForPinnedNamedSession(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "on_demand",
		"state":                      "active",
		"pin_awake":                  "true",
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
		"session_key":                "original-key",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	env.reconcile([]beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("explicit reset should still stop pinned named session %q", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared after explicit reset", got.Metadata["restart_requested"])
	}
	if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
		t.Fatalf("session_key = %q, want rotated after explicit reset", got.Metadata["session_key"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true until next start", got.Metadata["continuation_reset_pending"])
	}
}

// TestDoHandoff_PinnedAlwaysSessionPersistsResetAndReconcilerStopsSession
// covers Fix 1 end-to-end: doHandoffWithOutcome on a pinned always-mode
// session records the fenced row request (SG-3), which lands
// continuation_reset_pending on the session bead, and that persisted state must be exactly what lets the
// reconciler's explicit-reset escape hatch actually stop the pinned session
// on its next pass (mirrors
// TestReconcileSessionBeads_RestartRequestAllowsExplicitResetForPinnedNamedSession,
// but drives the persisted state through the CLI instead of setting it
// directly).
func TestDoHandoff_PinnedAlwaysSessionPersistsResetAndReconcilerStopsSession(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "mayor", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "mayor", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "mayor")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "mayor",
		ResolvedProvider: &config.ResolvedProvider{
			SessionIDFlag: "--session-id",
		},
	}

	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "mayor",
		namedSessionModeMetadata:     "always",
		"state":                      "active",
		"pin_awake":                  "true",
	})
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
		t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
	}

	dops := newDrainOps(env.sp)
	const token = "tok-pinned-live"
	env.setSessionMetadata(&session, map[string]string{"instance_token": token})
	restart := testRowFirstRestart("gc handoff", sessionName, dops, nil)
	restart.token = token
	restart.record = sessionRestartRecorder(t.TempDir(), env.store, env.sp, env.cfg, sessionName)

	var stdout, stderr bytes.Buffer
	outcome := doHandoffWithOutcome(env.store, env.store, env.rec, dops, restart,
		sessionName, sessionName, []string{"HANDOFF: context full"}, &stdout, &stderr)
	if outcome.code != 0 {
		t.Fatalf("code = %d, want 0; stderr: %s", outcome.code, stderr.String())
	}
	if !outcome.restartRequested {
		t.Fatal("restartRequested = false, want true for pinned always-mode session with a recorded restart")
	}
	if !strings.Contains(stdout.String(), "requesting restart") {
		t.Errorf("stdout = %q, want restart-requested confirmation", stdout.String())
	}

	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true after successful pinned handoff", got.Metadata["continuation_reset_pending"])
	}

	// The reconciler is the end-to-end oracle: with continuation_reset_pending
	// now landed, its explicit-reset escape hatch must actually stop the
	// pinned session on the next reconcile pass.
	env.reconcileWithPoolDesiredAndDrainOps([]beads.Bead{got}, map[string]int{"mayor": 1}, dops)
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("pinned session %q still running after reconcile; persisted restart should have let the reconciler stop it", sessionName)
	}
}

// TestReconcileSessionBeads_RestartRequestSetsAsleepOnlyWhenLiveRuntimeKilled
// pins ga-2fpf9z's second fix site: the restart-requested handoff in
// session_reconciler.go must add state=asleep to the SAME patch as the kill
// — but only on the branch that actually killed a live runtime. When the
// runtime was already dead going in, the deliberate same-tick fall-through
// (see the "Yield this tick" / #2345 comment, and
// TestReconcileSessionBeads_RestartRequestNamedAlwaysWakesSameTick) must stay
// byte-for-byte unchanged: this fix must never write "asleep" on that branch.
func TestReconcileSessionBeads_RestartRequestSetsAsleepOnlyWhenLiveRuntimeKilled(t *testing.T) {
	t.Run("live runtime killed", func(t *testing.T) {
		env := newRestartRequestTestEnv(t)
		env.cfg = &config.City{
			Workspace:     config.Workspace{Name: "test-city"},
			Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
			NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
		}
		sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
		env.desiredState[sessionName] = TemplateParams{
			Command:      "true",
			SessionName:  sessionName,
			TemplateName: "worker",
			ResolvedProvider: &config.ResolvedProvider{
				SessionIDFlag: "--session-id",
			},
		}

		session := env.createSessionBead(sessionName)
		env.setSessionMetadata(&session, map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "worker",
			namedSessionModeMetadata:     "on_demand",
			"state":                      "active",
			"restart_requested":          "true",
			"session_key":                "original-key",
			"started_config_hash":        "hash-before-restart",
		})
		if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
			t.Fatalf("start session: %v", err)
		}
		if err := env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID); err != nil {
			t.Fatalf("SetMeta(GC_SESSION_ID): %v", err)
		}

		// A single tick: runtimeRunning is true here, so the kill fires and
		// the tick yields (continue) before any wake decision runs. The
		// state left behind is therefore exactly what the restart-requested
		// patch wrote — nothing downstream has had a chance to touch it.
		env.reconcile([]beads.Bead{session})

		if env.sp.IsRunning(sessionName) {
			t.Fatalf("session %q still running after restart-requested kill", sessionName)
		}
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("store.Get(%s): %v", session.ID, err)
		}
		if got.Metadata["state"] != "asleep" {
			t.Fatalf("state = %q, want asleep — the handoff killed a live runtime and must not leave gc believing the session is still awake", got.Metadata["state"])
		}
		if got.Metadata["session_key"] == "" || got.Metadata["session_key"] == "original-key" {
			t.Fatalf("session_key = %q, want rotated key", got.Metadata["session_key"])
		}

		// Next tick: the recorded asleep state plus the reset-pending desire
		// must wake the on_demand session with no further trigger.
		env.reconcile([]beads.Bead{got})

		if !env.sp.IsRunning(sessionName) {
			t.Fatalf("session %q did not wake on the next tick after the restart-requested kill", sessionName)
		}
		woke, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("store.Get(%s) after wake: %v", session.ID, err)
		}
		if woke.Metadata["last_woke_at"] == "" {
			t.Fatal("last_woke_at after wake = empty, want a timestamp from a real wake commit")
		}
	})

	t.Run("already dead: fall-through unaffected", func(t *testing.T) {
		env := newRestartRequestTestEnv(t)
		env.cfg = &config.City{
			Workspace:     config.Workspace{Name: "test-city"},
			Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
			NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
		}
		sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
		env.desiredState[sessionName] = TemplateParams{
			Command:      "true",
			SessionName:  sessionName,
			TemplateName: "worker",
			ResolvedProvider: &config.ResolvedProvider{
				SessionIDFlag: "--session-id",
			},
		}

		session := env.createSessionBead(sessionName)
		env.setSessionMetadata(&session, map[string]string{
			namedSessionMetadataKey:      "true",
			namedSessionIdentityMetadata: "worker",
			namedSessionModeMetadata:     "always",
			"restart_requested":          "true",
			"session_key":                "original-key",
			"started_config_hash":        "hash-before-restart",
		})

		// Runtime is NOT started: the already-dead fall-through (mirrors
		// TestReconcileSessionBeads_RestartRequestNamedAlwaysWakesSameTick).
		if env.sp.IsRunning(sessionName) {
			t.Fatal("test fixture wrong: session should not be running")
		}

		env.reconcile([]beads.Bead{session})

		// Same-tick wake still fires — unchanged from before this fix.
		if !env.sp.IsRunning(sessionName) {
			t.Fatalf("session %q did not wake on the same reconciler tick; the already-dead fall-through must stay unchanged", sessionName)
		}
		got, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("store.Get(%s): %v", session.ID, err)
		}
		// The fix only ever adds "state" to the batch when runtimeRunning was
		// true at entry. Here it was false, so the restart-requested block
		// itself must never have written "asleep" — whatever state the
		// same-tick wake produced belongs to the (unchanged) wake decision,
		// not to this fix.
		if got.Metadata["state"] == "asleep" {
			t.Fatalf("state = %q, want NOT asleep — runtime was already dead, the restart-requested block must not have touched state on this branch", got.Metadata["state"])
		}
		if got.Metadata["last_woke_at"] == "" {
			t.Fatal("last_woke_at empty, want same-tick wake commit")
		}
	})
}

// newRestartRequestWakeFixture seeds a configured named worker session in
// mode always, the #5843 reporter's shape: a long-lived, pinned, configured
// session the reconciler keeps awake, whose wake a reset's handoff or a
// poked pass would run.
func newRestartRequestWakeFixture(t *testing.T, env *restartRequestTestEnv) (beads.Bead, string) {
	t.Helper()
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:                 "true",
		SessionName:             sessionName,
		TemplateName:            "worker",
		ConfiguredNamedIdentity: "worker",
		ConfiguredNamedMode:     "always",
	}
	// The row carries no reset markers and no session key of its own: a
	// request pair arrives only from the backing writes the tests make, and
	// the fresh wake has no key left to resume, so the PreWake must answer
	// it (a started_config_hash here makes the fixture's first pass repair
	// config drift instead of waking, so it stays off the fixture row too —
	// the start itself stamps the current hash).
	session := env.createSessionBead(sessionName)
	env.setSessionMetadata(&session, map[string]string{
		namedSessionMetadataKey:      "true",
		namedSessionIdentityMetadata: "worker",
		namedSessionModeMetadata:     "always",
		"state":                      "asleep",
		"sleep_reason":               "drained",
		// The reconciler stamps the sleep policy a row has not yet recorded,
		// and that local write's completion read installs the backing row into
		// the cache mid-pass. Pre-stamping the exact policy this fixture
		// resolves to keeps the pass from writing anything before its start,
		// so the backing sees no read of the row between the prime and the
		// start's own lease record.
		"requested_sleep_after_idle": "off",
		"effective_sleep_after_idle": "off",
		"sleep_policy_source":        "legacy_off",
		"sleep_capability":           "full",
		"sleep_policy_fingerprint":   "value=off|class=interactive_resume|source=legacy_off|wake=resume|cap=full|deps=|template=worker",
	})
	return session, sessionName
}

// rebaseFixtureConfigBaseline aligns the row's started-config baseline with
// the config-drift check's own current hash. The wake path resolves its
// launch template — its env and the startup prompt's fingerprint extras —
// far past what this file's hand-set desired state carries, so the start's
// own baseline never equals the hash the drift check computes from the
// desired state, and the pass after a start would read that gap as config
// drift and restart the replacement, drowning the restart request these
// tests isolate. The reconciler rebaselines the same fields itself when the
// stored hash is a version artifact; the fixture does the same for its
// baseline gap, so a later pass decides on the request alone.
func rebaseFixtureConfigBaseline(t *testing.T, env *restartRequestTestEnv, id, sessionName string) {
	t.Helper()
	row, err := env.store.Get(id)
	if err != nil {
		t.Fatalf("store get before rebasing the baseline: %v", err)
	}
	agentCfg := sessionCoreConfigForHashInfo(env.desiredState[sessionName], sessionInfoFromBead(row))
	if _, err := silentRebaselineSessionHashes(id, sessionFrontDoor(env.store), agentCfg, false); err != nil {
		t.Fatalf("rebasing the fixture baseline: %v", err)
	}
}

// TestRestartRequestSplitStoreSingleWave (ga-45119r AC-3, #5843): a start
// decided on the controller's stale cached row, whose backing row carries
// both reset markers, must consume the pair in its PreWake CAS — the mint's
// fenced read past the cache is the only read that sees the request — so no
// later pass stops the brand-new replacement (the report's two-wave
// restart).
func TestRestartRequestSplitStoreSingleWave(t *testing.T) {
	env, backing := newRestartRequestSplitStoreEnv(t)
	session, sessionName := newRestartRequestWakeFixture(t, env)
	env.primeCache(t)

	// The CLI's one-shot reset write: both markers straight to the backing,
	// around the controller's cache, with no event, feed or re-scan
	// delivering them.
	if err := backing.SetMetadataBatch(session.ID, map[string]string{
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
	}); err != nil {
		t.Fatalf("cli reset write: %v", err)
	}
	cached, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("cache get before the wake: %v", err)
	}
	if cached.Metadata["restart_requested"] == "true" {
		t.Fatalf("the cache already holds the reset marker; the fixture does not reproduce the stale row")
	}

	// The woken pass decides on the cached row — the restart block skips the
	// request — and its start runs the PreWake CAS, whose fresh read sees
	// the pair and must consume it whole.
	env.reconcile([]beads.Bead{session})

	got, err := backing.Get(session.ID)
	if err != nil {
		t.Fatalf("backing get after the wake: %v", err)
	}
	if got.Metadata["restart_requested"] != "" || got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("row after the PreWake CAS: restart_requested=%q continuation_reset_pending=%q, want both consumed", got.Metadata["restart_requested"], got.Metadata["continuation_reset_pending"])
	}
	if got.Metadata["generation"] != "2" {
		t.Fatalf("generation after the wake = %q, want the replacement's 2", got.Metadata["generation"])
	}
	if starts := env.sp.CountCalls("Start", sessionName); starts != 1 {
		t.Fatalf("starts of %s = %d, want the one fresh wake", sessionName, starts)
	}

	rebaseFixtureConfigBaseline(t, env, session.ID, sessionName)

	// A later pass must not stop the replacement: the request it answered is
	// gone, so the row's own incarnation never trips the restart block.
	later, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store get before the later pass: %v", err)
	}
	env.reconcile([]beads.Bead{later})
	if stops := env.sp.CountCalls("Stop", sessionName); stops != 0 {
		t.Fatalf("stops of %s = %d after a later pass, want none (the marker killed the replacement)", sessionName, stops)
	}
	if starts := env.sp.CountCalls("Start", sessionName); starts != 1 {
		t.Fatalf("starts of %s = %d after a later pass, want still the one", sessionName, starts)
	}
	if got.Metadata["generation"] != "2" {
		t.Fatalf("generation = %q after a later pass, want still the replacement's 2", got.Metadata["generation"])
	}
}

// TestRestartRequestAfterReplacementStopsOnce (ga-45119r AC-4, #5843): the
// PreWake consumes only the request its own start answers. A pair written
// after a replacement's CAS has landed is a request against that new
// incarnation: the reconciler stops it and restarts it fresh exactly once,
// and the second replacement is left running.
func TestRestartRequestAfterReplacementStopsOnce(t *testing.T) {
	env, _ := newRestartRequestSplitStoreEnv(t)
	session, sessionName := newRestartRequestWakeFixture(t, env)
	env.primeCache(t)

	// The reconciler's snapshot comes from the rows it is handed, so every
	// pass after the first must be given the row's current state.
	currentRow := func() beads.Bead {
		t.Helper()
		b, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("store get between passes: %v", err)
		}
		return b
	}

	env.reconcile([]beads.Bead{session}) // the first fresh wake, its CAS landed
	rebaseFixtureConfigBaseline(t, env, session.ID, sessionName)
	if starts := env.sp.CountCalls("Start", sessionName); starts != 1 {
		t.Fatalf("starts of %s = %d, want the one fresh wake", sessionName, starts)
	}

	// A new request pair, written after the replacement's PreWake CAS: the
	// next pass stops this incarnation and hands the row to a fresh wake.
	if err := env.store.SetMetadataBatch(session.ID, map[string]string{
		"restart_requested":          "true",
		"continuation_reset_pending": "true",
	}); err != nil {
		t.Fatalf("writing the new request pair: %v", err)
	}
	env.reconcile([]beads.Bead{currentRow()}) // the stop and its handoff
	if stops := env.sp.CountCalls("Stop", sessionName); stops != 1 {
		t.Fatalf("stops of %s = %d after the request pair, want exactly the one restart stop", sessionName, stops)
	}
	env.reconcile([]beads.Bead{currentRow()}) // the fresh wake
	rebaseFixtureConfigBaseline(t, env, session.ID, sessionName)
	if starts := env.sp.CountCalls("Start", sessionName); starts != 2 {
		t.Fatalf("starts of %s = %d, want the replacement and its fresh restart", sessionName, starts)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store get after the restart: %v", err)
	}
	if got.Metadata["restart_requested"] != "" || got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("row after the restart: restart_requested=%q continuation_reset_pending=%q, want both consumed", got.Metadata["restart_requested"], got.Metadata["continuation_reset_pending"])
	}

	// The second replacement is left running: no later pass stops it again.
	env.reconcile([]beads.Bead{currentRow()})
	if stops := env.sp.CountCalls("Stop", sessionName); stops != 1 {
		t.Fatalf("stops of %s = %d after the restart, want still the one", sessionName, stops)
	}
	if starts := env.sp.CountCalls("Start", sessionName); starts != 2 {
		t.Fatalf("starts of %s = %d after the restart, want still two", sessionName, starts)
	}
}
