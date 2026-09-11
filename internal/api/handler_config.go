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
// available (via RawConfigProvider), it uses the same two-phase detection the
// mutation gate uses (configedit.AgentOrigin), so the result agrees with the
// ErrPackDerived/409 decision. When raw is genuinely unavailable, it falls
// back to positive-only heuristics that can confirm pack origin but never
// emit a confident "inline" for a pack-derived agent:
//   - a non-empty BindingName is definitive proof of import expansion, and
//   - a [[patches.agent]] override targeting the agent implies a pack origin.
//
// In production raw is reliably cached (controllerState.RawConfig), so the
// fallback is a safety net, not the basis for routing decisions.
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
	// Fallback (raw unavailable): positive-only pack-derived signals.
	// An import-expanded agent always carries its binding name.
	if a.BindingName != "" {
		return "pack-derived"
	}
	for _, p := range expanded.Patches.Agents {
		if p.Dir == a.Dir && p.Name == a.Name {
			return "pack-derived"
		}
	}
	return "inline"
}

// agentPackProvenance reports whether an agent is pack-derived and, if so, the
// import binding name ([imports.<name>] key) it came from. It reuses the same
// origin detection that backs config-explain and ErrPackDerived: an agent is
// pack-derived exactly when a direct mutation would be rejected (it is present
// in the expanded config but absent from the raw config). The binding name is
// read straight from the agent's BindingName, the field set during V2 import
// expansion. Returns ("", false) for city-native agents.
func agentPackProvenance(a config.Agent, raw, expanded *config.City) (pack string, derived bool) {
	if agentOrigin(a, raw, expanded) != "pack-derived" {
		return "", false
	}
	return a.BindingName, true
}
