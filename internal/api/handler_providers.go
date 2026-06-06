package api

import (
	"github.com/gastownhall/gascity/internal/config"
)

// providerResponse is the admin DTO for a provider. The browser-safe DTO
// is ProviderPublicResponse in huma_types_providers.go, served by the
// /providers/public endpoint — fields that leak command-line details
// (Command, Args, PromptMode, PromptFlag, ReadyDelayMs, Env) live here
// only.
type providerResponse struct {
	Name         string            `json:"name"`
	DisplayName  string            `json:"display_name,omitempty"`
	Command      string            `json:"command,omitempty"`
	ACPCommand   string            `json:"acp_command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	ACPArgs      *[]string         `json:"acp_args,omitempty"`
	PromptMode   string            `json:"prompt_mode,omitempty"`
	PromptFlag   string            `json:"prompt_flag,omitempty"`
	ReadyDelayMs int               `json:"ready_delay_ms,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Builtin      bool              `json:"builtin"`
	CityLevel    bool              `json:"city_level"`
}

type providerOptionDTO struct {
	Key     string            `json:"key"`
	Label   string            `json:"label"`
	Type    string            `json:"type"`
	Default string            `json:"default"`
	Choices []optionChoiceDTO `json:"choices"`
}

type optionChoiceDTO struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func providerFromSpec(name string, spec config.ProviderSpec, builtin, cityLevel bool) providerResponse {
	return providerResponse{
		Name:         name,
		DisplayName:  spec.DisplayName,
		Command:      spec.Command,
		ACPCommand:   spec.ACPCommand,
		Args:         spec.Args,
		ACPArgs:      optionalStringSlice(spec.ACPArgs),
		PromptMode:   spec.PromptMode,
		PromptFlag:   spec.PromptFlag,
		ReadyDelayMs: spec.ReadyDelayMs,
		Env:          spec.Env,
		Builtin:      builtin,
		CityLevel:    cityLevel,
	}
}

func optionalStringSlice(values []string) *[]string {
	if values == nil {
		return nil
	}
	cloned := make([]string, len(values))
	copy(cloned, values)
	return &cloned
}

// toProviderPublicResponse builds the browser-safe DTO from a MERGED
// provider spec. The spec must already be the result of
// MergeProviderOverBuiltin so it carries the correct OptionsSchema and
// OptionDefaults (including inherited builtins).
func toProviderPublicResponse(name string, spec config.ProviderSpec, builtin, cityLevel bool) ProviderPublicResponse {
	resp := ProviderPublicResponse{
		Name:        name,
		DisplayName: spec.DisplayName,
		Builtin:     builtin,
		CityLevel:   cityLevel,
	}
	if len(spec.OptionsSchema) > 0 {
		resp.OptionsSchema = make([]providerOptionDTO, len(spec.OptionsSchema))
		for i, opt := range spec.OptionsSchema {
			choices := make([]optionChoiceDTO, len(opt.Choices))
			for j, c := range opt.Choices {
				choices[j] = optionChoiceDTO{Value: c.Value, Label: c.Label}
			}
			resp.OptionsSchema[i] = providerOptionDTO{
				Key:     opt.Key,
				Label:   opt.Label,
				Type:    opt.Type,
				Default: opt.Default,
				Choices: choices,
			}
		}
		resp.EffectiveDefaults = config.ComputeEffectiveDefaults(spec.OptionsSchema, spec.OptionDefaults, nil)
	}
	return resp
}

// isBuiltinProvider checks if a name is a known builtin provider.
func isBuiltinProvider(name string) bool {
	_, ok := config.BuiltinProviders()[name]
	return ok
}
