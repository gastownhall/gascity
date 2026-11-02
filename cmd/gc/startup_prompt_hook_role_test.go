package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// hookRoleNudge is the configured nudge of the hook-role contract city's worker.
const hookRoleNudge = "HOOK ROLE NUDGE: check your hook."

// hookRoleBeaconTime fixes the reconciler's beacon so its restart turn can be
// compared byte for byte.
var hookRoleBeaconTime = time.Unix(0, 0)

// newHookRoleContractCity is the contract city with its worker on provider,
// declared the way a city declares a builtin (base = "builtin:<provider>"),
// with session as the agent's session override, a configured nudge, and no
// install_agent_hooks: opencode and mimocode stage their per-generation role
// plugin for every launch, so this is their default configuration.
func newHookRoleContractCity(t *testing.T, provider, session string, hooksInstalled *bool) contractCity {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "prompts", "worker.template.md"), []byte(contractPromptBody), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:           "worker",
			Provider:       provider,
			Session:        session,
			PromptTemplate: "prompts/worker.template.md",
			Nudge:          hookRoleNudge,
			HooksInstalled: hooksInstalled,
		}},
		Providers: builtinProviderAliasesForTest(provider),
	}
	resolved, err := config.ResolveProvider(&cfg.Agents[0], &cfg.Workspace, cfg.Providers, func(name string) (string, error) { return name, nil })
	if err != nil {
		t.Fatalf("ResolveProvider(%s): %v", provider, err)
	}
	return contractCity{path: cityPath, cfg: cfg, resolved: resolved}
}

// resolvedReconcilerLaunch is reconcilerLaunch over the template the reconciler
// resolves for the city's worker, so the launch sees the provider, transport
// and hook wiring a real tick does.
func resolvedReconcilerLaunch(t *testing.T, c contractCity, mode launchMode, _ string) runtime.Config {
	t.Helper()
	tp, _ := resolveHookRoleTemplate(t, c)
	return prepareHookRoleLaunch(t, tp, mode)
}

