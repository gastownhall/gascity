package api

import (
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
)

// configResponse is the JSON representation of the city configuration.
// It provides a structured view of the expanded (post-pack, post-patch)
// configuration state.
type configResponse struct {
	Workspace       workspaceResponse           `json:"workspace"`
	EffectiveAPIURL string                      `json:"effective_api_url,omitempty"`
	Agents          []configAgentResponse       `json:"agents"`
	Rigs            []configRigResponse         `json:"rigs"`
	Providers       map[string]providerSpecJSON `json:"providers,omitempty"`
	Patches         *configPatchesResponse      `json:"patches,omitempty"`
}

type workspaceResponse struct {
	Name            string `json:"name"`
	Prefix          string `json:"prefix,omitempty"`
	DeclaredName    string `json:"declared_name,omitempty"`
	DeclaredPrefix  string `json:"declared_prefix,omitempty"`
	Provider        string `json:"provider,omitempty"`
	Suspended       bool   `json:"suspended"`
	SessionTemplate string `json:"session_template,omitempty"`
	// MaxActiveSessions is the city-wide cap on total concurrent sessions,
	// mirrored from config.Workspace.MaxActiveSessions. The tri-state is
	// preserved: nil = unset (no city-level cap declared), -1 = unlimited,
	// any other value = the explicit cap. Agents and rigs inherit this when
	// they don't declare their own.
	MaxActiveSessions *int `json:"max_active_sessions,omitempty"`
}

type configAgentResponse struct {
	Name      string `json:"name"`
	Dir       string `json:"dir,omitempty"`
	Provider  string `json:"provider,omitempty"`
	IsPool    bool   `json:"is_pool,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Suspended bool   `json:"suspended"`
}

type configRigResponse struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	Prefix    string `json:"prefix,omitempty"`
	Suspended bool   `json:"suspended"`
}

type providerSpecJSON struct {
	DisplayName  string            `json:"display_name,omitempty"`
	Command      string            `json:"command,omitempty"`
	ACPCommand   string            `json:"acp_command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	ACPArgs      *[]string         `json:"acp_args,omitempty"`
	PromptMode   string            `json:"prompt_mode,omitempty"`
	PromptFlag   string            `json:"prompt_flag,omitempty"`
	ReadyDelayMs int               `json:"ready_delay_ms,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
}

type configPatchesResponse struct {
	AgentCount    int `json:"agent_count"`
	RigCount      int `json:"rig_count"`
	ProviderCount int `json:"provider_count"`
}

// providerSpecJSONFrom renders a config.ProviderSpec into its wire shape.
// Shared by the loaded-config and defaults-baseline handlers so the two
// surfaces stay in lock-step.
func providerSpecJSONFrom(spec config.ProviderSpec) providerSpecJSON {
	return providerSpecJSON{
		DisplayName:  spec.DisplayName,
		Command:      spec.Command,
		ACPCommand:   spec.ACPCommand,
		Args:         spec.Args,
		ACPArgs:      optionalStringSlice(spec.ACPArgs),
		PromptMode:   spec.PromptMode,
		PromptFlag:   spec.PromptFlag,
		ReadyDelayMs: spec.ReadyDelayMs,
		Env:          spec.Env,
	}
}

// agentOrigin determines the provenance of an agent. When raw config is
// available (via RawConfigProvider), it uses two-phase detection for
// accurate results. Otherwise falls back to the patch-presence heuristic.
func agentOrigin(a config.Agent, raw, expanded *config.City) string {
	if raw != nil {
		switch configedit.AgentOrigin(raw, expanded, a.QualifiedName()) {
		case configedit.OriginInline:
			return "inline"
		case configedit.OriginDerived:
			return "pack-derived"
		default:
			return "inline"
		}
	}
	// Fallback: heuristic based on patch presence.
	for _, p := range expanded.Patches.Agents {
		if p.Dir == a.Dir && p.Name == a.Name {
			return "pack-derived"
		}
	}
	return "inline"
}
