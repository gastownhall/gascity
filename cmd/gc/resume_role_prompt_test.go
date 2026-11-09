package main

import (
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// resolveResumeRoleTemplate renders a template through the production
// resolver so the predicate under test sees the same HookEnabled / IsACP /
// ResolvedProvider wiring a real launch does.
func resolveResumeRoleTemplate(t *testing.T, providerName string, providers map[string]config.ProviderSpec, installHooks []string, agentSession string, hooksInstalled *bool) TemplateParams {
	t.Helper()
	return resolveResumeRoleTemplateWithNudge(t, providerName, providers, installHooks, agentSession, hooksInstalled, "configured nudge")
}

func resolveResumeRoleTemplateWithNudge(t *testing.T, providerName string, providers map[string]config.ProviderSpec, installHooks []string, agentSession string, hooksInstalled *bool, nudge string, cityRuntime ...string) TemplateParams {
	t.Helper()

	cityPath := t.TempDir()
	fs := fsys.NewFake()
	fs.Files[cityPath+"/prompts/worker.md"] = []byte("Base worker prompt")
	params := &agentBuildParams{
		fs:         fs,
		cityName:   "resume-role-city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: providerName, InstallAgentHooks: installHooks},
		providers:  providers,
		lookPath:   func(name string) (string, error) { return filepath.Join("/usr/bin", name), nil },
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
	if len(cityRuntime) > 0 {
		params.sessionProvider = cityRuntime[0]
	}
	agentCfg := &config.Agent{
		Name:           "worker",
		Provider:       providerName,
		PromptTemplate: "prompts/worker.md",
		Session:        agentSession,
		Nudge:          nudge,
		HooksInstalled: hooksInstalled,
		WorkDir:        filepath.Join(".gc", "agents", "worker"),
	}
	tp, err := resolveTemplate(params, agentCfg, agentCfg.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate(%s): %v", providerName, err)
	}
	return tp
}

func TestStartupPromptSourceRoleSuppliedByHook(t *testing.T) {
	no := false
	wrappedBase := "builtin:opencode"
	wrapped := map[string]config.ProviderSpec{"wrapped-opencode": {Base: &wrappedBase}}

	for _, tc := range []struct {
		name           string
		provider       string
		providers      map[string]config.ProviderSpec
		installHooks   []string
		agentSession   string
		hooksInstalled *bool
		want           bool
	}{
		{
			name:         "opencode with hooks installed",
			provider:     "opencode",
			providers:    builtinProviderAliasesForTest("opencode"),
			installHooks: []string{"opencode"},
			agentSession: config.SessionTransportTmux,
			want:         true,
		},
		{
			name:         "mimocode with hooks installed",
			provider:     "mimocode",
			providers:    builtinProviderAliasesForTest("mimocode"),
			installHooks: []string{"mimocode"},
			agentSession: config.SessionTransportTmux,
			want:         true,
		},
		{
			// The opencode overlay plugin is staged for every launch, so the
			// agent is hook-enabled without install_agent_hooks; the default
			// configuration takes the hook-primed branch.
			name:         "opencode default configuration (no install_agent_hooks)",
			provider:     "opencode",
			providers:    builtinProviderAliasesForTest("opencode"),
			agentSession: config.SessionTransportTmux,
			want:         true,
		},
		{
			name:           "opencode with hooks_installed = false",
			provider:       "opencode",
			providers:      builtinProviderAliasesForTest("opencode"),
			installHooks:   []string{"opencode"},
			agentSession:   config.SessionTransportTmux,
			hooksInstalled: &no,
			want:           false,
		},
		{
			name:         "pi hook delivers the role once at SessionStart",
			provider:     "pi",
			providers:    builtinProviderAliasesForTest("pi"),
			installHooks: []string{"pi"},
			agentSession: config.SessionTransportTmux,
			want:         false,
		},
		{
			name:         "claude settings hook delivers the role once at SessionStart",
			provider:     "claude",
			providers:    builtinProviderAliasesForTest("claude"),
			installHooks: []string{"claude"},
			agentSession: config.SessionTransportTmux,
			want:         false,
		},
		{
			// opencode's default transport is ACP (no session override), and
			// ACP never loads the CLI plugin: the nudge stays the role carrier.
			name:         "opencode default ACP transport keeps the resume prompt in the nudge",
			provider:     "opencode",
			providers:    builtinProviderAliasesForTest("opencode"),
			installHooks: []string{"opencode"},
			want:         false,
		},
		{
			name:         "opencode explicit ACP transport keeps the resume prompt in the nudge",
			provider:     "opencode",
			providers:    builtinProviderAliasesForTest("opencode"),
			installHooks: []string{"opencode"},
			agentSession: config.SessionTransportACP,
			want:         false,
		},
		{
			name:         "wrapped custom provider based on builtin opencode",
			provider:     "wrapped-opencode",
			providers:    wrapped,
			installHooks: []string{"wrapped-opencode"},
			agentSession: config.SessionTransportTmux,
			want:         true,
		},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tp := resolveResumeRoleTemplate(t, tc.provider, tc.providers, tc.installHooks, tc.agentSession, tc.hooksInstalled)
			if got := tp.startupPromptSource().roleSuppliedByHook(); got != tc.want {
				t.Fatalf("roleSuppliedByHook = %v, want %v (HookEnabled=%v IsACP=%v ancestor=%q)",
					got, tc.want, tp.HookEnabled, tp.IsACP, tp.ResolvedProvider.BuiltinAncestor)
			}
		})
	}

	t.Run("nil resolved provider", func(t *testing.T) {
		if (TemplateParams{HookEnabled: true}).startupPromptSource().roleSuppliedByHook() {
			t.Fatal("a template with no resolved provider must not claim a per-turn role hook")
		}
	})
}

