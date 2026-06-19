package config

import "testing"

// The popular builtin harnesses carry their serving-env binding, so an abstract
// [upstreams.<name>] works out-of-box without the user declaring one. Resolved
// through the explicit alias (base = "builtin:<name>") as a city gets it.
func TestBuiltinHarnessUpstreamBindings(t *testing.T) {
	// Direct: the binding is seeded on the builtin spec.
	if got := BuiltinProviders()["claude"].UpstreamEnv; got.BaseURL != "ANTHROPIC_BASE_URL" || got.APIKey != "ANTHROPIC_API_KEY" || got.AuthToken != "ANTHROPIC_AUTH_TOKEN" {
		t.Errorf("builtin claude binding = %+v, want ANTHROPIC_*", got)
	}
	// Resolved-through-alias: the binding survives chain resolution.
	for _, tc := range []struct {
		name, baseURL, apiKey, authToken string
	}{
		{"claude", "ANTHROPIC_BASE_URL", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"},
		{"codex", "OPENAI_BASE_URL", "OPENAI_API_KEY", ""},
	} {
		resolved, err := ResolveProviderChain(tc.name, BuiltinProviderAlias(tc.name), nil)
		if err != nil {
			t.Fatalf("%s: ResolveProviderChain: %v", tc.name, err)
		}
		got := resolved.UpstreamEnv
		if got.BaseURL != tc.baseURL || got.APIKey != tc.apiKey || got.AuthToken != tc.authToken {
			t.Errorf("%s resolved binding = %+v, want base=%q key=%q auth=%q", tc.name, got, tc.baseURL, tc.apiKey, tc.authToken)
		}
	}
}

// The harness serving-env binding parses, abstract upstream fields parse, and a
// derived harness inherits the binding through the provider chain.
func TestUpstreamHarnessBindingAndAbstractFields(t *testing.T) {
	const toml = `
[workspace]
name = "c"

[providers.claude]
command = "claude"
[providers.claude.upstream_env]
base_url = "ANTHROPIC_BASE_URL"
api_key  = "ANTHROPIC_API_KEY"

[providers.my-claude]
base = "provider:claude"

[upstreams.bedrock]
base_url = "https://bedrock.example/anthropic"
api_key  = "$AWS_BEDROCK_KEY"
`
	cfg, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	u := cfg.Upstreams["bedrock"]
	if u.BaseURL != "https://bedrock.example/anthropic" || u.APIKey != "$AWS_BEDROCK_KEY" {
		t.Errorf("abstract upstream fields not parsed: %+v", u)
	}
	if !u.HasAbstractServing() {
		t.Error("HasAbstractServing() = false, want true")
	}
	if got := cfg.Providers["claude"].UpstreamEnv.BaseURL; got != "ANTHROPIC_BASE_URL" {
		t.Errorf("claude binding base_url = %q, want ANTHROPIC_BASE_URL", got)
	}
	// A derived harness inherits the binding via MergeProviderOverBuiltin.
	resolved, err := ResolveProviderChain("my-claude", cfg.Providers["my-claude"], cfg.Providers)
	if err != nil {
		t.Fatalf("ResolveProviderChain: %v", err)
	}
	if got := resolved.UpstreamEnv.APIKey; got != "ANTHROPIC_API_KEY" {
		t.Errorf("my-claude inherited binding api_key = %q, want ANTHROPIC_API_KEY", got)
	}
}

func TestUpstreamConfigSurface(t *testing.T) {
	const toml = `
[workspace]
name = "c"

[upstreams.anthropic]
env = { ANTHROPIC_BASE_URL = "https://api.anthropic.com", ANTHROPIC_API_KEY = "$ANTHROPIC_API_KEY" }

[upstreams.bedrock]
description = "AWS Bedrock"
env = { ANTHROPIC_BASE_URL = "https://bedrock.example/anthropic", AWS_BEARER_TOKEN_BEDROCK = "$AWS_BEARER_TOKEN_BEDROCK" }

[agent_defaults]
upstream = "anthropic"

[[agent]]
name = "worker"

[[agent]]
name = "special"
upstream = "bedrock"
`
	cfg, err := Parse([]byte(toml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(cfg.Upstreams) != 2 {
		t.Fatalf("Upstreams = %d, want 2", len(cfg.Upstreams))
	}
	// Credentials are env-refs ($VAR), never inlined.
	if got := cfg.Upstreams["anthropic"].Env["ANTHROPIC_API_KEY"]; got != "$ANTHROPIC_API_KEY" {
		t.Errorf("anthropic ANTHROPIC_API_KEY = %q, want $ANTHROPIC_API_KEY (env-ref)", got)
	}
	if got := cfg.Upstreams["bedrock"].Description; got != "AWS Bedrock" {
		t.Errorf("bedrock description = %q, want %q", got, "AWS Bedrock")
	}

	// agent_defaults.upstream propagates to agents without an explicit upstream;
	// an explicit per-agent upstream wins.
	ApplyAgentDefaults(cfg)
	byName := map[string]Agent{}
	for _, a := range cfg.Agents {
		byName[a.Name] = a
	}
	if got := byName["worker"].Upstream; got != "anthropic" {
		t.Errorf("worker upstream = %q, want anthropic (inherited city default)", got)
	}
	if got := byName["special"].Upstream; got != "bedrock" {
		t.Errorf("special upstream = %q, want bedrock (explicit, not overridden by default)", got)
	}
}
