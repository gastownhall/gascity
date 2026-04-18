package api

import (
	"net/http"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// configResponse is the JSON representation of the city configuration.
// It provides a structured view of the expanded (post-pack, post-patch)
// configuration state.
type configResponse struct {
	Workspace workspaceResponse           `json:"workspace"`
	Agents    []configAgentResponse       `json:"agents"`
	Rigs      []configRigResponse         `json:"rigs"`
	Providers map[string]providerSpecJSON `json:"providers,omitempty"`
	Patches   *configPatchesResponse      `json:"patches,omitempty"`
}

type workspaceResponse struct {
	Name            string `json:"name"`
	Provider        string `json:"provider,omitempty"`
	Suspended       bool   `json:"suspended"`
	SessionTemplate string `json:"session_template,omitempty"`
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
	Args         []string          `json:"args,omitempty"`
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

func (s *Server) handleConfigGet(w http.ResponseWriter, _ *http.Request) {
	cfg := s.state.Config()

	agents := make([]configAgentResponse, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		agents = append(agents, configAgentResponse{
			Name:      a.BindingQualifiedName(),
			Dir:       a.Dir,
			Provider:  a.Provider,
			IsPool:    isMultiSessionAgent(a),
			Scope:     a.Scope,
			Suspended: a.Suspended,
		})
	}

	rigs := make([]configRigResponse, 0, len(cfg.Rigs))
	for _, r := range cfg.Rigs {
		rigs = append(rigs, configRigResponse{
			Name:      r.Name,
			Path:      r.Path,
			Prefix:    r.Prefix,
			Suspended: r.Suspended,
		})
	}

	providers := make(map[string]providerSpecJSON, len(cfg.Providers))
	for name, spec := range cfg.Providers {
		providers[name] = providerSpecJSON{
			DisplayName:  spec.DisplayName,
			Command:      spec.Command,
			Args:         spec.Args,
			PromptMode:   spec.PromptMode,
			PromptFlag:   spec.PromptFlag,
			ReadyDelayMs: spec.ReadyDelayMs,
			Env:          spec.Env,
		}
	}

	resp := configResponse{
		Workspace: workspaceResponse{
			Name:            cfg.Workspace.Name,
			Provider:        cfg.Workspace.Provider,
			Suspended:       cfg.Workspace.Suspended,
			SessionTemplate: cfg.Workspace.SessionTemplate,
		},
		Agents:    agents,
		Rigs:      rigs,
		Providers: providers,
	}

	if !cfg.Patches.IsEmpty() {
		resp.Patches = &configPatchesResponse{
			AgentCount:    len(cfg.Patches.Agents),
			RigCount:      len(cfg.Patches.Rigs),
			ProviderCount: len(cfg.Patches.Providers),
		}
	}

	writeIndexJSON(w, s.latestIndex(), resp)
}

// hopIdentityDTO mirrors config.HopIdentity for JSON-safe serialization.
// It is a stable public projection: Kind is always "builtin" or "custom",
// Name is the canonical provider name without namespace prefix.
type hopIdentityDTO struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

// resolvedProviderDTO is the structured serialization of a resolved
// provider returned by /v0/config/explain. Chain is ordered leaf→root so
// clients can show "codex-max -> codex -> builtin:codex" reading index 0
// to len-1. OptionsSchema preserves `omit = true` entries as authored;
// callers filtering to a presentation view can drop them.
type resolvedProviderDTO struct {
	Name                   string                  `json:"name"`
	BuiltinAncestor        string                  `json:"builtin_ancestor,omitempty"`
	Chain                  []hopIdentityDTO        `json:"chain,omitempty"`
	Command                string                  `json:"command,omitempty"`
	Args                   []string                `json:"args,omitempty"`
	PromptMode             string                  `json:"prompt_mode,omitempty"`
	PromptFlag             string                  `json:"prompt_flag,omitempty"`
	ReadyDelayMs           int                     `json:"ready_delay_ms,omitempty"`
	ReadyPromptPrefix      string                  `json:"ready_prompt_prefix,omitempty"`
	ProcessNames           []string                `json:"process_names,omitempty"`
	Env                    map[string]string       `json:"env,omitempty"`
	SupportsACP            bool                    `json:"supports_acp,omitempty"`
	SupportsHooks          bool                    `json:"supports_hooks,omitempty"`
	EmitsPermissionWarning bool                    `json:"emits_permission_warning,omitempty"`
	InstructionsFile       string                  `json:"instructions_file,omitempty"`
	ResumeFlag             string                  `json:"resume_flag,omitempty"`
	ResumeStyle            string                  `json:"resume_style,omitempty"`
	ResumeCommand          string                  `json:"resume_command,omitempty"`
	SessionIDFlag          string                  `json:"session_id_flag,omitempty"`
	PermissionModes        map[string]string       `json:"permission_modes,omitempty"`
	OptionsSchema          []config.ProviderOption `json:"options_schema,omitempty"`
	PrintArgs              []string                `json:"print_args,omitempty"`
	TitleModel             string                  `json:"title_model,omitempty"`
	EffectiveDefaults      map[string]string       `json:"effective_defaults,omitempty"`
}

// resolvedProviderToDTO folds a ResolvedProvider into the JSON DTO. It
// copies slice/map fields so the caller's mutations do not affect the
// shared cache entry.
func resolvedProviderToDTO(r config.ResolvedProvider) resolvedProviderDTO {
	dto := resolvedProviderDTO{
		Name:                   r.Name,
		BuiltinAncestor:        r.BuiltinAncestor,
		Command:                r.Command,
		Args:                   append([]string(nil), r.Args...),
		PromptMode:             r.PromptMode,
		PromptFlag:             r.PromptFlag,
		ReadyDelayMs:           r.ReadyDelayMs,
		ReadyPromptPrefix:      r.ReadyPromptPrefix,
		ProcessNames:           append([]string(nil), r.ProcessNames...),
		SupportsACP:            r.SupportsACP,
		SupportsHooks:          r.SupportsHooks,
		EmitsPermissionWarning: r.EmitsPermissionWarning,
		InstructionsFile:       r.InstructionsFile,
		ResumeFlag:             r.ResumeFlag,
		ResumeStyle:            r.ResumeStyle,
		ResumeCommand:          r.ResumeCommand,
		SessionIDFlag:          r.SessionIDFlag,
		PrintArgs:              append([]string(nil), r.PrintArgs...),
		TitleModel:             r.TitleModel,
	}
	if len(r.Chain) > 0 {
		dto.Chain = make([]hopIdentityDTO, len(r.Chain))
		for i, h := range r.Chain {
			dto.Chain[i] = hopIdentityDTO{Kind: h.Kind, Name: h.Name}
		}
	}
	if len(r.Env) > 0 {
		dto.Env = make(map[string]string, len(r.Env))
		for k, v := range r.Env {
			dto.Env[k] = v
		}
	}
	if len(r.PermissionModes) > 0 {
		dto.PermissionModes = make(map[string]string, len(r.PermissionModes))
		for k, v := range r.PermissionModes {
			dto.PermissionModes[k] = v
		}
	}
	if len(r.EffectiveDefaults) > 0 {
		dto.EffectiveDefaults = make(map[string]string, len(r.EffectiveDefaults))
		for k, v := range r.EffectiveDefaults {
			dto.EffectiveDefaults[k] = v
		}
	}
	if len(r.OptionsSchema) > 0 {
		dto.OptionsSchema = append([]config.ProviderOption(nil), r.OptionsSchema...)
	}
	return dto
}

// builtinResolvedDTO synthesizes a resolvedProviderDTO for a builtin
// provider that has no city-level override and therefore no cache
// entry. The resulting DTO has an empty Chain and the builtin's own
// name as BuiltinAncestor so clients can detect "this is a raw
// builtin, not an inherited chain."
func builtinResolvedDTO(name string, spec config.ProviderSpec) resolvedProviderDTO {
	dto := resolvedProviderDTO{
		Name:              name,
		BuiltinAncestor:   name,
		Command:           spec.Command,
		Args:              append([]string(nil), spec.Args...),
		PromptMode:        spec.PromptMode,
		PromptFlag:        spec.PromptFlag,
		ReadyDelayMs:      spec.ReadyDelayMs,
		ReadyPromptPrefix: spec.ReadyPromptPrefix,
		ProcessNames:      append([]string(nil), spec.ProcessNames...),
		InstructionsFile:  spec.InstructionsFile,
		ResumeFlag:        spec.ResumeFlag,
		ResumeStyle:       spec.ResumeStyle,
		ResumeCommand:     spec.ResumeCommand,
		SessionIDFlag:     spec.SessionIDFlag,
		PrintArgs:         append([]string(nil), spec.PrintArgs...),
		TitleModel:        spec.TitleModel,
	}
	// Capability bools on ProviderSpec are tri-state *bool post-migration.
	// Treat absent (nil) as false — matching runtime behavior.
	if spec.SupportsACP != nil {
		dto.SupportsACP = *spec.SupportsACP
	}
	if spec.SupportsHooks != nil {
		dto.SupportsHooks = *spec.SupportsHooks
	}
	if spec.EmitsPermissionWarning != nil {
		dto.EmitsPermissionWarning = *spec.EmitsPermissionWarning
	}
	if len(spec.Env) > 0 {
		dto.Env = make(map[string]string, len(spec.Env))
		for k, v := range spec.Env {
			dto.Env[k] = v
		}
	}
	if len(spec.PermissionModes) > 0 {
		dto.PermissionModes = make(map[string]string, len(spec.PermissionModes))
		for k, v := range spec.PermissionModes {
			dto.PermissionModes[k] = v
		}
	}
	if len(spec.OptionsSchema) > 0 {
		dto.OptionsSchema = append([]config.ProviderOption(nil), spec.OptionsSchema...)
	}
	// Synthesize EffectiveDefaults from OptionDefaults so the public
	// DTO carries a non-empty defaults map for builtins too.
	if eff := config.ComputeEffectiveDefaults(spec.OptionsSchema, spec.OptionDefaults, nil); len(eff) > 0 {
		dto.EffectiveDefaults = eff
	}
	return dto
}

// resolveProviderForExplain looks up a provider by name, preferring the
// resolved-provider cache (chain-walked) so callers see the same
// post-inheritance view as runtime. Falls back to raw spec + builtin
// lookup when the cache has no entry (Phase A legacy configs).
// Returns (dto, true) for any known name; (_, false) for unknown.
func resolveProviderForExplain(cfg *config.City, name string) (resolvedProviderDTO, bool) {
	builtins := config.BuiltinProviders()
	if resolved, ok := config.ResolvedProviderCached(cfg, name); ok {
		return resolvedProviderToDTO(resolved), true
	}
	// City-level without cache entry (e.g., cache not built in tests):
	// synthesize from raw spec merged with its builtin if names match.
	if spec, ok := cfg.Providers[name]; ok {
		if base, isBuiltin := builtins[name]; isBuiltin {
			merged := config.MergeProviderOverBuiltin(base, spec)
			dto := builtinResolvedDTO(name, merged)
			dto.BuiltinAncestor = name
			return dto, true
		}
		return builtinResolvedDTO(name, spec), true
	}
	if spec, ok := builtins[name]; ok {
		return builtinResolvedDTO(name, spec), true
	}
	return resolvedProviderDTO{}, false
}

// handleConfigExplain returns the config with provenance annotations showing
// where each resource originates: raw config, pack-derived, or patched.
//
// Query parameters:
//   - provider=<name>: return a focused view of a single provider's
//     ResolvedProvider + Chain. Omits the agents/providers map.
//   - view=json: force the structured JSON view (default is already JSON,
//     so this is effectively a no-op today; reserved for future
//     text/tabular output modes).
//
// Accept header: text/plain could be supported later for a tabular view.
// For now, application/json is the only content type.
func (s *Server) handleConfigExplain(w http.ResponseWriter, r *http.Request) {
	cfg := s.state.Config()
	builtins := config.BuiltinProviders()

	// Focused per-provider view: `?provider=<name>`.
	if focus := strings.TrimSpace(r.URL.Query().Get("provider")); focus != "" {
		dto, ok := resolveProviderForExplain(cfg, focus)
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "provider "+focus+" not found")
			return
		}
		writeIndexJSON(w, s.latestIndex(), map[string]any{
			"provider": dto,
		})
		return
	}

	type annotatedAgent struct {
		configAgentResponse
		Origin           string               `json:"origin"` // "inline" or "pack-derived"
		ResolvedProvider *resolvedProviderDTO `json:"resolved_provider,omitempty"`
	}

	type annotatedProvider struct {
		resolvedProviderDTO
		Origin string `json:"origin"` // "builtin", "city", or "builtin+city"
	}

	// Use raw config for accurate provenance when available.
	var rawCfg *config.City
	if rcp, ok := s.state.(RawConfigProvider); ok {
		rawCfg = rcp.RawConfig()
	}

	agents := make([]annotatedAgent, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		origin := agentOrigin(a, rawCfg, cfg)
		entry := annotatedAgent{
			configAgentResponse: configAgentResponse{
				Name:      a.BindingQualifiedName(),
				Dir:       a.Dir,
				Provider:  a.Provider,
				IsPool:    isMultiSessionAgent(a),
				Scope:     a.Scope,
				Suspended: a.Suspended,
			},
			Origin: origin,
		}
		// Attach the resolved provider so explain output matches
		// runtime resolution for every agent — base-only descendants
		// included. Workspace fallback is applied when the agent has
		// no provider of its own.
		providerName := a.Provider
		if providerName == "" {
			providerName = cfg.Workspace.Provider
		}
		if providerName != "" {
			if dto, ok := resolveProviderForExplain(cfg, providerName); ok {
				entry.ResolvedProvider = &dto
			}
		}
		agents = append(agents, entry)
	}

	// Annotate providers with origin, routing through the resolved cache.
	provMap := make(map[string]annotatedProvider)
	for name := range cfg.Providers {
		origin := "city"
		if _, isBuiltin := builtins[name]; isBuiltin {
			origin = "builtin+city"
		}
		dto, _ := resolveProviderForExplain(cfg, name)
		provMap[name] = annotatedProvider{
			resolvedProviderDTO: dto,
			Origin:              origin,
		}
	}
	for name, spec := range builtins {
		if _, ok := provMap[name]; !ok {
			provMap[name] = annotatedProvider{
				resolvedProviderDTO: builtinResolvedDTO(name, spec),
				Origin:              "builtin",
			}
		}
	}

	writeIndexJSON(w, s.latestIndex(), map[string]any{
		"agents":    agents,
		"providers": provMap,
		"patches": map[string]int{
			"agents":    len(cfg.Patches.Agents),
			"rigs":      len(cfg.Patches.Rigs),
			"providers": len(cfg.Patches.Providers),
		},
	})
}

// handleConfigValidate checks the current config for validation errors
// and semantic warnings without writing anything.
func (s *Server) handleConfigValidate(w http.ResponseWriter, _ *http.Request) {
	cfg := s.state.Config()

	var errors []string

	if err := config.ValidateAgents(cfg.Agents); err != nil {
		errors = append(errors, err.Error())
	}
	if err := config.ValidateRigs(cfg.Rigs, config.EffectiveHQPrefix(cfg)); err != nil {
		errors = append(errors, err.Error())
	}
	if err := config.ValidateServices(cfg.Services); err != nil {
		errors = append(errors, err.Error())
	} else if err := workspacesvc.ValidateRuntimeSupport(cfg.Services); err != nil {
		errors = append(errors, err.Error())
	}

	warnings := config.ValidateSemantics(cfg, "city.toml")
	warnings = append(warnings, config.ValidateDurations(cfg, "city.toml")...)

	valid := len(errors) == 0
	writeJSON(w, http.StatusOK, map[string]any{
		"valid":    valid,
		"errors":   errors,
		"warnings": warnings,
	})
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
