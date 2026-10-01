package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// contractPromptBody is a distinctive startup prompt body; every launch builder
// must carry it to the session through exactly one mechanism.
const contractPromptBody = "CONTRACT PROMPT BODY: claim your assigned work and run it to completion."

// launchMode names the three ways a launch relates to a session's conversation.
// The reconciler derives its create-vs-resume decision from these session
// fields; the worker resolvers see the same fields on session.Info.
type launchMode struct {
	name        string
	startedHash string
	sessionKey  string
	wakeMode    string
}

var launchModes = []launchMode{
	{name: "first start"},
	{name: "resume with key", startedHash: "cfg", sessionKey: "warm"},
	{name: "forceFresh", startedHash: "cfg", sessionKey: "warm", wakeMode: "fresh"},
}

// contractCity is a city with one agent named worker. promptBody is the agent's
// prompt_template content; empty means the agent has no prompt template.
type contractCity struct {
	path     string
	cfg      *config.City
	resolved *config.ResolvedProvider
}

func newContractCity(t *testing.T, promptBody string) contractCity {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := config.Agent{Name: "worker", Provider: "claude"}
	if promptBody != "" {
		if err := os.MkdirAll(filepath.Join(cityPath, "prompts"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cityPath, "prompts", "worker.md"), []byte(promptBody), 0o644); err != nil {
			t.Fatal(err)
		}
		agent.PromptTemplate = "prompts/worker.md"
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{agent},
		Providers: map[string]config.ProviderSpec{"claude": config.BuiltinProviders()["claude"]},
	}
	resolved, err := config.ResolveProvider(&cfg.Agents[0], &cfg.Workspace, cfg.Providers, func(name string) (string, error) { return name, nil })
	if err != nil {
		t.Fatalf("ResolveProvider: %v", err)
	}
	return contractCity{path: cityPath, cfg: cfg, resolved: resolved}
}

func (c contractCity) sessionInfo(mode launchMode) session.Info {
	return session.Info{
		Template:          "worker",
		WorkDir:           c.path,
		SessionKey:        mode.sessionKey,
		StartedConfigHash: mode.startedHash,
		WakeMode:          mode.wakeMode,
	}
}

// launchBuilder builds the runtime.Config of one gc-managed launch of the
// contract city's worker session.
type launchBuilder struct {
	name  string
	build func(t *testing.T, c contractCity, mode launchMode, prompt string) runtime.Config
}

var launchBuilders = []launchBuilder{
	{name: "reconciler", build: reconcilerLaunch},
	{name: "CLI worker resume", build: cliWorkerResumeLaunch},
	{name: "API worker resume", build: apiWorkerResumeLaunch},
}

