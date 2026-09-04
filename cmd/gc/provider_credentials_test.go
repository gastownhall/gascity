package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestProviderCredentialSourcesResolvesOnlyCredentialRoles pins the contract
// that only the api_key and auth_token bindings name credentials. base_url
// names an endpoint, and offering the variable behind it as a credential
// points the operator at a live routing value.
//
// The provider here is the shape a real city runs: base URL AND credential
// both env refs. A static-literal base URL cannot distinguish a resolver that
// honors the roles from one that walks every reference, so it is not used.
func TestProviderCredentialSourcesResolvesOnlyCredentialRoles(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name: "zai",
		UpstreamEnv: config.UpstreamEnvBinding{
			BaseURL:   "ANTHROPIC_BASE_URL",
			APIKey:    "ANTHROPIC_API_KEY",
			AuthToken: "ANTHROPIC_AUTH_TOKEN",
		},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL":   "${ZAI_BASE_URL}",
			"ANTHROPIC_API_KEY":    "${ACME_KEY}",
			"ANTHROPIC_AUTH_TOKEN": "${ACME_KEY}",
		},
	}

	got := providerCredentialSources(resolved)

	sources := map[string]bool{}
	for _, b := range got {
		if b.Resolved() {
			sources[b.SourceVar] = true
		}
	}
	if !sources["ACME_KEY"] {
		t.Errorf("credential source ACME_KEY not resolved; got %+v", got)
	}
	if sources["ZAI_BASE_URL"] {
		t.Errorf("ZAI_BASE_URL was resolved as a credential source; it backs upstream_env.base_url, which names an endpoint (got %+v)", got)
	}
	for _, b := range got {
		if b.Role == "base_url" || b.EnvKey == "ANTHROPIC_BASE_URL" {
			t.Errorf("base_url appeared as a credential role: %+v", b)
		}
	}
}

// TestProviderCredentialSourcesAcceptsLiteralAroundReference: the operator
// changes the REFERENCED variable, not the value, so a reference wrapped in
// literal text is resolvable — changing GW_KEY moves the secret and leaves
// "Bearer " intact through expansion.
func TestProviderCredentialSourcesAcceptsLiteralAroundReference(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name:        "gw",
		UpstreamEnv: config.UpstreamEnvBinding{AuthToken: "ANTHROPIC_AUTH_TOKEN"},
		Env:         map[string]string{"ANTHROPIC_AUTH_TOKEN": "Bearer ${GW_KEY}"},
	}
	got := providerCredentialSources(resolved)
	if len(got) != 1 {
		t.Fatalf("providerCredentialSources = %+v; want one entry", got)
	}
	if !got[0].Resolved() {
		t.Fatalf("refused (%q); changing GW_KEY rotates this credential correctly", got[0].Refusal)
	}
	if got[0].SourceVar != "GW_KEY" {
		t.Errorf("SourceVar = %q; want GW_KEY", got[0].SourceVar)
	}
}

func TestProviderCredentialSourcesRefusals(t *testing.T) {
	tests := []struct {
		name        string
		resolved    *config.ResolvedProvider
		wantRefusal []string
	}{
		{
			name: "credential written into the config itself",
			resolved: &config.ResolvedProvider{
				Name:        "inline",
				UpstreamEnv: config.UpstreamEnvBinding{APIKey: "ANTHROPIC_API_KEY"},
				Env:         map[string]string{"ANTHROPIC_API_KEY": "sk-ant-inlined"},
			},
			wantRefusal: []string{"literal value"},
		},
		{
			name: "two distinct variables feed one credential",
			resolved: &config.ResolvedProvider{
				Name:        "split",
				UpstreamEnv: config.UpstreamEnvBinding{APIKey: "ANTHROPIC_API_KEY"},
				Env:         map[string]string{"ANTHROPIC_API_KEY": "${ACME_ID}:${ACME_KEY}"},
			},
			// Naming BOTH variables is the point: a refusal that says only
			// "more than one" leaves the operator to find them by hand in the
			// merged provider env, which is where the wrong guess is made.
			wantRefusal: []string{"interpolates 2 variables", "ACME_ID", "ACME_KEY"},
		},
		{
			name: "explicitly withheld",
			resolved: &config.ResolvedProvider{
				Name:        "withheld",
				UpstreamEnv: config.UpstreamEnvBinding{APIKey: "ANTHROPIC_API_KEY"},
				Env:         map[string]string{"ANTHROPIC_API_KEY": ""},
			},
			wantRefusal: []string{"withholds the variable"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := providerCredentialSources(tt.resolved)
			if len(got) != 1 {
				t.Fatalf("providerCredentialSources = %+v; want exactly one entry", got)
			}
			if got[0].Resolved() {
				t.Fatalf("entry accepted with source %q; want a stated reason", got[0].SourceVar)
			}
			for _, want := range tt.wantRefusal {
				if !strings.Contains(got[0].Refusal, want) {
					t.Errorf("refusal = %q; want it to name %q", got[0].Refusal, want)
				}
			}
			if got[0].SourceVar != "" {
				t.Errorf("unresolvable entry still names SourceVar %q", got[0].SourceVar)
			}
		})
	}
}

