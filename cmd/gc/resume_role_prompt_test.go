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

func resolveResumeRoleTemplateWithNudge(t *testing.T, providerName string, providers map[string]config.ProviderSpec, installHooks []string, agentSession string, hooksInstalled *bool, nudge string) TemplateParams {
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

func TestResumeRolePromptSuppliedByHook(t *testing.T) {
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
			if got := resumeRolePromptSuppliedByHook(tp); got != tc.want {
				t.Fatalf("resumeRolePromptSuppliedByHook = %v, want %v (HookEnabled=%v IsACP=%v ancestor=%q)",
					got, tc.want, tp.HookEnabled, tp.IsACP, tp.ResolvedProvider.BuiltinAncestor)
			}
		})
	}

	t.Run("nil resolved provider", func(t *testing.T) {
		if resumeRolePromptSuppliedByHook(TemplateParams{HookEnabled: true}) {
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
// (the opencode hook supplies it to every generation), while a fresh
// incarnation — which is what a plugin-less first turn depends on — still
// carries the full prompt on the launch path (gastownhall/gascity#5238).
func TestResumeOnHookPrimedProviderNeverReplaysRolePrompt(t *testing.T) {
	prevProbe := staleResumeKeyProbe
	staleResumeKeyProbe = func(string, string, string) (present, probeable bool) { return true, true }
	t.Cleanup(func() { staleResumeKeyProbe = prevProbe })

	// No install_agent_hooks: this is the default opencode configuration, and
	// the overlay plugin is staged for it unconditionally.
	tp := resolveResumeRoleTemplate(t, "opencode", builtinProviderAliasesForTest("opencode"), nil, config.SessionTransportTmux, nil)
	if !resumeRolePromptSuppliedByHook(tp) {
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
	payload, err := singleShellArgValue(fresh.cfg.PromptSuffix)
	if err != nil {
		t.Fatalf("fresh PromptSuffix encoding invalid: %v", err)
	}
	if !strings.Contains(payload, "Base worker prompt") {
		t.Fatalf("fresh start payload = %q, want the rendered role prompt on the launch path", payload)
	}
	if !strings.Contains(payload, "User message:\nDo the first task.") {
		t.Fatalf("fresh start payload = %q, want initial_message on first start", payload)
	}
	if fresh.cfg.PromptFlag != "--prompt" {
		t.Fatalf("fresh PromptFlag = %q, want --prompt", fresh.cfg.PromptFlag)
	}
	if fresh.cfg.Nudge != "configured nudge" {
		t.Fatalf("fresh cfg.Nudge = %q, want configured nudge preserved separately", fresh.cfg.Nudge)
	}
	if !fresh.promptDelivered {
		t.Fatal("fresh start promptDelivered = false, want true")
	}
}

// TestResumeOnHookPrimedProviderWithBlankNudgeLandsIdle pins that with no
// configured nudge the hook-primed resume submits NO restart turn. The role
// is already in the system prompt through the plugin, so a content-free wake
// turn only buys a generation that acknowledges and waits; the next human
// message, queued nudge, or reconcile-tick claim backstop starts a real turn.
func TestResumeOnHookPrimedProviderWithBlankNudgeLandsIdle(t *testing.T) {
	prevProbe := staleResumeKeyProbe
	staleResumeKeyProbe = func(string, string, string) (present, probeable bool) { return true, true }
	t.Cleanup(func() { staleResumeKeyProbe = prevProbe })

	tp := resolveResumeRoleTemplateWithNudge(t, "opencode", builtinProviderAliasesForTest("opencode"), nil, config.SessionTransportTmux, nil, "")
	if !resumeRolePromptSuppliedByHook(tp) {
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
	staleResumeKeyProbe = func(string, string, string) (present, probeable bool) { return true, true }
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
			if resumeRolePromptSuppliedByHook(tp) {
				t.Fatal("fixture must not resolve to a hook-primed template")
			}
			prepared := prepareResumeRoleStart(t, tp, "resume-key")
			if want := restartPromptNudge(tp.Prompt, tp.Hints.Nudge); prepared.cfg.Nudge != want {
				t.Fatalf("cfg.Nudge = %q, want restart prompt %q", prepared.cfg.Nudge, want)
			}
		})
	}
}
