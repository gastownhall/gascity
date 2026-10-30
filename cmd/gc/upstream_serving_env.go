package main

import "github.com/gastownhall/gascity/internal/config"

// upstreamServingField is one abstract serving field of an [upstreams.<name>]
// spec, paired with the harness env-var name it renders onto.
type upstreamServingField struct {
	// Field is the abstract field name: "base_url", "api_key" or
	// "auth_token". It appears in operator-facing errors and reports.
	Field string
	// Set reports whether the upstream sets this field at all. The value
	// itself is deliberately not carried: it is usually a credential, and the
	// reporting side needs only the name it lands on.
	Set bool
	// EnvName is the env var the value renders onto, or "" when neither the
	// upstream nor the harness names one — which session start treats as a
	// hard error rather than a silent no-op.
	EnvName string
}

// upstreamServingFields resolves each abstract serving field of an upstream to
// the harness env var it writes, applying the per-field name precedence:
// the upstream's own *_env override wins (the gateway-harness escape hatch),
// else the resolved provider's upstream_env binding.
//
// This is the single statement of that precedence. Session start
// (template_resolve.go) renders the values through it; `gc provider
// credentials` reads it to tell an operator that a selected upstream
// overrides the credential the provider names. Two copies of this rule
// disagreeing is the report saying a rotation lands where it does not, so
// both callers read it from here.
//
// Fields the upstream leaves empty are returned with Set false; callers skip
// them, since an unset abstract field renders nothing. The fields are in the
// order base_url, api_key, auth_token, matching upstreamServingValues.
func upstreamServingFields(spec config.UpstreamSpec, binding config.UpstreamEnvBinding) []upstreamServingField {
	out := []upstreamServingField{
		{Field: "base_url", Set: spec.BaseURL != "", EnvName: spec.BaseURLEnv},
		{Field: "api_key", Set: spec.APIKey != "", EnvName: spec.APIKeyEnv},
		{Field: "auth_token", Set: spec.AuthToken != "", EnvName: spec.AuthTokenEnv},
	}
	bound := []string{binding.BaseURL, binding.APIKey, binding.AuthToken}
	for i := range out {
		if out[i].EnvName == "" {
			out[i].EnvName = bound[i]
		}
	}
	return out
}

// upstreamServingValues returns the upstream's raw serving values in the same
// order as upstreamServingFields. Only session start reads them.
func upstreamServingValues(spec config.UpstreamSpec) [3]string {
	return [3]string{spec.BaseURL, spec.APIKey, spec.AuthToken}
}
