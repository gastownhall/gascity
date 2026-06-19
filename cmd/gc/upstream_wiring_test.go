package main

import (
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

func upstreamTestParams(t *testing.T, city *config.City) *agentBuildParams {
	t.Helper()
	cityPath := t.TempDir()
	writeTemplateResolveCityConfig(t, cityPath, "file")
	return &agentBuildParams{
		city:       city,
		cityName:   "city",
		cityPath:   cityPath,
		workspace:  &config.Workspace{Provider: "test"},
		providers:  map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none"}},
		lookPath:   func(string) (string, error) { return "/bin/echo", nil },
		fs:         fsys.OSFS{},
		beaconTime: time.Unix(0, 0),
		beadNames:  make(map[string]string),
		stderr:     io.Discard,
	}
}

// The Upstream axis end-to-end: a selected upstream injects its serving env
// (with $VAR refs resolved from the controller env) into the session Config.Env,
// and the selected NAME flows to Config.Upstream — so switching upstream moves
// LaunchFingerprint (a B2.3 warm relaunch) while the credential value stays out
// of every fingerprint.
func TestResolveTemplateInjectsUpstreamServingEnv(t *testing.T) {
	t.Setenv("MY_ANTHROPIC_KEY", "sk-ant-secret")
	city := &config.City{Upstreams: map[string]config.UpstreamSpec{
		"bedrock": {Env: map[string]string{
			"ANTHROPIC_BASE_URL": "https://bedrock.example/anthropic",
			"ANTHROPIC_API_KEY":  "$MY_ANTHROPIC_KEY",
		}},
	}}
	params := upstreamTestParams(t, city)
	agent := &config.Agent{Name: "runner", Upstream: "bedrock"}

	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if tp.Upstream != "bedrock" {
		t.Errorf("tp.Upstream = %q, want bedrock", tp.Upstream)
	}
	if got := tp.Env["ANTHROPIC_BASE_URL"]; got != "https://bedrock.example/anthropic" {
		t.Errorf("ANTHROPIC_BASE_URL = %q, want the upstream base url", got)
	}
	if got := tp.Env["ANTHROPIC_API_KEY"]; got != "sk-ant-secret" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the $VAR-resolved secret", got)
	}

	cfg := templateParamsToConfig(tp)
	if cfg.Upstream != "bedrock" {
		t.Errorf("cfg.Upstream = %q, want bedrock", cfg.Upstream)
	}
	if cfg.Env["ANTHROPIC_API_KEY"] != "sk-ant-secret" {
		t.Errorf("serving secret lost from cfg.Env")
	}
	// Switching the upstream NAME moves LaunchFingerprint (→ relaunch).
	switched := cfg
	switched.Upstream = "anthropic"
	if runtime.LaunchFingerprint(cfg) == runtime.LaunchFingerprint(switched) {
		t.Error("switching upstream name must move LaunchFingerprint (so the reconciler relaunches)")
	}
}

// The same ABSTRACT upstream renders onto different env-var names per harness
// (claude → ANTHROPIC_*, codex → OPENAI_*) via the provider's upstream_env
// binding — the harness-portability payoff.
func TestResolveTemplateRendersAbstractUpstreamPerHarness(t *testing.T) {
	t.Setenv("BEDROCK_KEY", "sk-bedrock")
	city := &config.City{Upstreams: map[string]config.UpstreamSpec{
		"bedrock": {BaseURL: "https://bedrock.example/anthropic", APIKey: "$BEDROCK_KEY"},
	}}
	for _, tc := range []struct {
		harness                   string
		binding                   config.UpstreamEnvBinding
		baseKey, keyKey, otherKey string
	}{
		{"claude", config.UpstreamEnvBinding{BaseURL: "ANTHROPIC_BASE_URL", APIKey: "ANTHROPIC_API_KEY"}, "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "OPENAI_API_KEY"},
		{"codex", config.UpstreamEnvBinding{BaseURL: "OPENAI_BASE_URL", APIKey: "OPENAI_API_KEY"}, "OPENAI_BASE_URL", "OPENAI_API_KEY", "ANTHROPIC_API_KEY"},
	} {
		t.Run(tc.harness, func(t *testing.T) {
			params := upstreamTestParams(t, city)
			params.providers = map[string]config.ProviderSpec{"test": {Command: "echo", PromptMode: "none", UpstreamEnv: tc.binding}}
			agent := &config.Agent{Name: "runner", Upstream: "bedrock"}
			tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
			if err != nil {
				t.Fatalf("resolveTemplate: %v", err)
			}
			if got := tp.Env[tc.baseKey]; got != "https://bedrock.example/anthropic" {
				t.Errorf("%s = %q, want the bedrock base url", tc.baseKey, got)
			}
			if got := tp.Env[tc.keyKey]; got != "sk-bedrock" {
				t.Errorf("%s = %q, want the $VAR-resolved key", tc.keyKey, got)
			}
			if _, ok := tp.Env[tc.otherKey]; ok {
				t.Errorf("%s should not be set (renders only to this harness's binding)", tc.otherKey)
			}
		})
	}
}

// An abstract upstream field with no matching harness binding is a hard error
// (config-surface §4: never a silent no-op).
func TestResolveTemplateAbstractUpstreamNoBindingErrors(t *testing.T) {
	city := &config.City{Upstreams: map[string]config.UpstreamSpec{
		"bedrock": {BaseURL: "https://bedrock.example/anthropic"},
	}}
	params := upstreamTestParams(t, city) // provider "test" declares no upstream_env binding
	agent := &config.Agent{Name: "runner", Upstream: "bedrock"}
	if _, err := resolveTemplate(params, agent, agent.QualifiedName(), nil); err == nil {
		t.Fatal("expected an error: abstract upstream field with no harness binding")
	}
}

func TestResolveTemplateRejectsUnknownUpstream(t *testing.T) {
	params := upstreamTestParams(t, &config.City{})
	agent := &config.Agent{Name: "runner", Upstream: "nope"}
	if _, err := resolveTemplate(params, agent, agent.QualifiedName(), nil); err == nil {
		t.Fatal("expected an error when an agent selects an undeclared upstream")
	}
}

// No upstream selected is behavior-identical: no Config.Upstream, no injected
// serving env.
func TestResolveTemplateNoUpstreamIsInert(t *testing.T) {
	params := upstreamTestParams(t, &config.City{})
	agent := &config.Agent{Name: "runner"}
	tp, err := resolveTemplate(params, agent, agent.QualifiedName(), nil)
	if err != nil {
		t.Fatalf("resolveTemplate: %v", err)
	}
	if tp.Upstream != "" {
		t.Errorf("tp.Upstream = %q, want empty", tp.Upstream)
	}
	if templateParamsToConfig(tp).Upstream != "" {
		t.Error("cfg.Upstream should be empty when no upstream is selected")
	}
}
