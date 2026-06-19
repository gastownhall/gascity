package config

// UpstreamSpec is a named model-serving endpoint preset (Phase C — the Upstream
// axis: WHO serves+resolves the model). When an agent selects this upstream
// (agent.upstream, falling back to agent_defaults.upstream), its Env block is
// injected into the session environment LAST, so it overrides ambient/agent env.
//
// Values may reference controller environment variables via $VAR / ${VAR},
// expanded at resolution — so SECRETS ARE NEVER INLINED in config, e.g.
// api_key = "$ANTHROPIC_API_KEY". The resolved serving env is excluded from the
// fingerprint (the env allow-list), so switching upstream or rotating a
// credential never leaks a secret into the persisted bead metadata; only the
// SELECTED NAME is hashed (runtime.Config.Upstream, launch-half) to drive a
// warm-box relaunch on a switch.
//
//	[upstreams.anthropic]
//	env = { ANTHROPIC_BASE_URL = "https://api.anthropic.com", ANTHROPIC_API_KEY = "$ANTHROPIC_API_KEY" }
//
//	[upstreams.bedrock]
//	env = { ANTHROPIC_BASE_URL = "https://bedrock.example.com/anthropic", AWS_BEARER_TOKEN_BEDROCK = "$AWS_BEARER_TOKEN_BEDROCK" }
type UpstreamSpec struct {
	// Description is a human-readable summary shown in tooling.
	Description string `toml:"description,omitempty"`
	// Env is the serving environment for this upstream (base URL + credential
	// refs). Values may use $VAR / ${VAR} to reference controller env vars, so
	// secrets stay out of config; it is injected into the session env and is
	// excluded from the fingerprint.
	Env map[string]string `toml:"env,omitempty"`
}