// TestProviderCredentialSourcesHonorsBareDollarRef: session start expands with
// os.Expand, so the bare form is a real reference — the form expandEnvMap's
// own doc comment uses as its example.
func TestProviderCredentialSourcesHonorsBareDollarRef(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name:        "bare",
		UpstreamEnv: config.UpstreamEnvBinding{APIKey: "ANTHROPIC_API_KEY"},
		Env:         map[string]string{"ANTHROPIC_API_KEY": "$ACME_KEY"},
	}
	got := providerCredentialSources(resolved)
	if len(got) != 1 || !got[0].Resolved() {
		t.Fatalf("providerCredentialSources = %+v; want one resolved entry", got)
	}
	if got[0].SourceVar != "ACME_KEY" {
		t.Errorf("SourceVar = %q; want ACME_KEY", got[0].SourceVar)
	}
}

// TestProviderCredentialSourcesInheritsUndeclaredKey: a provider that declares
// no env entry still has the harness read the bound name out of the session
// environment, so the variable to change is that name itself.
func TestProviderCredentialSourcesInheritsUndeclaredKey(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name:        "claude",
		UpstreamEnv: config.UpstreamEnvBinding{APIKey: "KIMI_API_KEY"},
		Env:         map[string]string{},
	}
	got := providerCredentialSources(resolved)
	if len(got) != 1 {
		t.Fatalf("providerCredentialSources = %+v; want one entry", got)
	}
	if !got[0].Resolved() {
		t.Fatalf("refused (%q); the harness reads this name from the session environment", got[0].Refusal)
	}
	if got[0].SourceVar != "KIMI_API_KEY" || got[0].Kind != credentialInherited {
		t.Errorf("got SourceVar=%q Kind=%v; want KIMI_API_KEY / credentialInherited", got[0].SourceVar, got[0].Kind)
	}
}

// TestProviderCredentialSourcesNoBindingDeclared: with no declared credential
// role, which env key holds a secret is stated nowhere. Inferring it from a
// key's name is what a matcher would do, and a wrong inference points the
// operator at the wrong variable.
func TestProviderCredentialSourcesNoBindingDeclared(t *testing.T) {
	resolved := &config.ResolvedProvider{
		Name:        "nobinding",
		UpstreamEnv: config.UpstreamEnvBinding{BaseURL: "ANTHROPIC_BASE_URL"},
		Env: map[string]string{
			"ANTHROPIC_BASE_URL": "${ACME_BASE_URL}",
			"ANTHROPIC_API_KEY":  "${ACME_KEY}",
		},
	}
	if got := providerCredentialSources(resolved); len(got) != 0 {
		t.Fatalf("providerCredentialSources = %+v; want none — no api_key or auth_token binding is declared, and ACME_KEY must not be inferred from the key's name", got)
	}
}

