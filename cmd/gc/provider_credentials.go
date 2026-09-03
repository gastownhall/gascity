package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/processenv"
)

// credentialBinding is one declared credential role of a provider, resolved
// down to the environment variable that holds the secret.
//
// Exactly one of SourceVar and Refusal is set. An unresolvable role is
// reported, not skipped: on a credential path a silently-omitted entry is the
// failure that matters, because the operator concludes there was nothing to
// change.
type credentialBinding struct {
	// Role is the upstream_env role this entry came from: "api_key" or
	// "auth_token".
	Role string
	// EnvKey is the harness env var the role binds to, e.g. ANTHROPIC_API_KEY.
	EnvKey string
	// SourceVar is the environment variable that holds the credential — the
	// one an operator changes. Empty when Refusal is set.
	SourceVar string
	// Inherited reports that the provider's env declares nothing for EnvKey,
	// so the harness receives it straight from the supervisor environment
	// under its own name. SourceVar then equals EnvKey.
	Inherited bool
	// Refusal explains why no single variable holds this role's credential.
	// Empty when SourceVar is set.
	Refusal string
}

// providerCredentialSources resolves which environment variables back a
// provider's credentials, using the provider's own declaration rather than any
// inference from variable names.
//
// A provider's [config.UpstreamEnvBinding] states the roles structurally:
// api_key and auth_token name the harness env vars that carry a secret,
// base_url names the one that carries an endpoint. Only the first two are
// credentials. base_url is absent here and must stay absent: assigning a
// credential to the variable behind a base URL destroys the provider's
// routing.
//
// Deliberately NOT used: the envArgvSafe allow-list in internal/runtime. It
// answers "may this value appear in argv?", whose fail-safe is "unknown means
// assume secret" because the cost of being wrong is a temp file. This asks
// "which variable is the credential?", where naming the wrong one points the
// operator at a live endpoint value. ANTHROPIC_BASE_URL is not on that
// allow-list, so reusing it would classify the endpoint as a credential.
//
// A provider declaring neither credential role yields no entries; the caller
// refuses rather than guessing which of its env keys holds a secret.
func providerCredentialSources(resolved *config.ResolvedProvider) []credentialBinding {
	if resolved == nil {
		return nil
	}
	roles := []struct{ role, envKey string }{
		{"api_key", resolved.UpstreamEnv.APIKey},
		{"auth_token", resolved.UpstreamEnv.AuthToken},
	}

	out := make([]credentialBinding, 0, len(roles))
	for _, r := range roles {
		if r.envKey == "" {
			continue
		}
		out = append(out, resolveCredentialBinding(resolved, r.role, r.envKey))
	}
	return out
}

// resolveCredentialBinding resolves one declared role against the provider's
// merged env map.
func resolveCredentialBinding(resolved *config.ResolvedProvider, role, envKey string) credentialBinding {
	b := credentialBinding{Role: role, EnvKey: envKey}
	value, ok := resolved.Env[envKey]
	switch {
	case !ok:
		// The provider declares nothing for this key, so the harness reads it
		// from the session env, where the supervisor's own value for that
		// name arrives. The variable to change is therefore the key itself.
		b.SourceVar = envKey
		b.Inherited = true
	case value == "":
		b.Refusal = fmt.Sprintf("%s is set empty, which withholds the variable rather than supplying a credential", envKey)
	default:
		if source, isRef := processenv.SoleReferencedEnvVar(value); isRef {
			b.SourceVar = source
			break
		}
		if !processenv.HasEnvRef(value) {
			b.Refusal = fmt.Sprintf("%s is a literal value, so the credential is written into the config itself; change it where it is written", envKey)
			break
		}
		b.Refusal = fmt.Sprintf("%s interpolates more than one variable, so no single variable holds the credential on its own", envKey)
	}
	return b
}

// credentialOverride records a config layer that sets one of a provider's
// credential env keys AFTER the provider layer, so the variable the running
// agent actually reads is not the one the provider names.
//
// Session start merges env as passthrough < workspace < provider < agent
// (template_resolve.go), then injects the selected [upstreams.<name>] serving
// env LAST, so any of these wins over the provider entry this command
// resolves. Reporting them is not a nicety: a rotation aimed at the provider's
// variable would leave such an agent authenticating with the old credential.
type credentialOverride struct {
	// Layer names the config layer: "upstreams", "workspace.env" or "agent.env".
	Layer string
	// Detail identifies the specific entry, e.g. the agent and upstream names.
	Detail string
	// EnvKey is the credential key the layer overrides.
	EnvKey string
}

