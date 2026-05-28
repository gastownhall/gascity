package main

import (
	"bytes"
	"context"
	"errors"
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
}

func newRestartRequestTestEnv() *restartRequestTestEnv {
	return &restartRequestTestEnv{
		store:        beads.NewMemStore(),
		sp:           runtime.NewFake(),
		dt:           newDrainTracker(),
		clk:          &clock.Fake{Time: time.Date(2026, 3, 8, 12, 0, 0, 0, time.UTC)},
		rec:          events.Discard,
		cfg:          &config.City{},
		desiredState: make(map[string]TemplateParams),
	}
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
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	_ = reconcileSessionBeads(
		context.Background(),
		sessions,
		e.desiredState,
		cfgNames,
		e.cfg,
		e.sp,
		e.store,
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
	)
}

func TestReconcileSessionBeads_RestartRequestRotatesKeyForSessionIDProviders(t *testing.T) {
	env := newRestartRequestTestEnv()
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
	env := newRestartRequestTestEnv()
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
	env := newRestartRequestTestEnv()
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

func TestReconcileSessionBeads_RestartRequestPreservesIntentWhenKillFails(t *testing.T) {
	env := newRestartRequestTestEnv()
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
	env := newRestartRequestTestEnv()
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
	restore := setSessionCircuitBreakerForTest(cb)
	defer restore()

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
	if err := persistSessionCircuitBreakerMetadata(env.store, &session, cb, identity, base); err != nil {
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
	env := newRestartRequestTestEnv()
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
	env := newRestartRequestTestEnv()
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
