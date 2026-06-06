package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

type assignedWorkListErrorStore struct {
	beads.Store
	err error
}

func (s *assignedWorkListErrorStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Assignee != "" && (query.Status == "open" || query.Status == "in_progress") {
		return nil, s.err
	}
	return s.Store.List(query)
}

type sessionObservationGetErrorStore struct {
	beads.Store
	id        string
	remaining int
	err       error
}

func (s *sessionObservationGetErrorStore) Get(id string) (beads.Bead, error) {
	if id == s.id && s.remaining > 0 {
		s.remaining--
		return beads.Bead{}, s.err
	}
	return s.Store.Get(id)
}

func newProgressStallTestEnv(t *testing.T) (*restartRequestTestEnv, beads.Bead, string) {
	t.Helper()

	env := newRestartRequestTestEnv()
	env.cfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Session: config.SessionConfig{
			ProgressStallTimeout: "30m",
			StartupTimeout:       "60s",
		},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "on_demand"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		SessionName:  sessionName,
		TemplateName: "worker",
		ResolvedProvider: &config.ResolvedProvider{
			Name:          "zai",
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
	env.sp.SetActivity(sessionName, env.clk.Now().Add(-time.Hour))

	return env, session, sessionName
}

func (e *restartRequestTestEnv) reconcileAtPath(cityPath string, sessions []beads.Bead) {
	e.reconcileAtPathWithProvider(cityPath, e.sp, sessions)
}

func (e *restartRequestTestEnv) reconcileAtPathWithProvider(cityPath string, sp runtime.Provider, sessions []beads.Bead) {
	poolDesired := make(map[string]int)
	for _, tp := range e.desiredState {
		if tp.TemplateName != "" {
			poolDesired[tp.TemplateName]++
		}
	}
	cfgNames := configuredSessionNames(e.cfg, "", e.store)
	_ = reconcileSessionBeadsAtPath(
		context.Background(),
		cityPath,
		sessions,
		e.desiredState,
		cfgNames,
		e.cfg,
		sp,
		e.store,
		nil,
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

func TestReconcileSessionBeads_ProgressStallRecyclesStaleClaimlessHealthySession(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; stale claim-less session should be recycled", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want cleared after restart handoff", got.Metadata["restart_requested"])
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if !strings.Contains(env.stderr.String(), "progress-stalled") {
		t.Fatalf("stderr = %q, want progress-stalled diagnostic", env.stderr.String())
	}
}

func TestReconcileSessionBeads_ProgressStallRecyclesWithOpenAssignedWork(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	work, err := env.store.Create(beads.Bead{
		Title:    "ready work not yet claimed",
		Type:     "task",
		Assignee: sessionName,
	})
	if err != nil {
		t.Fatalf("Create(work): %v", err)
	}

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; open assigned work is not a held claim", sessionName)
	}
	gotWork, err := env.store.Get(work.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", work.ID, err)
	}
	if gotWork.Status != "open" {
		t.Fatalf("work status = %q, want open", gotWork.Status)
	}
	if gotWork.Assignee != sessionName {
		t.Fatalf("work assignee = %q, want %q", gotWork.Assignee, sessionName)
	}
}

func TestReconcileSessionBeads_ProgressStallDoesNotRecycleExemptOrSafeSessions(t *testing.T) {
	tests := []struct {
		name      string
		cityPath  func(t *testing.T) string
		configure func(t *testing.T, env *restartRequestTestEnv, session *beads.Bead, sessionName string)
		provider  func(env *restartRequestTestEnv) runtime.Provider
		wantLog   string
	}{
		{
			name: "attached session",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, sessionName string) {
				env.sp.SetAttached(sessionName, true)
			},
		},
		{
			name: "claim check error fails safe",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, _ string) {
				env.store = &assignedWorkListErrorStore{Store: env.store, err: errors.New("assigned work query failed")}
			},
			wantLog: "checking assigned work before progress-stall recycle",
		},
		{
			name: "attachment check error fails safe",
			configure: func(_ *testing.T, env *restartRequestTestEnv, session *beads.Bead, _ string) {
				env.store = &sessionObservationGetErrorStore{
					Store:     env.store,
					id:        session.ID,
					remaining: 1,
					err:       errors.New("attachment observation failed"),
				}
			},
			wantLog: "checking attachment before progress-stall recycle",
		},
		{
			name: "in-progress assigned work",
			configure: func(t *testing.T, env *restartRequestTestEnv, _ *beads.Bead, sessionName string) {
				t.Helper()
				work, err := env.store.Create(beads.Bead{
					Title:    "claimed work",
					Type:     "task",
					Assignee: sessionName,
				})
				if err != nil {
					t.Fatalf("Create(work): %v", err)
				}
				status := "in_progress"
				if err := env.store.Update(work.ID, beads.UpdateOpts{Status: &status}); err != nil {
					t.Fatalf("Update(work): %v", err)
				}
			},
		},
		{
			name: "provider health red",
			cityPath: func(t *testing.T) string {
				dir := t.TempDir()
				writeHealthCache(t, dir, "zai", "unhealthy", nowSecs())
				return dir
			},
		},
		{
			name: "recent provider activity",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, sessionName string) {
				env.sp.SetActivity(sessionName, env.clk.Now().Add(-time.Minute))
			},
		},
		{
			name: "unknown provider activity fails safe",
			provider: func(env *restartRequestTestEnv) runtime.Provider {
				return capabilityOverrideProvider{
					Provider: env.sp,
					caps: runtime.ProviderCapabilities{
						CanReportAttachment: true,
						CanReportActivity:   false,
					},
					sleepCap: runtime.SessionSleepCapabilityTimedOnly,
				}
			},
		},
		{
			name: "startup in-flight lease",
			configure: func(_ *testing.T, env *restartRequestTestEnv, session *beads.Bead, _ string) {
				env.setSessionMetadata(session, map[string]string{
					"pending_create_claim": "true",
					"state":                string(sessionpkg.StateCreating),
					"last_woke_at":         env.clk.Now().UTC().Format(time.RFC3339),
				})
			},
		},
		{
			name: "timeout below enforced minimum",
			configure: func(_ *testing.T, env *restartRequestTestEnv, _ *beads.Bead, sessionName string) {
				env.cfg.Session.ProgressStallTimeout = "30s"
				env.sp.SetActivity(sessionName, env.clk.Now().Add(-time.Minute))
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			env, session, sessionName := newProgressStallTestEnv(t)
			cityPath := t.TempDir()
			if tc.cityPath != nil {
				cityPath = tc.cityPath(t)
			}
			if tc.configure != nil {
				tc.configure(t, env, &session, sessionName)
			}
			sp := runtime.Provider(env.sp)
			if tc.provider != nil {
				sp = tc.provider(env)
			}

			env.reconcileAtPathWithProvider(cityPath, sp, []beads.Bead{session})

			if !env.sp.IsRunning(sessionName) {
				t.Fatalf("session %q was recycled; want it left running", sessionName)
			}
			got, err := env.store.Get(session.ID)
			if err != nil {
				t.Fatalf("store.Get(%s): %v", session.ID, err)
			}
			if got.Metadata["continuation_reset_pending"] != "" {
				t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
			}
			if strings.Contains(env.stderr.String(), "progress-stalled") {
				t.Fatalf("stderr = %q, want no progress-stalled diagnostic", env.stderr.String())
			}
			if tc.wantLog != "" && !strings.Contains(env.stderr.String(), tc.wantLog) {
				t.Fatalf("stderr = %q, want %q", env.stderr.String(), tc.wantLog)
			}
		})
	}
}

