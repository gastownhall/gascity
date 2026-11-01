package main

import (
	"fmt"
	"io"
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/hooks"
)

func TestIsOperationalScript(t *testing.T) {
	cases := []struct {
		rel  string
		want bool
	}{
		{"city-start.sh", true},
		{"city-stop.sh", true},
		{"update-gascity.sh", true},
		{"update-external-tools.sh", true},
		// Non-operational / agent-relevant scripts stay content-hashed.
		{"gc-human-notify.sh", false},
		{"hq-noms-recovery.sh", false},
		{"embedder-eval.py", false},
		{"gc-beads-bd.sh", false},
		// Prefix must be followed by a hyphen + a name + .sh; bare stems and
		// non-.sh extensions are not operational.
		{"city.sh", false},
		{"update.sh", false},
		{"city-start.py", false},
		{"update-gascity.txt", false},
		{"a.sh", false},
		// Match is on the basename, not a substring of the path.
		{"sub/update-gascity.sh", true},
		{"my-update-tool.sh", false},
	}
	for _, c := range cases {
		if got := isOperationalScript(c.rel); got != c.want {
			t.Errorf("isOperationalScript(%q) = %v, want %v", c.rel, got, c.want)
		}
	}
}

// TestAgentTemplateLaunchCommandMatchesResolveTemplate pins the deferred
// agent-session create baseline to the controller's template command. The API
// persists config.BuildProviderTemplateLaunchCommand's command before the
// controller resolves the agent, and the async-start drift check rolls the
// create back unless resolveTemplate composes the same command (#7453). It
// covers every builtin provider on each transport it supports, a wrapped Kimi
// alias, installed agent hooks, and option defaults no flag can render.
func TestAgentTemplateLaunchCommandMatchesResolveTemplate(t *testing.T) {
	kimiBase := "builtin:kimi"
	providers := config.BuiltinProviders()
	providers["fast-kimi"] = config.ProviderSpec{Base: &kimiBase}
	providerNames := append(append([]string{}, config.BuiltinProviderOrder()...), "fast-kimi")
	hookSets := []struct {
		name  string
		hooks []string
	}{
		{name: "no-hooks"},
		{name: "all-hooks", hooks: append(hooks.SupportedProviders(), "fast-kimi")},
	}
	pinSets := []struct {
		name string
		pins map[string]string
	}{
		{name: "provider-defaults"},
		{name: "unrenderable-pins", pins: map[string]string{"bogus": "x", "permission_mode": "not-a-permission-mode", "effort": ""}},
	}
	lookPath := func(name string) (string, error) { return "/bin/" + name, nil }

	for _, providerName := range providerNames {
		probe, err := config.ResolveProvider(&config.Agent{Name: "worker", Provider: providerName}, &config.Workspace{}, providers, lookPath)
		if err != nil {
			t.Fatalf("ResolveProvider(%q): %v", providerName, err)
		}
		sessions := []string{"", config.SessionTransportTmux}
		if probe.SupportsACP {
			sessions = append(sessions, config.SessionTransportACP)
		}
		for _, session := range sessions {
			sessionLabel := session
			if sessionLabel == "" {
				sessionLabel = "provider-default"
			}
			for _, hookSet := range hookSets {
				for _, pinSet := range pinSets {
					t.Run(fmt.Sprintf("%s/%s/%s/%s", providerName, sessionLabel, hookSet.name, pinSet.name), func(t *testing.T) {
						cityDir := t.TempDir()
						cfgAgent := &config.Agent{
							Name:              "worker",
							Provider:          providerName,
							Session:           session,
							InstallAgentHooks: hookSet.hooks,
							OptionDefaults:    maps.Clone(pinSet.pins),
						}
						bp := &agentBuildParams{
							cityName:   "city",
							cityPath:   cityDir,
							workspace:  &config.Workspace{Name: "city"},
							providers:  providers,
							lookPath:   lookPath,
							fs:         fsys.OSFS{},
							rigs:       []config.Rig{},
							beaconTime: testBeaconTime,
							beadNames:  make(map[string]string),
							stderr:     io.Discard,
						}
						// Resolve the template first: it projects Claude's
						// .gc/settings.json, as a running controller already
						// has by the time the API accepts a create.
						tp, err := resolveTemplate(bp, cfgAgent, "worker", nil)
						if err != nil {
							t.Fatalf("resolveTemplate: %v", err)
						}
						resolved, err := config.ResolveProvider(cfgAgent, bp.workspace, bp.providers, bp.lookPath)
						if err != nil {
							t.Fatalf("ResolveProvider: %v", err)
						}
						transport := config.ResolveSessionCreateTransport(cfgAgent.Session, resolved)
						launch, err := config.BuildProviderTemplateLaunchCommand(cityDir, resolved, transport, config.ResolveInstallHooks(cfgAgent, bp.workspace), bp.providers)
						if err != nil {
							t.Fatalf("BuildProviderTemplateLaunchCommand: %v", err)
						}
						if launch.Command != tp.Command {
							t.Errorf("deferred-create baseline = %q, want resolveTemplate's command %q", launch.Command, tp.Command)
						}
					})
				}
			}
		}
	}
}

