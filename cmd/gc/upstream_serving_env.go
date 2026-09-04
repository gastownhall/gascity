package main

import "github.com/gastownhall/gascity/internal/config"

// upstreamServingField is one abstract serving field of an [upstreams.<name>]
// spec, paired with the harness env-var name it renders onto.
type upstreamServingField struct {
	// Field is the abstract field name: "base_url", "api_key" or
	// "auth_token". It appears in operator-facing errors and reports.
	Field string
	// Value is what the spec sets for that field, before expansion. Empty
	// means the upstream does not set this field at all.
	Value string
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
// Fields the upstream leaves empty are returned with an empty Value; callers
// skip them, since an unset abstract field renders nothing.
func upstreamServingFields(spec config.UpstreamSpec, binding config.UpstreamEnvBinding) []upstreamServingField {
	out := []upstreamServingField{
		{Field: "base_url", Value: spec.BaseURL, EnvName: spec.BaseURLEnv},
		{Field: "api_key", Value: spec.APIKey, EnvName: spec.APIKeyEnv},
		{Field: "auth_token", Value: spec.AuthToken, EnvName: spec.AuthTokenEnv},
	}
	bound := []string{binding.BaseURL, binding.APIKey, binding.AuthToken}
	for i := range out {
		if out[i].EnvName == "" {
			out[i].EnvName = bound[i]
		}
	}
	return out
}
