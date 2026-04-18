package api

import (
	"net/http"
	"sort"

	"github.com/gastownhall/gascity/internal/config"
)

type providerResponse struct {
	Name         string            `json:"name"`
	DisplayName  string            `json:"display_name,omitempty"`
	Command      string            `json:"command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	PromptMode   string            `json:"prompt_mode,omitempty"`
	PromptFlag   string            `json:"prompt_flag,omitempty"`
	ReadyDelayMs int               `json:"ready_delay_ms,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Builtin      bool              `json:"builtin"`
	CityLevel    bool              `json:"city_level"`
}

// providerPublicResponse is the browser-safe DTO. No command, args, env, or flag details.
type providerPublicResponse struct {
	Name              string              `json:"name"`
	DisplayName       string              `json:"display_name,omitempty"`
	Builtin           bool                `json:"builtin"`
	CityLevel         bool                `json:"city_level"`
	OptionsSchema     []providerOptionDTO `json:"options_schema,omitempty"`
	EffectiveDefaults map[string]string   `json:"effective_defaults,omitempty"`
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
		Args:         spec.Args,
		PromptMode:   spec.PromptMode,
		PromptFlag:   spec.PromptFlag,
		ReadyDelayMs: spec.ReadyDelayMs,
		Env:          spec.Env,
		Builtin:      builtin,
		CityLevel:    cityLevel,
	}
}

// providerPublicFromMerged builds the public DTO from a MERGED provider spec.
// The spec must already be the result of mergeProviderOverBuiltin so it has
// the correct OptionsSchema and OptionDefaults (including inherited builtins).
func providerPublicFromMerged(name string, spec config.ProviderSpec, builtin, cityLevel bool) providerPublicResponse {
	resp := providerPublicResponse{
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

func (s *Server) handleProviderList(w http.ResponseWriter, r *http.Request) {
	cfg := s.state.Config()
	builtins := config.BuiltinProviders()
	builtinOrder := config.BuiltinProviderOrder()
	isPublic := r.URL.Query().Get("view") == "public"

	// Collect all providers: city-level overrides + builtins.
	seen := make(map[string]bool)

	if isPublic {
		var providers []providerPublicResponse
		// City-level providers first (sorted alphabetically).
		// Merge with builtins to inherit OptionsSchema, OptionDefaults, etc.
		var cityNames []string
		for name := range cfg.Providers {
			cityNames = append(cityNames, name)
		}
		sort.Strings(cityNames)
		for _, name := range cityNames {
			spec := cfg.Providers[name]
			_, isBuiltin := builtins[name]
			// Prefer the eager-resolution cache (built via chain walk
			// in BuildResolvedProviderCache). Fall back to legacy
			// name/command merge only when the cache has no entry —
			// this preserves compat for Phase A configs that don't
			// declare `base` yet.
			merged := spec
			if resolved, ok := config.ResolvedProviderCached(cfg, name); ok {
				merged = resolvedProviderToSpec(resolved, spec)
			} else if base, ok := builtins[name]; ok {
				merged = config.MergeProviderOverBuiltin(base, spec)
			} else if base, ok := builtins[spec.Command]; ok {
				merged = config.MergeProviderOverBuiltin(base, spec)
			}
			providers = append(providers, providerPublicFromMerged(name, merged, isBuiltin, true))
			seen[name] = true
		}
		// Builtins not overridden by city-level (in canonical order).
		for _, name := range builtinOrder {
			if seen[name] {
				continue
			}
			providers = append(providers, providerPublicFromMerged(name, builtins[name], true, false))
		}
		writeListJSON(w, s.latestIndex(), providers, len(providers))
		return
	}

	var providers []providerResponse
	// City-level providers first (sorted alphabetically).
	var cityNames []string
	for name := range cfg.Providers {
		cityNames = append(cityNames, name)
	}
	sort.Strings(cityNames)
	for _, name := range cityNames {
		spec := cfg.Providers[name]
		_, isBuiltin := builtins[name]
		providers = append(providers, providerFromSpec(name, spec, isBuiltin, true))
		seen[name] = true
	}

	// Builtins not overridden by city-level (in canonical order).
	for _, name := range builtinOrder {
		if seen[name] {
			continue
		}
		providers = append(providers, providerFromSpec(name, builtins[name], true, false))
	}

	writeListJSON(w, s.latestIndex(), providers, len(providers))
}

func (s *Server) handleProviderGet(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg := s.state.Config()
	builtins := config.BuiltinProviders()

	// Check city-level first. Prefer the resolved cache (chain-walked)
	// so inherited fields (PromptMode, ReadyDelayMs, PermissionModes, …)
	// are returned to callers. Fall back to the raw spec when cache
	// doesn't have an entry (Phase A legacy providers).
	if spec, ok := cfg.Providers[name]; ok {
		_, isBuiltin := builtins[name]
		effective := spec
		if resolved, ok := config.ResolvedProviderCached(cfg, name); ok {
			effective = resolvedProviderToSpec(resolved, spec)
		}
		writeIndexJSON(w, s.latestIndex(), providerFromSpec(name, effective, isBuiltin, true))
		return
	}

	// Check builtins.
	if spec, ok := builtins[name]; ok {
		writeIndexJSON(w, s.latestIndex(), providerFromSpec(name, spec, true, false))
		return
	}

	writeError(w, http.StatusNotFound, "not_found", "provider "+name+" not found")
}

// resolvedProviderToSpec folds a ResolvedProvider back into a
// ProviderSpec shape so the existing response DTOs (which accept
// ProviderSpec) can consume cache-derived data without taking a
// dependency on the full ResolvedProvider struct. Preserves fields
// from the original raw spec that ResolvedProvider doesn't carry
// (PathCheck in particular).
func resolvedProviderToSpec(r config.ResolvedProvider, fallback config.ProviderSpec) config.ProviderSpec {
	out := fallback
	// DisplayName is not carried on ResolvedProvider — retain the
	// fallback spec's DisplayName.
	out.Command = r.Command
	out.Args = append([]string(nil), r.Args...)
	out.PromptMode = r.PromptMode
	out.PromptFlag = r.PromptFlag
	out.ReadyDelayMs = r.ReadyDelayMs
	out.ReadyPromptPrefix = r.ReadyPromptPrefix
	out.ProcessNames = append([]string(nil), r.ProcessNames...)
	out.EmitsPermissionWarning = r.EmitsPermissionWarning
	out.SupportsACP = r.SupportsACP
	out.SupportsHooks = r.SupportsHooks
	out.InstructionsFile = r.InstructionsFile
	out.ResumeFlag = r.ResumeFlag
	out.ResumeStyle = r.ResumeStyle
	out.ResumeCommand = r.ResumeCommand
	out.SessionIDFlag = r.SessionIDFlag
	out.PrintArgs = append([]string(nil), r.PrintArgs...)
	out.TitleModel = r.TitleModel
	if r.PermissionModes != nil {
		out.PermissionModes = make(map[string]string, len(r.PermissionModes))
		for k, v := range r.PermissionModes {
			out.PermissionModes[k] = v
		}
	}
	if r.Env != nil {
		out.Env = make(map[string]string, len(r.Env))
		for k, v := range r.Env {
			out.Env[k] = v
		}
	}
	if r.OptionsSchema != nil {
		out.OptionsSchema = append([]config.ProviderOption(nil), r.OptionsSchema...)
	}
	return out
}