func TestT3BridgeStartupEnvelopeModel_PrefersResolvedEnvModel(t *testing.T) {
	tp := TemplateParams{
		Env: map[string]string{
			"GC_PROVIDER": "codex",
			"GC_MODEL":    "gpt-5.4-mini",
		},
	}

	if got := t3BridgeStartupEnvelopeModel(tp.Env["GC_PROVIDER"], tp); got != "gpt-5.4-mini" {
		t.Fatalf("startupEnvelopeModel() = %q, want gpt-5.4-mini", got)
	}
}

func TestT3BridgeStartupEnvelopeModel_UsesCurrentProviderDefaults(t *testing.T) {
	tests := []struct {
		name string
		tp   TemplateParams
		want string
	}{
		{
			name: "codex",
			tp:   TemplateParams{Env: map[string]string{"GC_PROVIDER": "codex"}},
			want: "gpt-5-codex",
		},
		{
			name: "codex-mini",
			tp:   TemplateParams{Env: map[string]string{"GC_PROVIDER": "codex-mini"}},
			want: "claude-opus-4-6",
		},
		{
			name: "claude",
			tp:   TemplateParams{Env: map[string]string{"GC_PROVIDER": "claude"}},
			want: "claude-opus-4-6",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := t3BridgeStartupEnvelopeModel(tt.tp.Env["GC_PROVIDER"], tt.tp); got != tt.want {
				t.Fatalf("startupEnvelopeModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTemplateParamsToConfigInjectsGCAliasFromAlias(t *testing.T) {
	tp := TemplateParams{
		Command:                  "codex",
		TemplateName:             "t3code/witness",
		InstanceName:             "t3code/witness",
		Alias:                    "t3code/witness",
		EffectiveSessionProvider: "t3bridge",
		Env: map[string]string{
			"GC_AGENT":        "t3code/witness",
			"GC_SESSION_NAME": "t3code--witness",
		},
	}

	cfg := templateParamsToConfig(tp)
	if got := cfg.Env["GC_ALIAS"]; got != "t3code/witness" {
		t.Fatalf("GC_ALIAS = %q, want %q", got, "t3code/witness")
	}
}

func TestTemplateParamsToConfigPreservesExistingGCAlias(t *testing.T) {
	tp := TemplateParams{
		Command:                  "codex",
		TemplateName:             "deacon",
		InstanceName:             "deacon",
		Alias:                    "deacon",
		EffectiveSessionProvider: "t3bridge",
		Env: map[string]string{
			"GC_AGENT":        "deacon",
			"GC_SESSION_NAME": "deacon",
			"GC_ALIAS":        "custom-alias",
		},
	}

	cfg := templateParamsToConfig(tp)
	if got := cfg.Env["GC_ALIAS"]; got != "custom-alias" {
		t.Fatalf("GC_ALIAS = %q, want %q", got, "custom-alias")
	}
	if got := tp.Env["GC_ALIAS"]; got != "custom-alias" {
		t.Fatalf("tp.Env[GC_ALIAS] mutated to %q", got)
	}
}

func TestTemplateParamsToConfigDoesNotInjectGCAliasForNonT3Provider(t *testing.T) {
	tp := TemplateParams{
		Command:                  "codex",
		TemplateName:             "deacon",
		InstanceName:             "deacon",
		Alias:                    "deacon",
		EffectiveSessionProvider: "",
		Env: map[string]string{
			"GC_AGENT":        "deacon",
			"GC_SESSION_NAME": "deacon",
		},
	}

	cfg := templateParamsToConfig(tp)
	if got := cfg.Env["GC_ALIAS"]; got != "" {
		t.Fatalf("GC_ALIAS = %q, want empty for non-t3 provider", got)
	}
}

func TestBuildT3BridgeStartupEnvelopeOnlyForT3Provider(t *testing.T) {
	nonT3 := TemplateParams{
		Command:                  "codex",
		TemplateName:             "deacon",
		InstanceName:             "deacon",
		EffectiveSessionProvider: "",
		Env: map[string]string{
			"GC_CITY_PATH": "/data/projects/gc",
			"GC_PROVIDER":  "codex",
		},
	}
	if got := buildT3BridgeStartupEnvelope(nonT3, "prompt"); got != nil {
		t.Fatalf("buildT3BridgeStartupEnvelope(nonT3) = %q, want nil", string(got))
	}

	t3 := nonT3
	t3.EffectiveSessionProvider = "t3bridge"
	if got := buildT3BridgeStartupEnvelope(t3, "prompt"); len(got) == 0 {
		t.Fatal("buildT3BridgeStartupEnvelope(t3) = empty, want envelope")
	}
}