// TestCredentialOverridesFindsAgentScopedLayers guards the divergence that
// makes a provider-scoped answer incomplete: session start merges env as
// passthrough < workspace < provider < agent and injects the selected
// upstream's serving env LAST, so the layers AFTER the provider — agent.env
// and upstreams — win over the provider's own entry. An agent affected by one
// authenticates with a different variable than the provider names.
//
// The auth_token binding here is INHERITED, which is the only shape where
// [workspace.env] is an override: with no provider entry for the key, the
// workspace entry survives the merge. TestCredentialOverridesIgnoresWorkspace-
// EnvForDeclaredBinding pins the other half.
func TestCredentialOverridesFindsAgentScopedLayers(t *testing.T) {
	bindings := []credentialBinding{
		declaredCredential("api_key", "ANTHROPIC_API_KEY", "ACME_KEY"),
		inheritedCredential("auth_token", "ANTHROPIC_AUTH_TOKEN"),
	}
	cfg := &config.City{
		Workspace: config.Workspace{
			Provider: "zai",
			Env:      map[string]string{"ANTHROPIC_AUTH_TOKEN": "${WS_KEY}"},
		},
		Upstreams: map[string]config.UpstreamSpec{
			"alt": {APIKey: "${ZAI_KEY}"},
		},
		Agents: []config.Agent{
			{Name: "picks-upstream", Provider: "zai", Upstream: "alt"},
			{Name: "overrides-env", Provider: "zai", Env: map[string]string{"ANTHROPIC_API_KEY": "${AGENT_KEY}"}},
			{Name: "other-provider", Provider: "claude", Upstream: "alt"},
		},
	}

	got := credentialOverrides(cfg, "zai", bindings)

	byLayer := map[string]string{}
	for _, o := range got {
		byLayer[o.Layer] = o.Detail
	}
	for _, want := range []string{"workspace.env", "agent.env", "upstreams"} {
		if _, ok := byLayer[want]; !ok {
			t.Errorf("override layer %q not reported; got %+v", want, got)
		}
	}
	if detail := byLayer["upstreams"]; !strings.Contains(detail, "picks-upstream") {
		t.Errorf("upstream override detail = %q; want it to name the agent", detail)
	}
	for _, o := range got {
		if strings.Contains(o.Detail, "other-provider") {
			t.Errorf("reported an override for an agent on a different provider: %+v", o)
		}
	}
}

// TestCredentialOverridesIgnoresWorkspaceEnvForDeclaredBinding is the guard
// for the inversion this command must never ship: reporting a layer that
// LOSES the merge as one that wins.
//
// [workspace.env] merges BEFORE the provider (template_resolve.go:469), so
// when the provider declares the credential key itself the provider's entry
// overwrites the workspace one and the workspace entry changes nothing.
// Reporting it would tell the operator the variable this command just resolved
// is "NOT the one in use" and send them to rotate a dead entry — the precise
// failure the command exists to prevent.
func TestCredentialOverridesIgnoresWorkspaceEnvForDeclaredBinding(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{
			Provider: "zai",
			Env:      map[string]string{"ANTHROPIC_API_KEY": "${WS_KEY}"},
		},
		Agents: []config.Agent{{Name: "plain", Provider: "zai"}},
	}
	bindings := []credentialBinding{declaredCredential("api_key", "ANTHROPIC_API_KEY", "ACME_KEY")}

	for _, o := range credentialOverrides(cfg, "zai", bindings) {
		if o.Layer == "workspace.env" {
			t.Errorf("reported [workspace.env] as overriding a credential the provider DECLARES (%+v). "+
				"The provider layer merges after workspace and wins, so the workspace entry is dead; "+
				"naming it points the operator at a variable whose value never reaches the harness.", o)
		}
	}
}

// TestCredentialOverridesIgnoresWorkspaceEnvWithNoAgentOnTheProvider: the
// section names the agents for which the resolved credential is not the one in
// use. With no agent on this provider there are none, and a bare
// "[workspace.env]" row implies a divergence that no session can experience.
func TestCredentialOverridesIgnoresWorkspaceEnvWithNoAgentOnTheProvider(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{
			Provider: "claude",
			Env:      map[string]string{"ANTHROPIC_API_KEY": "${WS_KEY}"},
		},
		Agents: []config.Agent{{Name: "elsewhere", Provider: "claude"}},
	}
	bindings := []credentialBinding{inheritedCredential("api_key", "ANTHROPIC_API_KEY")}

	if got := credentialOverrides(cfg, "zai", bindings); len(got) != 0 {
		t.Errorf("credentialOverrides = %+v; want none — no agent resolves to \"zai\", so no session reads this provider's credential at all", got)
	}
}

// TestCredentialOverridesQuietWhenNothingOverrides: the section must not
// appear for the ordinary single-provider city, or it becomes noise the
// operator learns to skip.
func TestCredentialOverridesQuietWhenNothingOverrides(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Provider: "zai"},
		Agents:    []config.Agent{{Name: "plain", Provider: "zai"}},
	}
	bindings := []credentialBinding{declaredCredential("api_key", "ANTHROPIC_API_KEY", "ACME_KEY")}
	if got := credentialOverrides(cfg, "zai", bindings); len(got) != 0 {
		t.Errorf("credentialOverrides = %+v; want none", got)
	}
}
