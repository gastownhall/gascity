package api

import (
	"github.com/gastownhall/gascity/internal/config"
)

// AgentProviderOptions projects configured settings for a new agent session.
// These defaults are declarations, not observations of a running provider's
// actual model, effort, profile or user configuration. Existing sessions expose
// their target-specific delivery/steer capabilities on the session response.
type AgentProviderOptions struct {
	Resolution             string              `json:"resolution" enum:"resolved,unknown" doc:"Whether GC can project configured session-start defaults. Never an observation of the running provider."`
	Reason                 string              `json:"reason,omitempty" doc:"Stable reason when defaults are unknown: custom_start_command, custom_arguments, unhonored_defaults or resolution_failed."`
	Scope                  string              `json:"scope" enum:"session_start" doc:"Options apply to creation of a new session, not one turn of an existing conversation."`
	OptionsSchema          []providerOptionDTO `json:"options_schema,omitempty"`
	ConfiguredDefaults     map[string]string   `json:"configured_defaults,omitempty" doc:"Resolved declarative defaults for a new session. Missing values defer to provider-owned configuration; do not infer a model or effort from curated choices."`
	SupportsPerTurnOptions bool                `json:"supports_per_turn_options" doc:"Whether GC accepts model/effort overrides atomically for one turn. False in this contract; session-start options must not be presented as per-turn controls."`
}

func providerOptionsUnknown(reason string) *AgentProviderOptions {
	return &AgentProviderOptions{Resolution: "unknown", Reason: reason, Scope: "session_start"}
}

// providerProjectionLookPath resolves declarations without probing binaries or
// claiming launch readiness. Existing available/readiness projections own that.
func providerProjectionLookPath(command string) (string, error) { return command, nil }

func publicOptionsSchema(schema []config.ProviderOption) []providerOptionDTO {
	if len(schema) == 0 {
		return nil
	}
	out := make([]providerOptionDTO, len(schema))
	for i, opt := range schema {
		choices := make([]optionChoiceDTO, len(opt.Choices))
		for j, c := range opt.Choices {
			choices[j] = optionChoiceDTO{Value: c.Value, Label: c.Label}
		}
		out[i] = providerOptionDTO{
			Key: opt.Key, Label: opt.Label, Type: opt.Type, Default: opt.Default, Choices: choices,
			AcceptsCustomValues: config.IsOpenOption(opt),
		}
	}
	return out
}

func resolvedProviderOptions(resolved *config.ResolvedProvider) *AgentProviderOptions {
	if len(resolved.UnhonoredOptionPins()) != 0 {
		return providerOptionsUnknown("unhonored_defaults")
	}
	out := &AgentProviderOptions{Resolution: "resolved", Scope: "session_start", OptionsSchema: publicOptionsSchema(resolved.OptionsSchema)}
	if len(resolved.EffectiveDefaults) != 0 {
		out.ConfiguredDefaults = make(map[string]string, len(resolved.EffectiveDefaults))
		for key, value := range resolved.EffectiveDefaults {
			out.ConfiguredDefaults[key] = value
		}
	}
	return out
}

func agentProviderOptions(agent config.Agent, cfg *config.City) *AgentProviderOptions {
	if cfg == nil {
		return providerOptionsUnknown("resolution_failed")
	}
	if agent.StartCommand != "" || cfg.Workspace.StartCommand != "" {
		return providerOptionsUnknown("custom_start_command")
	}
	resolved, err := config.ResolveProvider(&agent, &cfg.Workspace, cfg.Providers, providerProjectionLookPath)
	if err != nil {
		return providerOptionsUnknown("resolution_failed")
	}
	// Recognized schema-managed args have already become declarative defaults.
	// Remaining authored args can select profiles/configuration outside GC's
	// schema, so the API must not guess which defaults that command will honor.
	if len(agent.Args) != 0 && len(resolved.Args) != 0 {
		return providerOptionsUnknown("custom_arguments")
	}
	return resolvedProviderOptions(resolved)
}

func providerPublicOptions(name string, providers map[string]config.ProviderSpec) *AgentProviderOptions {
	catalog := providers
	if _, declared := catalog[name]; !declared {
		// /providers/public includes available builtin definitions even when
		// they are absent from the composed city's explicit provider catalog.
		// Resolve that definition without changing the city's catalog.
		catalog = make(map[string]config.ProviderSpec, len(providers)+1)
		for key, spec := range providers {
			catalog[key] = spec
		}
		catalog[name] = config.BuiltinProviderAlias(name)
	}
	resolved, err := config.ResolveProvider(&config.Agent{Provider: name}, nil, catalog, providerProjectionLookPath)
	if err != nil {
		return providerOptionsUnknown("resolution_failed")
	}
	return resolvedProviderOptions(resolved)
}
