package api

import (
	"net/http"
	"sort"

	"github.com/gastownhall/gascity/internal/config"
)

type providerResponse struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	// Base reflects the authored base declaration. nil when no Base
	// was declared (or on a builtin lookup); *"" means explicit
	// standalone opt-out; otherwise a concrete name or prefixed form.
	// Marshaled as JSON null when nil so CRUD round-trips preserve
	// presence, matching the PATCH contract.
	Base               *string                 `json:"base,omitempty"`
	Command            string                  `json:"command,omitempty"`
	Args               []string                `json:"args,omitempty"`
	ArgsAppend         []string                `json:"args_append,omitempty"`
	PromptMode         string                  `json:"prompt_mode,omitempty"`
	PromptFlag         string                  `json:"prompt_flag,omitempty"`
	ReadyDelayMs       int                     `json:"ready_delay_ms,omitempty"`
	Env                map[string]string       `json:"env,omitempty"`
	OptionsSchemaMerge string                  `json:"options_schema_merge,omitempty"`
	OptionsSchema      []config.ProviderOption `json:"options_schema,omitempty"`
	Builtin            bool                    `json:"builtin"`
	CityLevel          bool                    `json:"city_level"`
}

// providerPublicResponse is the browser-safe DTO. No command, args, env, or flag details.
//
// Tri-state capability bools (SupportsHooks, SupportsACP,
// EmitsPermissionWarning) serialize as JSON null / true / false to match
// the config model: null = "inherit from base", true = enable, false =
// "explicit disable". Clients that previously treated absence as false
// should key off the resolved provider cache entry, which still reports
// the final post-resolution value.
type providerPublicResponse struct {
	Name                   string              `json:"name"`
	DisplayName            string              `json:"display_name,omitempty"`
	Builtin                bool                `json:"builtin"`
	CityLevel              bool                `json:"city_level"`
	SupportsHooks          *bool               `json:"supports_hooks"`
	SupportsACP            *bool               `json:"supports_acp"`
	EmitsPermissionWarning *bool               `json:"emits_permission_warning"`
	OptionsSchema          []providerOptionDTO `json:"options_schema,omitempty"`
	EffectiveDefaults      map[string]string   `json:"effective_defaults,omitempty"`
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
	resp := providerResponse{
		Name:               name,
		DisplayName:        spec.DisplayName,
		Command:            spec.Command,
		Args:               spec.Args,
		ArgsAppend:         spec.ArgsAppend,
		PromptMode:         spec.PromptMode,
		PromptFlag:         spec.PromptFlag,
		ReadyDelayMs:       spec.ReadyDelayMs,
		Env:                spec.Env,
		OptionsSchemaMerge: spec.OptionsSchemaMerge,
		OptionsSchema:      spec.OptionsSchema,
		Builtin:            builtin,
		CityLevel:          cityLevel,
	}
	if spec.Base != nil {
		// Copy the underlying string so mutations on the response
		// don't leak into shared config state.
		b := *spec.Base
		resp.Base = &b
	}
	return resp
}

// providerPublicFromMerged builds the public DTO from a MERGED provider spec.
// The spec must already be the result of mergeProviderOverBuiltin so it has
// the correct OptionsSchema and OptionDefaults (including inherited builtins).
func providerPublicFromMerged(name string, spec config.ProviderSpec, builtin, cityLevel bool) providerPublicResponse {
	resp := providerPublicResponse{
		Name:                   name,
		DisplayName:            spec.DisplayName,
		Builtin:                builtin,
		CityLevel:              cityLevel,
		SupportsHooks:          spec.SupportsHooks,
		SupportsACP:            spec.SupportsACP,
		EmitsPermissionWarning: spec.EmitsPermissionWarning,
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
	// ResolvedProvider carries the resolved bool value; fold back into
	// the *bool tri-state form on ProviderSpec (only non-zero values
	// propagate — nil means "inherit", matching the tri-state rule).
	if r.EmitsPermissionWarning {
		t := true
		out.EmitsPermissionWarning = &t
	}
	if r.SupportsACP {
		t := true
		out.SupportsACP = &t
	}
	if r.SupportsHooks {
		t := true
		out.SupportsHooks = &t
	}
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
