package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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

// TestRestartHandoff_ManagerStartRaceEndsPrimed is AC-8 of
// gastownhall/gascity#6417, the end-to-end regression: after a requested
// restart's handoff opens the gap, a non-controller start (a Manager send
// through ensureDeliverable) launches the stored resume command into it before
// the controller's start does, and the controller's start then collides. The
// reconciler must not adopt the promptless occupant: once the startup window
// has passed, the row's final runtime is one the controller launched and
// primed with the startup prompt, and its started_config_hash is committed.
func TestRestartHandoff_ManagerStartRaceEndsPrimed(t *testing.T) {
	env := newRestartRequestTestEnv(t)
	const renderedPrompt = "rendered template prompt"
	// The agent's prompt template file: the named session's wake re-renders
	// its template from config, so the row's final restart carries the real
	// rendered startup prompt, exactly as the reporter's city did.
	promptFile := filepath.Join(t.TempDir(), "worker-prompt.md")
	if err := os.WriteFile(promptFile, []byte(renderedPrompt), 0o600); err != nil {
		t.Fatalf("writing the prompt template: %v", err)
	}
	env.cfg = &config.City{
		Workspace:     config.Workspace{Name: "test-city"},
		Agents:        []config.Agent{{Name: "worker", StartCommand: "true", PromptTemplate: promptFile, MaxActiveSessions: restartRequestTestIntPtr(1)}},
		NamedSessions: []config.NamedSession{{Template: "worker", Mode: "always"}},
	}
	sessionName := config.NamedSessionRuntimeName(env.cfg.Workspace.Name, env.cfg.Workspace, "worker")
	env.desiredState[sessionName] = TemplateParams{
		Command:      "true",
		Prompt:       renderedPrompt,
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

	tick := func() {
		b, err := env.store.Get(session.ID)
		if err != nil {
			t.Fatalf("Get(session): %v", err)
		}
		cfgNames := configuredSessionNames(env.cfg, "", env.store)
		_ = reconcileSessionBeadsAtPath(
			ctx, env.city, []beads.Bead{b}, env.desiredState, cfgNames, env.cfg, env.sp, env.store,
			newDrainOps(env.sp), nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
			nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
		)
		env.clk.Time = env.clk.Time.Add(90 * time.Second)
	}

	// Phase 1 — the requested restart: the handoff kills the runtime, clears
	// the start witness and yields the tick.
	tick()
	if env.sp.IsRunning(sessionName) {
		t.Fatalf("the restart handoff did not stop the runtime\nstdout=%s\nstderr=%s", env.stdout.String(), env.stderr.String())
	}

	// Phase 2 — the race: a Manager send (ensureDeliverable's mintStart
	// consuming the reset and launching the stored resume command) fills the
	// gap before the controller's start does. Forced deterministically as the
	// occupant it leaves: the row's own runtime, launched without the prompt.
	if err := env.sp.Start(ctx, sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("planting the non-controller launch: %v", err)
	}
	_ = env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID)

	// Phase 3 — drive ticks past the startup window: the controller's start
	// refuses the occupant (deferred, nothing launched or stopped), the
	// uncommitted-start eviction replaces it, and the next tick is the
	// controller's own launch carrying the startup prompt.
	primed := func() bool {
		if !env.sp.IsRunning(sessionName) {
			return false
		}
		// The launch carries the prompt as the resume nudge or in argv.
		cfg := env.sp.LastStartConfig(sessionName)
		if cfg == nil || (!strings.Contains(cfg.Nudge, renderedPrompt) && !strings.Contains(cfg.PromptSuffix, renderedPrompt)) {
			return false
		}
		b, err := env.store.Get(session.ID)
		return err == nil && strings.TrimSpace(b.Metadata["started_config_hash"]) != "" && strings.TrimSpace(b.Metadata["started_config_hash"]) != "hash-before-restart"
	}
	for i := 0; i < 8 && !primed(); i++ {
		tick()
	}
	if !primed() {
		var lastNudge, lastPromptSuffix string
		if cfg := env.sp.LastStartConfig(sessionName); cfg != nil {
			lastNudge, lastPromptSuffix = cfg.Nudge, cfg.PromptSuffix
		}
		t.Fatalf("the session never ended primed by a controller launch after the Manager-start race\nrunning=%v last start nudge=%q prompt suffix=%q\nstdout=%s\nstderr=%s",
			env.sp.IsRunning(sessionName), lastNudge, lastPromptSuffix, env.stdout.String(), env.stderr.String())
	}
	b, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	if got := strings.TrimSpace(b.Metadata["started_config_hash"]); got == "" || got == "hash-before-restart" {
		t.Fatalf("started_config_hash = %q, want the controller's own start committed", got)
	}
}