// TestReconcileSessionBeads_ProgressStallExemptsMinFloorIdleWorker drives the
// reconciler's pool-counting branch (not just the extracted predicate): a stale,
// claimless, healthy session whose pool is at its configured floor
// (min_active_sessions == open == 1) must be left running. The floor worker is
// waiting for routed work, not parked on an error, so it is exempt from the
// progress-stall recycler.
func TestReconcileSessionBeads_ProgressStallExemptsMinFloorIdleWorker(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	env.cfg.Agents[0].MinActiveSessions = restartRequestTestIntPtr(1)

	// Pool at floor: this single open session is the entire always-warm
	// contingent (open == min == 1).
	env.reconcileAtPath(t.TempDir(), []beads.Bead{session})

	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q was recycled; floor worker at pool floor must be exempt", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want empty for exempt floor worker", got.Metadata["restart_requested"])
	}
	if got.Metadata["continuation_reset_pending"] != "" {
		t.Fatalf("continuation_reset_pending = %q, want empty", got.Metadata["continuation_reset_pending"])
	}
	if strings.Contains(env.stderr.String(), "progress-stalled") {
		t.Fatalf("stderr = %q, want no progress-stalled diagnostic", env.stderr.String())
	}
}

// TestReconcileSessionBeads_ProgressStallRecyclesAboveFloorWorker is the
// counter-case proving the floor exemption is floor-bounded, not blanket: with
// the same min_active_sessions floor of 1 but two open sessions in the pool
// (open == 2 > min == 1), a stale claimless session is above the always-warm
// contingent and IS recycled.
func TestReconcileSessionBeads_ProgressStallRecyclesAboveFloorWorker(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	env.cfg.Agents[0].MinActiveSessions = restartRequestTestIntPtr(1)
	env.cfg.Agents[0].MaxActiveSessions = restartRequestTestIntPtr(2)

	// A second open worker session lifts the pool above its floor (open == 2 >
	// min == 1), so the stale session under test is no longer floor-protected.
	companion := env.createSessionBead("worker-floor-companion")

	env.reconcileAtPath(t.TempDir(), []beads.Bead{session, companion})

	if env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q still running; above-floor stale claimless session should be recycled", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["continuation_reset_pending"] != "true" {
		t.Fatalf("continuation_reset_pending = %q, want true", got.Metadata["continuation_reset_pending"])
	}
	if !strings.Contains(env.stderr.String(), "progress-stalled") {
		t.Fatalf("stderr = %q, want progress-stalled diagnostic", env.stderr.String())
	}
}