// prepareResumeRoleStart runs the production start preparation for a session
// bead. A non-empty resumeKey models a warm resume (started hash recorded and
// a provider conversation to resume); an empty one models a fresh launch.
func prepareResumeRoleStart(t *testing.T, tp TemplateParams, resumeKey string) *preparedStart {
	t.Helper()

	overrides, err := json.Marshal(map[string]string{"initial_message": "Do the first task."})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	metadata := map[string]string{
		"session_name":       "resume-role-worker",
		"template":           "worker",
		"template_overrides": string(overrides),
	}
	if resumeKey != "" {
		metadata["started_config_hash"] = "already-started"
		metadata["session_key"] = resumeKey
	}
	return prepareRoleStart(t, tp, metadata)
}

func prepareRoleStart(t *testing.T, tp TemplateParams, metadata map[string]string) *preparedStart {
	t.Helper()
	store := beads.NewMemStore()
	session, err := store.Create(beads.Bead{
		Title:    "resume-role-worker",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel},
		Metadata: metadata,
	})
	if err != nil {
		t.Fatalf("Create session bead: %v", err)
	}
	prepared, err := prepareStartCandidate(startCandidate{
		info: sessiontest.SeedBead(t, session),
		tp:   tp,
	}, &config.City{}, store, &clock.Fake{Time: time.Date(2026, 4, 5, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("prepareStartCandidate: %v", err)
	}
	return prepared
}

// TestResumeOnHookPrimedProviderNeverReplaysRolePrompt pins the fix for the
// duplicated role on hook-primed providers: across repeated resumes of the
// same provider conversation the rendered template never rides in the nudge
// (the opencode hook supplies it to every generation). Fresh starts deliver
// their initial message without persisting a second copy of the role either.
func TestResumeOnHookPrimedProviderNeverReplaysRolePrompt(t *testing.T) {
	prevProbe := staleResumeKeyProbe
	staleResumeKeyProbe = func([]string, string, string, string) (present, probeable bool) { return true, true }
	t.Cleanup(func() { staleResumeKeyProbe = prevProbe })

	// No install_agent_hooks: this is the default opencode configuration, and
	// the overlay plugin is staged for it unconditionally.
	tp := resolveResumeRoleTemplate(t, "opencode", builtinProviderAliasesForTest("opencode"), nil, config.SessionTransportTmux, nil)
	if !tp.startupPromptSource().roleSuppliedByHook() {
		t.Fatal("fixture must resolve to a hook-primed opencode template")
	}
	if !strings.Contains(tp.Prompt, "Base worker prompt") {
		t.Fatalf("Prompt = %q, want rendered template", tp.Prompt)
	}

	wantBeacon := runtime.FormatBeaconAt("resume-role-city", "worker", false, time.Unix(0, 0))
	wantRestartTurn := wantBeacon + startupPromptNudgeSeparator + "configured nudge"
	for i := 0; i < 3; i++ {
		prepared := prepareResumeRoleStart(t, tp, "resume-key")
		if strings.Contains(prepared.cfg.Nudge, "Base worker prompt") {
			t.Fatalf("resume %d: cfg.Nudge = %q, want no replayed role prompt", i+1, prepared.cfg.Nudge)
		}
		// The restart turn is still needed: the provider hook supplies the
		// role TO a generation but never starts one, so the resumed session
		// must receive a real user turn. It is the beacon plus the configured
		// nudge, never the template.
		if prepared.cfg.Nudge != wantRestartTurn {
			t.Fatalf("resume %d: cfg.Nudge = %q, want beacon-prefixed configured nudge %q", i+1, prepared.cfg.Nudge, wantRestartTurn)
		}
		if prepared.cfg.PromptSuffix != "" || prepared.cfg.PromptFlag != "" {
			t.Fatalf("resume %d: launch prompt = (%q, %q), want none on resume", i+1, prepared.cfg.PromptSuffix, prepared.cfg.PromptFlag)
		}
		if strings.Contains(prepared.cfg.Nudge, "Do the first task.") {
			t.Fatalf("resume %d: cfg.Nudge = %q, want no replayed initial_message", i+1, prepared.cfg.Nudge)
		}
		// The env marker is what the provider hook keys on and what observers
		// use to tell "primed" from "live but never primed"; the role still
		// reaches the model through the hook, so the marker stays set.
		if prepared.cfg.Env[startupPromptDeliveredEnv] != "1" {
			t.Fatalf("resume %d: %s = %q, want 1", i+1, startupPromptDeliveredEnv, prepared.cfg.Env[startupPromptDeliveredEnv])
		}
		if prepared.promptDelivered {
			t.Fatalf("resume %d: promptDelivered = true, want false on a resume incarnation", i+1)
		}
		if got, want := prepared.promptHash, sessionpkg.PromptHash(tp.Prompt); got != want {
			t.Fatalf("resume %d: promptHash = %q, want %q", i+1, got, want)
		}
	}

	fresh := prepareResumeRoleStart(t, tp, "")
	if fresh.cfg.PromptSuffix != "" || fresh.cfg.PromptFlag != "" {
		t.Fatalf("fresh start submitted a role via argv: %+v", fresh.cfg)
	}
	if want := wantRestartTurn + startupPromptNudgeSeparator + "User message:\nDo the first task."; fresh.cfg.Nudge != want {
		t.Fatalf("fresh cfg.Nudge = %q, want %q", fresh.cfg.Nudge, want)
	}
	if !fresh.promptDelivered {
		t.Fatal("fresh start promptDelivered = false, want true")
	}
}

// A fresh managed conversation receives its role through the plugin and only
// meaningful activation text through the runtime. Exercise real resolution
// and start preparation, including wrappers and both per-generation families.
func TestFreshHookPrimedStartActivation(t *testing.T) {
	wrappedBase := "builtin:opencode"
	for _, provider := range []string{"opencode", "mimocode", "wrapped-opencode"} {
		for _, tc := range []struct {
			name, nudge, message string
		}{
			{name: "idle"},
			{name: "whitespace nudge", nudge: " \n\t"},
			{name: "nudge", nudge: "Claim routed work."},
			{name: "message", message: "Discuss the extraction initiative."},
			{name: "both", nudge: "Claim routed work.", message: "Discuss the extraction initiative."},
		} {
			t.Run(provider+"/"+tc.name, func(t *testing.T) {
				providers := builtinProviderAliasesForTest(provider)
				if provider == "wrapped-opencode" {
					providers = map[string]config.ProviderSpec{provider: {Base: &wrappedBase}}
				}
				tp := resolveResumeRoleTemplateWithNudge(t, provider, providers, nil, config.SessionTransportTmux, nil, tc.nudge)
				overrides, err := json.Marshal(map[string]string{"initial_message": tc.message})
				if err != nil {
					t.Fatal(err)
				}
				prepared := prepareRoleStart(t, tp, map[string]string{
					"session_name": "fresh-worker", "template": "worker", "session_origin": "manual", "template_overrides": string(overrides),
				})
				want := tp.Beacon + startupPromptNudgeSeparator + "Confirm your role: reply with your role name and a one-sentence purpose, then wait for instructions."
				if strings.TrimSpace(tc.nudge) != "" {
					want = tp.Beacon + startupPromptNudgeSeparator + tc.nudge
				}
				if tc.message != "" {
					if strings.TrimSpace(tc.nudge) != "" {
						want += startupPromptNudgeSeparator
					} else {
						want = ""
					}
					want += "User message:\n" + tc.message
				}
				if prepared.cfg.PromptSuffix != "" || prepared.cfg.PromptFlag != "" || prepared.cfg.Nudge != want {
					t.Fatalf("activation = (suffix=%q flag=%q nudge=%q), want nudge=%q only", prepared.cfg.PromptSuffix, prepared.cfg.PromptFlag, prepared.cfg.Nudge, want)
				}
				if !prepared.promptDelivered || prepared.cfg.Env[startupPromptDeliveredEnv] != "1" {
					t.Fatal("hook-selected role delivery lost its priming metadata")
				}
				if prepared.promptHash != sessionpkg.PromptHash(tp.Prompt) {
					t.Fatal("role hash must still describe the rendered template")
				}
			})
		}
	}
}

func TestFreshHookPrimedStartRecovery(t *testing.T) {
	prevProbe := staleResumeKeyProbe
	staleResumeKeyProbe = func([]string, string, string, string) (bool, bool) { return false, true }
	t.Cleanup(func() { staleResumeKeyProbe = prevProbe })
	for _, mode := range []string{"missing-key", "stale-key", "fresh"} {
		t.Run(mode, func(t *testing.T) {
			tp := resolveResumeRoleTemplate(t, "opencode", builtinProviderAliasesForTest("opencode"), nil, config.SessionTransportTmux, nil)
			metadata := map[string]string{"session_name": "fresh-worker", "template": "worker", "started_config_hash": "old", "primed_at": "2026-01-01T00:00:00Z"}
			if mode == "stale-key" {
				metadata["session_key"] = "gone"
			}
			if mode == "fresh" {
				metadata["wake_mode"] = "fresh"
			}
			prepared := prepareRoleStart(t, tp, metadata)
			if prepared.cfg.PromptSuffix != "" || prepared.cfg.PromptFlag != "" || prepared.cfg.Nudge != tp.Beacon+startupPromptNudgeSeparator+"configured nudge" {
				t.Fatalf("recovered fresh start must activate without role replay: suffix=%q flag=%q nudge=%q", prepared.cfg.PromptSuffix, prepared.cfg.PromptFlag, prepared.cfg.Nudge)
			}
			if !prepared.promptDelivered {
				t.Fatal("fresh incarnation must select hook priming despite old metadata")
			}
		})
	}
}

func TestFreshRoleLaunchFallbacks(t *testing.T) {
	no := false
	standalone := ""
	for _, tc := range []struct {
		name, provider, transport string
		providers                 map[string]config.ProviderSpec
		hooksInstalled            *bool
	}{
		{name: "hook opt-out", provider: "opencode", transport: config.SessionTransportTmux, hooksInstalled: &no},
		{name: "ACP", provider: "opencode", transport: config.SessionTransportACP},
		{name: "SessionStart hook", provider: "pi", transport: config.SessionTransportTmux},
		{name: "standalone name collision", provider: "opencode", transport: config.SessionTransportTmux, providers: map[string]config.ProviderSpec{"opencode": {Base: &standalone, Command: "opencode"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			providers := tc.providers
			if providers == nil {
				providers = builtinProviderAliasesForTest(tc.provider)
			}
			tp := resolveResumeRoleTemplate(t, tc.provider, providers, nil, tc.transport, tc.hooksInstalled)
			prepared := prepareResumeRoleStart(t, tp, "")
			if !strings.Contains(prepared.cfg.PromptSuffix+prepared.cfg.Nudge, "Base worker prompt") {
				t.Fatal("plugin-less path lost its role carrier")
			}
		})
	}
}

func TestHookPrimedRoleDoesNotUseArgvBudget(t *testing.T) {
	// A herdr city: herdr is a local runtime, so the hook carries the role.
	tp := resolveResumeRoleTemplateWithNudge(t, "opencode", builtinProviderAliasesForTest("opencode"), nil, config.SessionTransportTmux, nil, "configured nudge", "herdr")
	if !tp.startupPromptSource().roleSuppliedByHook() {
		t.Fatal("fixture must resolve to a hook-primed template in a herdr city")
	}
	tp.Prompt = strings.Repeat("role ", maxPromptSuffixRawBytes)
	// A hook-supplied role never enters argv or the oversized nudge fallback.
	_, delivery, err := templateParamsToConfigWithDelivery(tp)
	if err != nil || delivery.OversizedFallback {
		t.Fatalf("hook role used argv fallback: %+v, %v", delivery, err)
	}
	prepared := prepareResumeRoleStart(t, tp, "")
	if prepared.cfg.PromptSuffix != "" || strings.Contains(prepared.cfg.Nudge, "role ") {
		t.Fatal("oversized role escaped the hook carrier")
	}
}

func TestFreshHookPrimedAutomaticKickoff(t *testing.T) {
	for _, origin := range []string{"named", "ephemeral", "pool"} {
		t.Run(origin, func(t *testing.T) {
			tp := resolveResumeRoleTemplateWithNudge(t, "opencode", builtinProviderAliasesForTest("opencode"), nil, config.SessionTransportTmux, nil, "")
			metadata := map[string]string{"session_name": "worker", "template": "worker", "session_origin": origin}
			want := "Begin your startup routine as your instructions describe. If it finds no work for you, end your turn without a status report."
			if origin == "pool" {
				metadata["session_origin"] = "ephemeral"
				metadata["pool_managed"] = "true"
				want = "Run gc hook --claim --drain-ack --json now; if it returns work, execute it immediately."
			}
			prepared := prepareRoleStart(t, tp, metadata)
			if prepared.cfg.Nudge != tp.Beacon+startupPromptNudgeSeparator+want || prepared.cfg.PromptSuffix != "" {
				t.Fatalf("automated fresh start lost its kickoff: suffix=%q nudge=%q", prepared.cfg.PromptSuffix, prepared.cfg.Nudge)
			}
			metadata["template_overrides"] = `{"initial_message":"Do my actual task."}`
			prepared = prepareRoleStart(t, tp, metadata)
			if prepared.cfg.Nudge != "User message:\nDo my actual task." {
				t.Fatalf("kickoff displaced initial message: %q", prepared.cfg.Nudge)
			}
		})
	}
}

func TestFreshHookPrimedForceFreshWithValidKey(t *testing.T) {
	prev := staleResumeKeyProbe
	staleResumeKeyProbe = func([]string, string, string, string) (bool, bool) { return true, true }
	t.Cleanup(func() { staleResumeKeyProbe = prev })
	tp := resolveResumeRoleTemplateWithNudge(t, "opencode", builtinProviderAliasesForTest("opencode"), nil, config.SessionTransportTmux, nil, "")
	prepared := prepareRoleStart(t, tp, map[string]string{
		"session_name": "worker", "template": "worker", "session_origin": "manual", "started_config_hash": "old",
		"session_key": "valid", "wake_mode": "fresh", "template_overrides": `{"initial_message":"Start again."}`,
	})
	if prepared.cfg.Nudge != "User message:\nStart again." || prepared.cfg.PromptSuffix != "" || !prepared.promptDelivered {
		t.Fatalf("forced fresh start treated as resume: nudge=%q suffix=%q primed=%v", prepared.cfg.Nudge, prepared.cfg.PromptSuffix, prepared.promptDelivered)
	}
}

func TestHookRoleRequiresLocalStagingRuntime(t *testing.T) {
	for _, name := range []string{"", "tmux", "herdr", "ssh:remote", "exec:/worker", "k8s", "hybrid", "subprocess", "custom-runtime"} {
		t.Run(name, func(t *testing.T) {
			// session=tmux selects the terminal transport, not the city runtime.
			tp := resolveResumeRoleTemplateWithNudge(t, "opencode", builtinProviderAliasesForTest("opencode"), nil, config.SessionTransportTmux, nil, "configured nudge", name)
			cfg, _, err := templateParamsToConfigWithDelivery(tp)
			if err != nil {
				t.Fatal(err)
			}
			wantHook := name == "" || name == "tmux" || name == "herdr"
			if (cfg.PromptSuffix == "") != wantHook {
				t.Fatalf("runtime=%q suffix=%q, hook eligible=%v", name, cfg.PromptSuffix, wantHook)
			}
		})
	}
}

// TestResumeOnHookPrimedProviderWithBlankNudgeLandsIdle pins that with no
// configured nudge the hook-primed resume submits NO restart turn. The role
// is already in the system prompt through the plugin, so a content-free wake
// turn only buys a generation that acknowledges and waits; the next human
// message, queued nudge, or reconcile-tick claim backstop starts a real turn.
func TestResumeOnHookPrimedProviderWithBlankNudgeLandsIdle(t *testing.T) {
	prevProbe := staleResumeKeyProbe
	staleResumeKeyProbe = func([]string, string, string, string) (present, probeable bool) { return true, true }
	t.Cleanup(func() { staleResumeKeyProbe = prevProbe })

	tp := resolveResumeRoleTemplateWithNudge(t, "opencode", builtinProviderAliasesForTest("opencode"), nil, config.SessionTransportTmux, nil, "")
	if !tp.startupPromptSource().roleSuppliedByHook() {
		t.Fatal("fixture must resolve to a hook-primed opencode template")
	}
	if tp.Hints.Nudge != "" {
		t.Fatalf("Hints.Nudge = %q, want blank fixture", tp.Hints.Nudge)
	}

	prepared := prepareResumeRoleStart(t, tp, "resume-key")
	if prepared.cfg.Nudge != "" {
		t.Fatalf("cfg.Nudge = %q, want no restart turn when no nudge is configured", prepared.cfg.Nudge)
	}
	if prepared.cfg.PromptSuffix != "" || prepared.cfg.PromptFlag != "" {
		t.Fatalf("launch prompt = (%q, %q), want none on resume", prepared.cfg.PromptSuffix, prepared.cfg.PromptFlag)
	}
	// The hook still keys on the delivered marker; the role reaches the model
	// through the plugin, so the session is primed even though nothing is sent.
	if prepared.cfg.Env[startupPromptDeliveredEnv] != "1" {
		t.Fatalf("%s = %q, want 1", startupPromptDeliveredEnv, prepared.cfg.Env[startupPromptDeliveredEnv])
	}
}

// TestResumeWithoutPerTurnRoleHookStillReplaysRolePrompt guards the other
// side of the split: a hook-enabled provider whose hook only primes at
// SessionStart (pi) keeps the restart prompt in its resume nudge.
func TestResumeWithoutPerTurnRoleHookStillReplaysRolePrompt(t *testing.T) {
	prevProbe := staleResumeKeyProbe
	staleResumeKeyProbe = func([]string, string, string, string) (present, probeable bool) { return true, true }
	t.Cleanup(func() { staleResumeKeyProbe = prevProbe })

	no := false
	for _, tc := range []struct {
		name           string
		provider       string
		installHooks   []string
		hooksInstalled *bool
	}{
		{name: "pi with hooks", provider: "pi", installHooks: []string{"pi"}},
		{name: "opencode with hooks_installed = false", provider: "opencode", hooksInstalled: &no},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tp := resolveResumeRoleTemplate(t, tc.provider, builtinProviderAliasesForTest(tc.provider), tc.installHooks, config.SessionTransportTmux, tc.hooksInstalled)
			if tp.startupPromptSource().roleSuppliedByHook() {
				t.Fatal("fixture must not resolve to a hook-primed template")
			}
			prepared := prepareResumeRoleStart(t, tp, "resume-key")
			if want := restartPromptNudge(tp.Prompt, tp.Hints.Nudge); prepared.cfg.Nudge != want {
				t.Fatalf("cfg.Nudge = %q, want restart prompt %q", prepared.cfg.Nudge, want)
			}
		})
	}
}

