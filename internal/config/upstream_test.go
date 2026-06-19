package config

import "testing"

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