// credentialOverrides finds the config layers that override a provider's
// credential env keys for any agent using that provider.
//
// It reports rather than resolves, because the effective variable is
// agent-scoped: two agents on the same provider can select different
// upstreams. A provider-scoped answer cannot be correct for both, so the
// command states which agents diverge instead of picking one.
func credentialOverrides(cfg *config.City, providerName string, bindings []credentialBinding) []credentialOverride {
	if cfg == nil {
		return nil
	}
	keys := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		if b.EnvKey != "" {
			keys[b.EnvKey] = true
		}
	}
	if len(keys) == 0 {
		return nil
	}

	var out []credentialOverride
	for key := range keys {
		if _, ok := cfg.Workspace.Env[key]; ok {
			out = append(out, credentialOverride{Layer: "workspace.env", Detail: "[workspace.env]", EnvKey: key})
		}
	}

	for _, agent := range cfg.Agents {
		if !agentUsesProvider(cfg, agent, providerName) {
			continue
		}
		for key := range keys {
			if _, ok := agent.Env[key]; ok {
				out = append(out, credentialOverride{
					Layer:  "agent.env",
					Detail: fmt.Sprintf("agent %q env", agent.Name),
					EnvKey: key,
				})
			}
		}
		if agent.Upstream == "" {
			continue
		}
		spec, ok := cfg.Upstreams[agent.Upstream]
		if !ok {
			continue
		}
		for key := range keys {
			if upstreamSetsEnvKey(spec, key, bindings) {
				out = append(out, credentialOverride{
					Layer:  "upstreams",
					Detail: fmt.Sprintf("agent %q selects upstream %q", agent.Name, agent.Upstream),
					EnvKey: key,
				})
			}
		}
		for key := range keys {
			if _, ok := spec.Env[key]; ok {
				out = append(out, credentialOverride{
					Layer:  "upstreams",
					Detail: fmt.Sprintf("agent %q upstream %q raw env", agent.Name, agent.Upstream),
					EnvKey: key,
				})
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].EnvKey != out[j].EnvKey {
			return out[i].EnvKey < out[j].EnvKey
		}
		if out[i].Layer != out[j].Layer {
			return out[i].Layer < out[j].Layer
		}
		return out[i].Detail < out[j].Detail
	})
	return dedupeOverrides(out)
}

// upstreamSetsEnvKey reports whether the upstream's abstract serving fields
// render onto key, mirroring the per-field name precedence session start uses:
// the upstream's own *_env override wins, else the harness binding.
func upstreamSetsEnvKey(spec config.UpstreamSpec, key string, bindings []credentialBinding) bool {
	bound := func(role string) string {
		for _, b := range bindings {
			if b.Role == role {
				return b.EnvKey
			}
		}
		return ""
	}
	for _, r := range []struct{ value, override, role string }{
		{spec.APIKey, spec.APIKeyEnv, "api_key"},
		{spec.AuthToken, spec.AuthTokenEnv, "auth_token"},
	} {
		if r.value == "" {
			continue
		}
		name := r.override
		if name == "" {
			name = bound(r.role)
		}
		if name == key {
			return true
		}
	}
	return false
}

// agentUsesProvider reports whether the agent resolves to providerName,
// falling back to the workspace default the way agent resolution does.
func agentUsesProvider(cfg *config.City, agent config.Agent, providerName string) bool {
	name := strings.TrimSpace(agent.Provider)
	if name == "" {
		name = strings.TrimSpace(cfg.Workspace.Provider)
	}
	return name == providerName
}

// dedupeOverrides collapses identical entries from repeated agents.
func dedupeOverrides(in []credentialOverride) []credentialOverride {
	out := in[:0]
	var prev credentialOverride
	for i, o := range in {
		if i > 0 && o == prev {
			continue
		}
		out = append(out, o)
		prev = o
	}
	return out
}
