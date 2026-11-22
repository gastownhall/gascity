package main

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
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

// restartHandoffRace is the fixture of AC-8 of gastownhall/gascity#6417: a
// named session whose template renders a startup prompt, live under a
// committed start with a restart requested, so the first tick's handoff kills
// its runtime and opens the gap a non-controller start can reach before the
// controller's own start does.
type restartHandoffRace struct {
	env     *restartRequestTestEnv
	name    string
	session beads.Bead
	prompt  string
}

func newRestartHandoffRace(t *testing.T) *restartHandoffRace {
	t.Helper()
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
	if err := env.sp.Start(context.Background(), sessionName, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("start session: %v", err)
	}
	_ = env.sp.SetMeta(sessionName, "GC_SESSION_ID", session.ID)
	_ = env.sp.SetMeta(sessionName, "GC_RESTART_REQUESTED", "1")
	return &restartHandoffRace{env: env, name: sessionName, session: session, prompt: renderedPrompt}
}

// row is the session row as the store holds it now.
func (r *restartHandoffRace) row(t *testing.T) map[string]string {
	t.Helper()
	b, err := r.env.store.Get(r.session.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	return b.Metadata
}

// tick runs one reconciler pass over the row as the store holds it, then
// moves the clock 90s on, past the default 60s startup window.
func (r *restartHandoffRace) tick(t *testing.T) {
	t.Helper()
	env := r.env
	b, err := env.store.Get(r.session.ID)
	if err != nil {
		t.Fatalf("Get(session): %v", err)
	}
	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	_ = reconcileSessionBeadsAtPath(
		context.Background(), env.city, []beads.Bead{b}, env.desiredState, cfgNames, env.cfg, env.sp, env.store,
		newDrainOps(env.sp), nil, nil, nil, env.dt, map[string]int{"worker": 1}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr, env.startOptions...,
	)
	env.clk.Time = env.clk.Time.Add(90 * time.Second)
}

// handOff runs phase 1, the requested restart: the handoff kills the runtime,
// clears the start witness and yields the tick. It returns the handed-off row.
func (r *restartHandoffRace) handOff(t *testing.T) map[string]string {
	t.Helper()
	r.tick(t)
	if r.env.sp.IsRunning(r.name) {
		t.Fatalf("the restart handoff did not stop the runtime\nstdout=%s\nstderr=%s", r.env.stdout.String(), r.env.stderr.String())
	}
	row := r.row(t)
	if strings.TrimSpace(row["reset_committed_at"]) == "" || strings.TrimSpace(row["started_config_hash"]) != "" {
		t.Fatalf("fixture: the handoff did not commit: reset_committed_at=%q started_config_hash=%q", row["reset_committed_at"], row["started_config_hash"])
	}
	return row
}

// primed reports whether the row's runtime is one the controller launched
// with the startup prompt (as the resume nudge or in argv) and whose start
// committed a started_config_hash of its own.
func (r *restartHandoffRace) primed(t *testing.T) bool {
	t.Helper()
	if !r.env.sp.IsRunning(r.name) {
		return false
	}
	cfg := r.env.sp.LastStartConfig(r.name)
	if cfg == nil || (!strings.Contains(cfg.Nudge, r.prompt) && !strings.Contains(cfg.PromptSuffix, r.prompt)) {
		return false
	}
	hash := strings.TrimSpace(r.row(t)["started_config_hash"])
	return hash != "" && hash != "hash-before-restart"
}

// requirePrimed drives up to eight ticks and fails unless the row ends primed
// by a controller launch.
func (r *restartHandoffRace) requirePrimed(t *testing.T) {
	t.Helper()
	for i := 0; i < 8 && !r.primed(t); i++ {
		r.tick(t)
	}
	if r.primed(t) {
		return
	}
	var lastNudge, lastPromptSuffix string
	if cfg := r.env.sp.LastStartConfig(r.name); cfg != nil {
		lastNudge, lastPromptSuffix = cfg.Nudge, cfg.PromptSuffix
	}
	row := r.row(t)
	t.Fatalf("the session never ended primed by a controller launch after the restart handoff\nrunning=%v started_config_hash=%q reset_committed_at=%q last start nudge=%q prompt suffix=%q\nstdout=%s\nstderr=%s",
		r.env.sp.IsRunning(r.name), row["started_config_hash"], row["reset_committed_at"], lastNudge, lastPromptSuffix, r.env.stdout.String(), r.env.stderr.String())
}

// TestRestartHandoff_ManagerStartRaceEndsPrimed is AC-8 of
// gastownhall/gascity#6417, the end-to-end regression: after a requested
// restart's handoff opens the gap, a non-controller start reaches the row
// before the controller's start does: here `gc nudge`'s own path, a worker
// Nudge as ActorAgent through Manager.Send and ensureDeliverable. That start
// must not launch the stored resume command into the gap (a runtime without
// the startup prompt, whose mint would consume the handoff's
// reset_committed_at and leave the uncommitted-start eviction nothing to key
// on): it queues its text and leaves the row as the handoff wrote it. The
// controller's own start then launches the runtime with the startup prompt
// and commits its started_config_hash, and the queued text still waits, for
// the conversation that start opened.
func TestRestartHandoff_ManagerStartRaceEndsPrimed(t *testing.T) {
	r := newRestartHandoffRace(t)
	env := r.env
	handedOff := r.handOff(t)

	// Phase 2 — the race: the nudge reaches the row in the gap.
	h, err := workerHandleForSessionWithConfig(env.city, env.store, env.sp, env.cfg, r.session.ID)
	if err != nil {
		t.Fatalf("worker handle: %v", err)
	}
	startsBefore := env.sp.CountCalls("Start", r.name)
	res, err := h.Nudge(context.Background(), sessionActor(sessionpkg.ActorAgent, env.city), worker.NudgeRequest{Text: "you have mail", Source: "session"})
	if err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if res.Delivered || res.Undelivered != worker.NudgeQueued {
		t.Fatalf("Nudge = %+v, want the text queued behind the controller's start", res)
	}
	if got := env.sp.CountCalls("Start", r.name) - startsBefore; got != 0 || env.sp.IsRunning(r.name) {
		t.Fatalf("the nudge launched %d runtime(s) into the restart gap (running=%v)", got, env.sp.IsRunning(r.name))
	}
	if got := r.row(t); !maps.Equal(got, handedOff) {
		t.Fatalf("the nudge rewrote the handed-off row:\n got %v\nwant %v", got, handedOff)
	}

	// Phase 3 — the controller's start serves the handoff.
	r.requirePrimed(t)

	// The queued text is fenced by its session alone, so the epoch the
	// controller's start advanced does not fence it off.
	state, err := nudgequeue.LoadState(env.city)
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	var queued []nudgequeue.Item
	for _, item := range state.Pending {
		if item.SessionID == r.session.ID {
			queued = append(queued, item)
		}
	}
	if len(queued) != 1 || queued[0].Message != "you have mail" || queued[0].ContinuationEpoch != "" {
		t.Fatalf("queued = %+v, want the nudge's text pending for the session with no epoch fence", queued)
	}
}

// TestRestartHandoff_UnmintedOccupantIsReplacedPrimed is the uncommitted-start
// eviction's end-to-end backstop (#6417 AC-6): a runtime under the row's name
// that came up in the restart gap without any Manager start's mint (planted
// on the provider directly, carrying the row's GC_SESSION_ID) leaves the
// handoff's reset_committed_at standing. The controller's start refuses that
// occupant (deferred, nothing launched or stopped), the eviction replaces it
// once the startup window has passed, and the next tick's controller launch
// carries the startup prompt and commits.
func TestRestartHandoff_UnmintedOccupantIsReplacedPrimed(t *testing.T) {
	r := newRestartHandoffRace(t)
	r.handOff(t)

	if err := r.env.sp.Start(context.Background(), r.name, runtime.Config{Command: "true"}); err != nil {
		t.Fatalf("planting the occupant: %v", err)
	}
	_ = r.env.sp.SetMeta(r.name, "GC_SESSION_ID", r.session.ID)

	r.requirePrimed(t)
	if !strings.Contains(r.env.stderr.String(), "evicted "+r.name) {
		t.Fatalf("the occupant was not replaced through the uncommitted-start eviction\nstderr=%s", r.env.stderr.String())
	}
}
