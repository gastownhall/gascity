package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/processenv"
)

// credentialSourceKind is the resolved state of one declared credential role.
// It replaces a pair of fields whose valid combinations lived only in prose.
type credentialSourceKind int

const (
	// credentialUnresolved: no single variable holds this role's credential,
	// and Refusal says why.
	credentialUnresolved credentialSourceKind = iota
	// credentialDeclared: the provider's env names the variable in SourceVar.
	credentialDeclared
	// credentialInherited: the provider declares nothing for EnvKey, so the
	// harness reads that name straight out of the session environment.
	// SourceVar is EnvKey.
	credentialInherited
)

// credentialBinding is one declared credential role of a provider, resolved
// down to the environment variable that holds the secret.
//
// Build these only through the three constructors below, which are what make
// Kind, SourceVar and Refusal consistent.
type credentialBinding struct {
	// Role is the upstream_env role this entry came from: "api_key" or
	// "auth_token".
	Role string
	// EnvKey is the harness env var the role binds to, e.g. ANTHROPIC_API_KEY.
	EnvKey string
	// Kind says which of the remaining fields is meaningful.
	Kind credentialSourceKind
	// SourceVar is the environment variable that holds the credential — the
	// one an operator changes. Empty when Kind is credentialUnresolved.
	SourceVar string
	// Refusal explains why no single variable holds this role's credential.
	// Empty unless Kind is credentialUnresolved.
	Refusal string
}

// Resolved reports whether this role has a variable an operator can change.
func (b credentialBinding) Resolved() bool { return b.Kind != credentialUnresolved }

func declaredCredential(role, envKey, sourceVar string) credentialBinding {
	return credentialBinding{Role: role, EnvKey: envKey, Kind: credentialDeclared, SourceVar: sourceVar}
}

// inheritedCredential records that nothing in provider config names this key,
// so the harness receives it from the session environment under its own name.
func inheritedCredential(role, envKey string) credentialBinding {
	return credentialBinding{Role: role, EnvKey: envKey, Kind: credentialInherited, SourceVar: envKey}
}

func unresolvedCredential(role, envKey, reason string) credentialBinding {
	return credentialBinding{Role: role, EnvKey: envKey, Kind: credentialUnresolved, Refusal: reason}
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
// The envArgvSafe allow-list in internal/runtime is deliberately not reused:
// it answers "may this value appear in argv?" with the fail-safe "unknown
// means assume secret", and ANTHROPIC_BASE_URL is absent from it, so borrowing
// it here would classify a live endpoint as a credential.
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
	value, ok := resolved.Env[envKey]
	if !ok {
		return inheritedCredential(role, envKey)
	}
	if value == "" {
		return unresolvedCredential(role, envKey,
			fmt.Sprintf("%s is set empty, which withholds the variable rather than supplying a credential", envKey))
	}
	switch refs := processenv.ReferencedEnvVars(value); len(refs) {
	case 1:
		return declaredCredential(role, envKey, refs[0])
	case 0:
		return unresolvedCredential(role, envKey,
			fmt.Sprintf("%s is a literal value, so the credential is written into the config itself; change it where it is written", envKey))
	default:
		return unresolvedCredential(role, envKey,
			fmt.Sprintf("%s interpolates %d variables (%s), so no single variable holds the credential on its own",
				envKey, len(refs), strings.Join(refs, ", ")))
	}
}

// credentialOverride records a config layer whose entry for a credential env
// key WINS over the provider's, so the variable the running agent actually
// reads is not the one the provider names.
//
// Session start merges env as passthrough < workspace < provider < agent
// (template_resolve.go:469), then injects the selected [upstreams.<name>]
// serving env LAST. Only the layers AFTER the provider can override it:
// agent.env and upstreams. Reporting them is not a nicety — a rotation aimed
// at the provider's variable would leave such an agent authenticating with the
// old credential.
//
// [workspace.env] sits BEFORE the provider, so it is not an override of a
// credential the provider declares — the provider's entry wins and the
// workspace entry is dead. It matters only for an INHERITED binding, where the
// provider declares nothing for the key: there the workspace entry is what
// lands in the session env, in place of the value the supervisor would have
// passed through. Reporting it in the declared case would name the wrong
// variable to change, which is the failure this command exists to prevent.
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
	// Keys the provider does NOT declare, so an earlier layer's entry survives
	// the merge instead of being overwritten by the provider's.
	inherited := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		if b.EnvKey == "" {
			continue
		}
		keys[b.EnvKey] = true
		if b.Kind == credentialInherited {
			inherited[b.EnvKey] = true
		}
	}
	if len(keys) == 0 {
		return nil
	}

	var out []credentialOverride
	// Scoped to the agents on this provider: [workspace.env] applies to every
	// session, but it can only displace THIS provider's credential for an
	// agent that uses it, and with no such agent there is nothing to report.
	if anyAgentUsesProvider(cfg, providerName) {
		for key := range inherited {
			if _, ok := cfg.Workspace.Env[key]; ok {
				out = append(out, credentialOverride{Layer: "workspace.env", Detail: "[workspace.env]", EnvKey: key})
			}
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
// render onto key.
//
// The per-field name precedence comes from [upstreamServingFields], the same
// helper session start renders through, so this cannot drift into reporting a
// rotation that lands somewhere else. base_url is skipped here for the reason
// stated on providerCredentialSources: it names an endpoint, not a credential.
func upstreamSetsEnvKey(spec config.UpstreamSpec, key string, bindings []credentialBinding) bool {
	binding := config.UpstreamEnvBinding{}
	for _, b := range bindings {
		switch b.Role {
		case "api_key":
			binding.APIKey = b.EnvKey
		case "auth_token":
			binding.AuthToken = b.EnvKey
		}
	}
	for _, r := range upstreamServingFields(spec, binding) {
		if r.Field == "base_url" || r.Value == "" {
			continue
		}
		if r.EnvName == key {
			return true
		}
	}
	return false
}

// anyAgentUsesProvider reports whether any configured agent resolves to
// providerName.
func anyAgentUsesProvider(cfg *config.City, providerName string) bool {
	for _, agent := range cfg.Agents {
		if agentUsesProvider(cfg, agent, providerName) {
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
