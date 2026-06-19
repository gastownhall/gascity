package config

// UpstreamSpec is a named model-serving endpoint preset (Phase C — the Upstream
// axis: WHO serves+resolves the model). An agent selects it (agent.upstream,
// falling back to agent_defaults.upstream); the resolver injects its serving env
// into the session LAST, so it overrides ambient/agent env.
//
// Two forms, which compose:
//   - ABSTRACT (portable): BaseURL/APIKey/AuthToken are harness-AGNOSTIC. The
//     resolver renders them onto the agent's HARNESS env-var names (the harness's
//     UpstreamEnvBinding) — so one upstream works on claude (→ ANTHROPIC_*) AND
//     codex (→ OPENAI_*). An abstract field with no matching harness binding is a
//     hard error (never a silent no-op).
//   - RAW (escape hatch): Env is harness-specific env keys, merged AFTER the
//     abstract render. Use for vars the abstract trio doesn't cover, or when a
//     harness has no binding.
//
// Values (abstract and raw) may reference controller env vars via $VAR/${VAR},
// expanded at resolution — so SECRETS ARE NEVER INLINED, e.g. api_key =
// "$ANTHROPIC_API_KEY". The resolved serving env is excluded from the fingerprint
// (the env allow-list); only the selected NAME is hashed (runtime.Config.Upstream,
// launch-half) to drive a warm-box relaunch on a switch.
//
//	[upstreams.bedrock]
//	base_url = "https://bedrock.example.com/anthropic"
//	api_key  = "$AWS_BEDROCK_KEY"   # rendered to the harness's api_key env var
type UpstreamSpec struct {
	// Description is a human-readable summary shown in tooling.
	Description string `toml:"description,omitempty"`
	// BaseURL is the abstract serving endpoint, rendered onto the harness's
	// base_url env var name (UpstreamEnvBinding.BaseURL).
	BaseURL string `toml:"base_url,omitempty"`
	// APIKey is the abstract credential, rendered onto the harness's api_key env
	// var name. May be a $VAR ref so the secret stays out of config.
	APIKey string `toml:"api_key,omitempty"`
	// AuthToken is an abstract bearer-token credential (an alternative to APIKey
	// for harnesses/upstreams that use a token), rendered onto the harness's
	// auth_token env var name.
	AuthToken string `toml:"auth_token,omitempty"`
	// BaseURLEnv/APIKeyEnv/AuthTokenEnv override the HARNESS binding's env-var
	// name for the corresponding abstract field. Needed for GATEWAY harnesses —
	// one CLI (e.g. opencode) fronting many upstreams where the credential env
	// var is upstream-dependent (GROQ_API_KEY, CEREBRAS_API_KEY, …), so the
	// HARNESS has no single binding and the UPSTREAM names its own target.
	// Precedence per field: this override > the harness binding > error.
	BaseURLEnv   string `toml:"base_url_env,omitempty"`
	APIKeyEnv    string `toml:"api_key_env,omitempty"`
	AuthTokenEnv string `toml:"auth_token_env,omitempty"`
	// Env is a harness-specific escape hatch: raw env keys merged AFTER the
	// abstract fields render. Values may use $VAR refs.
	Env map[string]string `toml:"env,omitempty"`
}

// HasAbstractServing reports whether the upstream sets any abstract field that
// must be rendered through a harness binding.
func (u UpstreamSpec) HasAbstractServing() bool {
	return u.BaseURL != "" || u.APIKey != "" || u.AuthToken != ""
}

// UpstreamEnvBinding is a harness's serving-env contract: the env-var NAMES this
// CLI reads for the model-serving endpoint and credential. The resolver maps an
// abstract UpstreamSpec onto these names so an upstream preset is harness-portable
// (declared per harness, e.g. claude → ANTHROPIC_*, codex → OPENAI_*).
type UpstreamEnvBinding struct {
	// BaseURL is the env var name the harness reads for the serving base URL.
	BaseURL string `toml:"base_url,omitempty"`
	// APIKey is the env var name the harness reads for the API key.
	APIKey string `toml:"api_key,omitempty"`
	// AuthToken is the env var name the harness reads for a bearer auth token.
	AuthToken string `toml:"auth_token,omitempty"`
}

// IsZero reports whether the binding declares no serving-env names.
func (b UpstreamEnvBinding) IsZero() bool {
	return b.BaseURL == "" && b.APIKey == "" && b.AuthToken == ""
}