// resolveHookRoleTemplate resolves the city's worker template the way the
// reconciler does and returns it with the build params it used.
func resolveHookRoleTemplate(t *testing.T, c contractCity) (TemplateParams, *agentBuildParams) {
	t.Helper()
	agent := &c.cfg.Agents[0]
	bp := &agentBuildParams{
		fs:         fsys.OSFS{},
		cityName:   c.cfg.Workspace.Name,
		cityPath:   c.path,
		workspace:  &c.cfg.Workspace,
		providers:  c.cfg.Providers,
		lookPath:   func(name string) (string, error) { return filepath.Join("/usr/bin", name), nil },
		beaconTime: hookRoleBeaconTime,
		beadNames:  map[string]string{},
		stderr:     io.Discard,
	}
	tp, err := resolveTemplate(bp, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	return tp, bp
}

// prepareHookRoleLaunch runs the reconciler's start preparation for tp in the
// given launch mode. The transcript probe reports the keyed conversation
// present, so a session with a key resumes it.
func prepareHookRoleLaunch(t *testing.T, tp TemplateParams, mode launchMode) runtime.Config {
	t.Helper()
	prev := staleResumeKeyProbe
	staleResumeKeyProbe = func([]string, string, string, string) (present, probeable bool) { return true, true }
	t.Cleanup(func() { staleResumeKeyProbe = prev })

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
	prepared, _, err := buildPreparedStart(startCandidate{info: sessiontest.SeedBead(t, bead), tp: tp}, &config.City{}, store)
	if err != nil {
		t.Fatalf("buildPreparedStart: %v", err)
	}
	return prepared.cfg
}

// hookRoleResumeBuilders are the three launch builders that resume a session's
// conversation: the reconciler's resume and the CLI and API worker factories.
var hookRoleResumeBuilders = []launchBuilder{
	{name: "reconciler", build: resolvedReconcilerLaunch},
	{name: "CLI worker resume", build: cliWorkerResumeLaunch},
	{name: "API worker resume", build: apiWorkerResumeLaunch},
}

// contractLaunchMode returns the contract launch mode with the given name, so
// a test does not depend on the order of launchModes.
func contractLaunchMode(t *testing.T, name string) launchMode {
	t.Helper()
	for _, mode := range launchModes {
		if mode.name == name {
			return mode
		}
	}
	t.Fatalf("no contract launch mode %q", name)
	return launchMode{}
}

// resumeWithKey is the contract's warm resume: a committed start and a key
// whose conversation is still on disk.
func resumeWithKey(t *testing.T) launchMode {
	t.Helper()
	return contractLaunchMode(t, "resume with key")
}

// TestStartupPromptContractHookSuppliedRoleIsNotReplayedOnResume is the
// regression for gastownhall/gascity#6627. opencode and mimocode on the CLI
// transport stage a plugin that prepends `gc prime --hook` to the system prompt
// of every generation, so a resumed conversation already has its role. No
// resume builder may submit the role again as a user turn: each copy persists
// in the transcript and grows it on every wake. The delivered marker stays set,
// since the session is still primed, through the hook instead of the nudge.
// The reconciler's restart turn is the beacon plus the configured nudge; the
// worker factories carry no configured nudge, so they submit no restart turn.
func TestStartupPromptContractHookSuppliedRoleIsNotReplayedOnResume(t *testing.T) {
	wantRestartTurn := runtime.FormatBeaconAt("test-city", "worker", false, hookRoleBeaconTime) + startupPromptNudgeSeparator + hookRoleNudge
	for _, tc := range []struct {
		provider string
		session  string
	}{
		{provider: "opencode", session: config.SessionTransportTmux},
		// mimocode resolves the CLI transport by default.
		{provider: "mimocode"},
	} {
		city := newHookRoleContractCity(t, tc.provider, tc.session, nil)
		for _, builder := range hookRoleResumeBuilders {
			t.Run(tc.provider+"/"+builder.name, func(t *testing.T) {
				cfg := builder.build(t, city, resumeWithKey(t), contractPromptBody)
				if strings.Contains(cfg.Nudge, contractPromptBody) || carriedThroughArgv(cfg, contractPromptBody) {
					t.Errorf("resume replayed the role the hook already supplies: Nudge=%q PromptSuffix=%q", abbreviate(cfg.Nudge), abbreviate(cfg.PromptSuffix))
				}
				if cfg.PromptSuffix != "" || cfg.PromptFlag != "" {
					t.Errorf("a resume must not carry a launch prompt, got PromptSuffix=%q PromptFlag=%q", abbreviate(cfg.PromptSuffix), cfg.PromptFlag)
				}
				if cfg.Env[startupPromptDeliveredEnv] != "1" {
					t.Errorf("%s = %q, want 1: the hook still primes the session", startupPromptDeliveredEnv, cfg.Env[startupPromptDeliveredEnv])
				}
				want := ""
				if builder.name == "reconciler" {
					want = wantRestartTurn
				}
				if cfg.Nudge != want {
					t.Errorf("restart turn = %q, want %q", abbreviate(cfg.Nudge), want)
				}
			})
		}
	}
}

// TestStartupPromptContractRoleStillReplayedOnResumeWithoutPerTurnHook guards
// the other side: when no plugin carries the role into every generation, every
// resume builder keeps replaying it in the nudge. That covers an operator's
// hooks_installed = false and a provider whose hook primes only at
// SessionStart. TestStartupPromptContractACPResumeStillReplaysRole covers ACP.
func TestStartupPromptContractRoleStillReplayedOnResumeWithoutPerTurnHook(t *testing.T) {
	no := false
	for _, tc := range []struct {
		name           string
		provider       string
		session        string
		hooksInstalled *bool
	}{
		{name: "opencode hooks_installed=false", provider: "opencode", session: config.SessionTransportTmux, hooksInstalled: &no},
		{name: "pi", provider: "pi"},
	} {
		city := newHookRoleContractCity(t, tc.provider, tc.session, tc.hooksInstalled)
		for _, builder := range hookRoleResumeBuilders {
			t.Run(tc.name+"/"+builder.name, func(t *testing.T) {
				cfg := builder.build(t, city, resumeWithKey(t), contractPromptBody)
				if !strings.Contains(cfg.Nudge, contractPromptBody) {
					t.Errorf("resume without a per-turn role hook must replay the role in the nudge, got %q", abbreviate(cfg.Nudge))
				}
				if cfg.Env[startupPromptDeliveredEnv] != "1" {
					t.Errorf("%s = %q, want 1", startupPromptDeliveredEnv, cfg.Env[startupPromptDeliveredEnv])
				}
			})
		}
	}
}

// TestStartupPromptContractACPResumeStillReplaysRole pins that an ACP resume
// of a hook-primed provider keeps the role in the nudge on every resume path:
// ACP never loads the CLI plugin. opencode resolves ACP by default; each path
// sees the transport the way production hands it over (the reconciler from
// the resolved template, the CLI factory from the session's recorded
// transport, the API factory from the transport its caller resolved).
func TestStartupPromptContractACPResumeStillReplaysRole(t *testing.T) {
	city := newHookRoleContractCity(t, "opencode", "", nil)
	info := city.sessionInfo(resumeWithKey(t))
	info.Transport = config.SessionTransportACP
	for _, path := range []struct {
		name  string
		build func(t *testing.T) runtime.Config
	}{
		{name: "reconciler", build: func(t *testing.T) runtime.Config {
			return resolvedReconcilerLaunch(t, city, resumeWithKey(t), contractPromptBody)
		}},
		{name: "CLI worker resume", build: func(t *testing.T) runtime.Config {
			resolved, err := resolvedWorkerRuntimeWithConfig(city.path, city.cfg, info, "")
			if err != nil || resolved == nil {
				t.Fatalf("resolvedWorkerRuntimeWithConfig = %v, %v", resolved, err)
			}
			return resolved.Hints
		}},
		{name: "API worker resume", build: func(t *testing.T) runtime.Config {
			got, err := city.controllerState().ApplyStartupPrompt(info, city.resolved, config.SessionTransportACP, runtime.Config{WorkDir: city.path})
			if err != nil {
				t.Fatalf("ApplyStartupPrompt: %v", err)
			}
			return got
		}},
	} {
		t.Run(path.name, func(t *testing.T) {
			cfg := path.build(t)
			if !strings.Contains(cfg.Nudge, contractPromptBody) {
				t.Errorf("an ACP resume must replay the role in the nudge, got %q", abbreviate(cfg.Nudge))
			}
			if cfg.Env[startupPromptDeliveredEnv] != "1" {
				t.Errorf("%s = %q, want 1", startupPromptDeliveredEnv, cfg.Env[startupPromptDeliveredEnv])
			}
		})
	}
}

// TestStartupPromptContractHookSuppliedRoleFreshStartKeepsLaunchPrompt pins
// that the resume fix leaves fresh launches alone: a first start or a forced
// fresh start of a hook-primed session still carries the role on the launch
// path and marks it delivered.
func TestStartupPromptContractHookSuppliedRoleFreshStartKeepsLaunchPrompt(t *testing.T) {
	city := newHookRoleContractCity(t, "opencode", config.SessionTransportTmux, nil)
	for _, mode := range []launchMode{contractLaunchMode(t, "first start"), contractLaunchMode(t, "forceFresh")} {
		t.Run(mode.name, func(t *testing.T) {
			cfg := resolvedReconcilerLaunch(t, city, mode, contractPromptBody)
			if !carriedThroughArgv(cfg, contractPromptBody) || cfg.PromptFlag != "--prompt" {
				t.Errorf("fresh start must carry the role through the --prompt launch flag, got PromptSuffix=%q PromptFlag=%q", abbreviate(cfg.PromptSuffix), cfg.PromptFlag)
			}
			if cfg.Nudge != hookRoleNudge {
				t.Errorf("fresh start nudge = %q, want the configured nudge %q", abbreviate(cfg.Nudge), hookRoleNudge)
			}
			if cfg.Env[startupPromptDeliveredEnv] != "1" {
				t.Errorf("%s = %q, want 1", startupPromptDeliveredEnv, cfg.Env[startupPromptDeliveredEnv])
			}
		})
	}
}

// TestStartupPromptContractClientProviderChatResumeKeepsBeacon guards a
// client's provider chat (#7721) on a hook-primed provider. Its hook writes no
// persona, so no hook supplies a role and the chat keeps the beacon-only turns
// upstream gives it: the reconciler resumes it with the beacon, and the worker
// factories leave its hints alone.
func TestStartupPromptContractClientProviderChatResumeKeepsBeacon(t *testing.T) {
	city := newHookRoleContractCity(t, "opencode", config.SessionTransportTmux, nil)
	agent := &city.cfg.Agents[0]
	agent.Implicit = true
	agent.Nudge = ""

	t.Run("reconciler", func(t *testing.T) {
		tp, bp := resolveHookRoleTemplate(t, city)
		tp.ManualSession = true
		applyClientProviderChatStartupPrompt(bp, agent, agent, agent.QualifiedName(), "my-chat", &tp)
		beacon := runtime.FormatBeaconAt("test-city", "worker", false, hookRoleBeaconTime)
		if tp.Prompt != beacon {
			t.Fatalf("fixture must resolve to a client chat with the beacon alone, got Prompt=%q", abbreviate(tp.Prompt))
		}
		cfg := prepareHookRoleLaunch(t, tp, resumeWithKey(t))
		if cfg.Nudge != beacon || cfg.Env[startupPromptDeliveredEnv] != "1" {
			t.Errorf("client chat resume = (Nudge=%q marker=%q), want the beacon and the marker", abbreviate(cfg.Nudge), cfg.Env[startupPromptDeliveredEnv])
		}
	})
	info := city.sessionInfo(resumeWithKey(t))
	info.SessionOrigin = "manual"
	info.Alias = "my-chat"
	for _, path := range []struct {
		name  string
		build func(t *testing.T) runtime.Config
	}{
		{name: "CLI worker resume", build: func(t *testing.T) runtime.Config {
			resolved, err := resolvedWorkerRuntimeWithConfig(city.path, city.cfg, info, "")
			if err != nil || resolved == nil {
				t.Fatalf("resolvedWorkerRuntimeWithConfig = %v, %v", resolved, err)
			}
			return resolved.Hints
		}},
		{name: "API worker resume", build: func(t *testing.T) runtime.Config {
			got, err := city.controllerState().ApplyStartupPrompt(info, city.resolved, "", runtime.Config{WorkDir: city.path})
			if err != nil {
				t.Fatalf("ApplyStartupPrompt: %v", err)
			}
			return got
		}},
	} {
		t.Run(path.name, func(t *testing.T) {
			cfg := path.build(t)
			if cfg.Nudge != "" || cfg.Env[startupPromptDeliveredEnv] != "" {
				t.Errorf("client chat hints must be left alone, got Nudge=%q Env=%v", abbreviate(cfg.Nudge), cfg.Env)
			}
		})
	}
}
