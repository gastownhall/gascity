package main

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// restartCollisionProvider models the #6417 race: after a requested restart
// kills the runtime, the next Start finds a runtime under the same name that
// the reconciler did not launch (it never received the start's prompt), and
// refuses with ErrSessionExists. The intruder stays live until Stop.
type restartCollisionProvider struct {
	*runtime.Fake
	collideNextStart bool
	intruder         map[string]bool
	intruderMeta     map[string]map[string]string
}

func (p *restartCollisionProvider) Start(ctx context.Context, name string, cfg runtime.Config) error {
	if p.collideNextStart {
		p.collideNextStart = false
		p.intruder[name] = true
		return runtime.ErrSessionExists
	}
	if p.intruder[name] {
		return runtime.ErrSessionExists
	}
	return p.Fake.Start(ctx, name, cfg)
}

func (p *restartCollisionProvider) Stop(name string) error {
	delete(p.intruder, name)
	return p.Fake.Stop(name)
}

func (p *restartCollisionProvider) IsRunning(name string) bool {
	return p.intruder[name] || p.Fake.IsRunning(name)
}

func (p *restartCollisionProvider) ProcessAlive(name string, processNames []string) bool {
	return p.intruder[name] || p.Fake.ProcessAlive(name, processNames)
}

func (p *restartCollisionProvider) GetMeta(name, key string) (string, error) {
	if p.intruder[name] {
		return p.intruderMeta[name][key], nil
	}
	return p.Fake.GetMeta(name, key)
}

// TestReconcileSessionBeads_RestartRequestStartCollisionDoesNotLeavePromptlessRuntime
// reproduces #6417: a requested restart of a named session whose next Start
// collides with ErrSessionExists must not settle on the colliding runtime (it
// never got the rendered prompt). The reconciler must eventually launch its
// own runtime with the start config, i.e. a successful provider Start.
func TestReconcileSessionBeads_RestartRequestStartCollisionDoesNotLeavePromptlessRuntime(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		Prompt:       "rendered template prompt",
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
		"session_key":                "original-key",
		"started_config_hash":        "hash-before-restart",
	})
	ctx := context.Background()
	if err := env.sp.Start(ctx, sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	_ = env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID)
	_ = env.sp.SetMeta(sessionName, "GC_RESTART_REQUESTED", "1")

	sp := &restartCollisionProvider{
		Fake:         env.sp,
		intruder:     map[string]bool{},
		intruderMeta: map[string]map[string]string{sessionName: {"GC_SESSION_ID": session.ID, "GC_INSTANCE_TOKEN": "test-token"}},
	}
	tick := func() {
		b, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get(session): %v", err)
		}
		cfgNames := configuredSessionNames(env.cfg, "", env.store)
		_ = reconcileSessionBeadsAtPath(
			ctx, env.city, []beads.Bead{b}, env.desiredState, cfgNames, env.cfg, sp, env.store,
			newDrainOps(sp), nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
		)
		env.clk.Time = env.clk.Time.Add(time.Minute)
	}

	// Tick 1: the restart request kills the live runtime and yields.
	tick()
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("tick 1 did not stop the restart-requested runtime; stdout=%s stderr=%s", env.stdout.String(), env.stderr.String())
	}
	// Tick 2: the wake's Start collides with a runtime it did not launch.
	sp.collideNextStart = true
	callsBefore := len(env.sp.Calls)
	tick()
	if sp.collideNextStart {
		t.Fatalf("fixture did not exercise the collision: no Start reached the provider after the restart\nstdout=%s\nstderr=%s", env.stdout.String(), env.stderr.String())
	}

	// The colliding runtime never received the start's prompt. The reconciler
	// may replace it, deliver the prompt to it, or leave the session pending a
	// retry, but it must not record it as this session's woken runtime with
	// no prompt delivered: nothing would ever revisit it.
	promptDelivered := false
	for _, c := range env.sp.Calls[callsBefore:] {
		if c.Name == sessionName && (c.Method == "Nudge" || c.Method == "NudgeNow" || c.Method == "NudgeFenced") {
			promptDelivered = true
		}
	}
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	t.Logf("after collision tick: intruder=%v promptDelivered=%v state=%q last_woke_at=%q", sp.intruder[sessionName], promptDelivered, b.Metadata["state"], b.Metadata["last_woke_at"])
	if sp.intruder[sessionName] && !promptDelivered && b.Metadata["state"] == "active" && b.Metadata["last_woke_at"] != "" {
		t.Fatalf("restart start collided with ErrSessionExists and the reconciler adopted the colliding runtime as woken (state=%q, last_woke_at=%q) without delivering the prompt: a live, promptless session nothing retries\nstdout=%s\nstderr=%s",
			b.Metadata["state"], b.Metadata["last_woke_at"], env.stdout.String(), env.stderr.String())
	}
}