// TestHookRoleKeepsSessionOverrideRouting pins that the hook-role runtime check
// reads the city's runtime without changing what an agent's session override
// resolves to. In a t3bridge city, session = "tmux" still resolves the tmux
// session provider, so templateParamsUseT3Bridge stays false as it was, and
// the role stays on the launch path because the bridge is not a local runtime.
// In a herdr city the same agent takes the hook, since herdr is local.
func TestHookRoleKeepsSessionOverrideRouting(t *testing.T) {
	tp := resolveResumeRoleTemplateWithNudge(t, "opencode", builtinProviderAliasesForTest("opencode"), nil, config.SessionTransportTmux, nil, "configured nudge", "t3bridge")
	if tp.EffectiveSessionProvider != config.SessionTransportTmux || templateParamsUseT3Bridge(tp) {
		t.Fatalf("session = tmux in a t3bridge city: EffectiveSessionProvider = %q, t3bridge = %v; want tmux and false",
			tp.EffectiveSessionProvider, templateParamsUseT3Bridge(tp))
	}
	if tp.startupPromptSource().roleSuppliedByHook() {
		t.Fatal("a t3bridge city must keep the role on the launch path")
	}
	herdr := resolveResumeRoleTemplateWithNudge(t, "opencode", builtinProviderAliasesForTest("opencode"), nil, config.SessionTransportTmux, nil, "configured nudge", "herdr")
	if herdr.EffectiveSessionProvider != config.SessionTransportTmux {
		t.Fatalf("session = tmux in a herdr city: EffectiveSessionProvider = %q, want tmux", herdr.EffectiveSessionProvider)
	}
	if !herdr.startupPromptSource().roleSuppliedByHook() {
		t.Fatal("a herdr city stages the plugin locally, so the hook must carry the role")
	}
}