// reconcilerLaunch covers both reconciler launch builders: buildPreparedStart
// creates or resumes depending on the session bead's state.
func reconcilerLaunch(t *testing.T, _ contractCity, mode launchMode, prompt string) runtime.Config {
	t.Helper()
	store := beads.NewMemStore()
	meta := map[string]string{"session_name": "worker", "template": "worker", "state": "asleep"}
	if mode.startedHash != "" {
		meta["started_config_hash"] = mode.startedHash
	}
	if mode.sessionKey != "" {
		meta["session_key"] = mode.sessionKey
	}
	if mode.wakeMode != "" {
		meta["wake_mode"] = mode.wakeMode
	}
	bead, err := store.Create(beads.Bead{Title: "worker", Type: sessionBeadType, Labels: []string{sessionBeadLabel}, Metadata: meta})
	if err != nil {
		t.Fatalf("Create(session): %v", err)
	}
	candidate := startCandidate{
		info: sessiontest.SeedBead(t, bead),
		tp:   TemplateParams{TemplateName: "worker", SessionName: "worker", Command: "claude", Prompt: prompt},
	}
	prepared, _, err := buildPreparedStart(candidate, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	return prepared.cfg
}

func cliWorkerResumeLaunch(t *testing.T, c contractCity, mode launchMode, _ string) runtime.Config {
	t.Helper()
	resolved, err := resolvedWorkerRuntimeWithConfig(c.path, c.cfg, c.sessionInfo(mode), "")
	if err != nil {
		t.Fatalf("resolvedWorkerRuntimeWithConfig: %v", err)
	}
	if resolved == nil {
		t.Fatal("resolvedWorkerRuntimeWithConfig() = nil")
	}
	return resolved.Hints
}

func apiWorkerResumeLaunch(t *testing.T, c contractCity, mode launchMode, _ string) runtime.Config {
	t.Helper()
	cs := newControllerState(context.Background(), c.cfg, runtime.NewFake(), events.NewFake(), "test-city", c.path)
	hints := runtime.Config{WorkDir: c.path, Env: map[string]string{"KEEP": "me"}}
	got, err := cs.ApplyStartupPrompt(c.sessionInfo(mode), c.resolved, "", hints)
	if err != nil {
		t.Fatalf("ApplyStartupPrompt: %v", err)
	}
	return got
}

// carriedThroughArgv reports whether the prompt rides in the launch command's
// argv suffix, unquoting it the way a shell would.
func carriedThroughArgv(cfg runtime.Config, prompt string) bool {
	if cfg.PromptSuffix == "" {
		return false
	}
	parts := shellquote.Split(cfg.PromptSuffix)
	return len(parts) > 0 && strings.Contains(parts[0], prompt)
}

// TestStartupPromptContractEveryLaunchBuilderDeliversPrompt is the guard against
// the launch-population gap (ga-4k4zfk): for a prompted agent, every gc-managed
// launch builder, in every launch mode, must carry the prompt through exactly one
// of argv or the nudge AND set the delivered marker. The forbidden combination
// is a non-empty prompt with no payload and no marker: that launch leaves the
// SessionStart hook as the only carrier, and the provider truncates a large hook
// payload.
func TestStartupPromptContractEveryLaunchBuilderDeliversPrompt(t *testing.T) {
	city := newContractCity(t, contractPromptBody)
	for _, builder := range launchBuilders {
		for _, mode := range launchModes {
			t.Run(builder.name+"/"+mode.name, func(t *testing.T) {
				cfg := builder.build(t, city, mode, contractPromptBody)

				argv := carriedThroughArgv(cfg, contractPromptBody)
				nudge := strings.Contains(cfg.Nudge, contractPromptBody)
				marked := cfg.Env[startupPromptDeliveredEnv] == "1"

				if argv == nudge {
					t.Errorf("prompt must ride exactly one of argv or nudge, got argv=%v nudge=%v (PromptSuffix=%q PromptFlag=%q Nudge=%q)",
						argv, nudge, abbreviate(cfg.PromptSuffix), cfg.PromptFlag, abbreviate(cfg.Nudge))
				}
				if !marked {
					t.Errorf("a launch that carries the prompt must set %s=1, got %q", startupPromptDeliveredEnv, cfg.Env[startupPromptDeliveredEnv])
				}
				if !argv && !nudge && !marked {
					t.Errorf("non-empty prompt with no payload and no marker: the SessionStart hook would be the sole carrier")
				}
			})
		}
	}
}

// TestStartupPromptContractWorkerResumeCarriesPromptInNudgeNotArgv pins the
// mechanism for the two worker resume builders: they only run with a resume
// command, so the prompt must never ride in argv behind it.
func TestStartupPromptContractWorkerResumeCarriesPromptInNudgeNotArgv(t *testing.T) {
	city := newContractCity(t, contractPromptBody)
	for _, builder := range launchBuilders {
		if builder.name == "reconciler" {
			continue
		}
		for _, mode := range launchModes {
			t.Run(builder.name+"/"+mode.name, func(t *testing.T) {
				cfg := builder.build(t, city, mode, contractPromptBody)
				if cfg.PromptSuffix != "" || cfg.PromptFlag != "" {
					t.Errorf("worker resume must not carry the prompt in argv, got PromptSuffix=%q PromptFlag=%q", abbreviate(cfg.PromptSuffix), cfg.PromptFlag)
				}
				if !strings.Contains(cfg.Nudge, contractPromptBody) {
					t.Errorf("worker resume must carry the prompt in the nudge, got %q", abbreviate(cfg.Nudge))
				}
			})
		}
	}
}

// TestStartupPromptContractEmptyPromptDeliversNothing pins the unchanged
// behavior for agents with no prompt: no payload, no marker, and the hook stays
// free to carry whatever it carries today.
func TestStartupPromptContractEmptyPromptDeliversNothing(t *testing.T) {
	city := newContractCity(t, "")
	for _, builder := range launchBuilders {
		for _, mode := range launchModes {
			t.Run(builder.name+"/"+mode.name, func(t *testing.T) {
				cfg := builder.build(t, city, mode, "")
				if cfg.PromptSuffix != "" || cfg.PromptFlag != "" || cfg.Nudge != "" {
					t.Errorf("empty prompt must deliver no payload, got PromptSuffix=%q PromptFlag=%q Nudge=%q", cfg.PromptSuffix, cfg.PromptFlag, cfg.Nudge)
				}
				if v, ok := cfg.Env[startupPromptDeliveredEnv]; ok {
					t.Errorf("empty prompt must not set %s, got %q", startupPromptDeliveredEnv, v)
				}
			})
		}
	}
}

// TestStartupPromptContractRunningSessionIsLeftAlone pins that a session whose
// runtime is already up gets no prompt payload: the worker factory resolves
// launch hints on every handle it builds, including read-only ones, and only a
// launch consumes them. Every other session state may launch and gets the prompt.
func TestStartupPromptContractRunningSessionIsLeftAlone(t *testing.T) {
	city := newContractCity(t, contractPromptBody)
	cs := newControllerState(context.Background(), city.cfg, runtime.NewFake(), events.NewFake(), "test-city", city.path)

	for _, tc := range []struct {
		state    session.State
		wantSent bool
	}{
		{state: session.StateActive, wantSent: false},
		{state: session.StateAsleep, wantSent: true},
		{state: session.StateSuspended, wantSent: true},
		{state: session.StateCreating, wantSent: true},
		{state: "", wantSent: true},
	} {
		t.Run("state="+string(tc.state), func(t *testing.T) {
			info := city.sessionInfo(launchModes[1])
			info.State = tc.state
			hints := runtime.Config{WorkDir: city.path, Env: map[string]string{"KEEP": "me"}}

			got, err := cs.ApplyStartupPrompt(info, city.resolved, "", hints)
			if err != nil {
				t.Fatalf("ApplyStartupPrompt: %v", err)
			}
			sent := strings.Contains(got.Nudge, contractPromptBody)
			if sent != tc.wantSent {
				t.Errorf("prompt delivered = %v, want %v (Nudge=%q)", sent, tc.wantSent, abbreviate(got.Nudge))
			}
			if !tc.wantSent && (got.Nudge != "" || got.Env[startupPromptDeliveredEnv] != "") {
				t.Errorf("a running session's hints must be returned unchanged, got Nudge=%q Env=%v", abbreviate(got.Nudge), got.Env)
			}
		})
	}
}

// TestStartupPromptContractWorkerResumeLeavesCallerEnvUntouched pins that the
// resolver's session env map is never mutated: the API resolver hands the same
// map to both the worker's session env and its launch hints.
func TestStartupPromptContractWorkerResumeLeavesCallerEnvUntouched(t *testing.T) {
	city := newContractCity(t, contractPromptBody)
	cs := newControllerState(context.Background(), city.cfg, runtime.NewFake(), events.NewFake(), "test-city", city.path)
	shared := map[string]string{"KEEP": "me"}
	hints := runtime.Config{WorkDir: city.path, Env: shared}

	got, err := cs.ApplyStartupPrompt(city.sessionInfo(launchModes[1]), city.resolved, "", hints)
	if err != nil {
		t.Fatalf("ApplyStartupPrompt: %v", err)
	}
	if got.Env[startupPromptDeliveredEnv] != "1" {
		t.Fatalf("returned hints must carry the marker, got %v", got.Env)
	}
	if _, leaked := shared[startupPromptDeliveredEnv]; leaked {
		t.Errorf("ApplyStartupPrompt mutated the caller's env map: %v", shared)
	}
}

// TestStartupPromptAPIWorkerResumeOfLargePromptSuppressesTheHookPrompt is the
// regression for the a7e50450 shape (ga-4k4zfk): an API session.submit that
// cold-starts a stopped session whose startup prompt is about 90 KB. The launch
// must carry the whole prompt in the nudge and set the marker so that
// `gc prime --hook` blanks its prompt instead of inlining 90 KB into a hook
// payload the provider truncates after about 10,000 characters.
func TestStartupPromptAPIWorkerResumeOfLargePromptSuppressesTheHookPrompt(t *testing.T) {
	body := repeatToBytes("Step: read the bead, claim it, and report back with evidence.\n", 90_000)
	city := newContractCity(t, body)
	cfg := apiWorkerResumeLaunch(t, city, launchModes[1], body)

	if !strings.Contains(cfg.Nudge, strings.TrimSpace(body)) {
		t.Fatalf("nudge carries %d bytes, want the whole ~90 KB prompt", len(cfg.Nudge))
	}
	if cfg.PromptSuffix != "" {
		t.Errorf("a 90 KB prompt must not ride in argv (E2BIG), got a %d byte PromptSuffix", len(cfg.PromptSuffix))
	}

	t.Setenv(startupPromptDeliveredEnv, cfg.Env[startupPromptDeliveredEnv])
	t.Setenv(managedSessionHookEnv, "1")
	if !managedSessionHookPromptAlreadyDelivered(primeHookContext{HookEventName: "SessionStart"}) {
		t.Errorf("the launch env must make the SessionStart hook suppress its prompt (marker=%q)", cfg.Env[startupPromptDeliveredEnv])
	}
}
