package api

import (
	"context"
	"strings"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/workspacesvc"
)

// humaHandleConfigGet is the Huma-typed handler for GET /v0/config.
func (s *Server) humaHandleConfigGet(_ context.Context, _ *ConfigGetInput) (*IndexOutput[configResponse], error) {
	cfg := s.state.Config()
	name := strings.TrimSpace(s.state.CityName())
	if name == "" {
		name = config.EffectiveCityName(cfg, "")
	}
	prefix := effectiveWorkspacePrefix(cfg, name)

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
			ACPCommand:   spec.ACPCommand,
			Args:         spec.Args,
			ACPArgs:      optionalStringSlice(spec.ACPArgs),
			PromptMode:   spec.PromptMode,
			PromptFlag:   spec.PromptFlag,
			ReadyDelayMs: spec.ReadyDelayMs,
			Env:          spec.Env,
		}
	}

	resp := configResponse{
		Workspace: workspaceResponse{
			Name:            name,
			Prefix:          prefix,
			DeclaredName:    strings.TrimSpace(cfg.Workspace.Name),
			DeclaredPrefix:  strings.TrimSpace(cfg.Workspace.Prefix),
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

	return &IndexOutput[configResponse]{
		Index: s.latestIndex(),
		Body:  resp,
	}, nil
}

func effectiveWorkspacePrefix(cfg *config.City, name string) string {
	if cfg == nil {
		return config.DeriveBeadsPrefix(name)
	}
	if prefix := strings.TrimSpace(cfg.ResolvedWorkspacePrefix); prefix != "" {
		return prefix
	}
	if prefix := strings.TrimSpace(cfg.Workspace.Prefix); prefix != "" {
		return prefix
	}
	return config.DeriveBeadsPrefix(name)
}

// humaHandleConfigExplain is the Huma-typed handler for GET /v0/config/explain.
func (s *Server) humaHandleConfigExplain(_ context.Context, _ *ConfigExplainInput) (*IndexOutput[configExplainResponse], error) {
	cfg := s.state.Config()
	builtins := config.BuiltinProviders()

	// Use raw config for accurate provenance when available.
	var rawCfg *config.City
	if rcp, ok := s.state.(RawConfigProvider); ok {
		rawCfg = rcp.RawConfig()
	}

	agents := make([]annotatedAgentResponse, 0, len(cfg.Agents))
	for _, a := range cfg.Agents {
		origin := agentOrigin(a, rawCfg, cfg)
		agents = append(agents, annotatedAgentResponse{
			Name:      a.BindingQualifiedName(),
			Dir:       a.Dir,
			Provider:  a.Provider,
			IsPool:    isMultiSessionAgent(a),
			Scope:     a.Scope,
			Suspended: a.Suspended,
			Origin:    origin,
		})
	}

	// Annotate providers with origin.
	provMap := make(map[string]annotatedProviderResponse)
	for name, spec := range cfg.Providers {
		origin := "city"
		if _, isBuiltin := builtins[name]; isBuiltin {
			origin = "builtin+city"
		}
		provMap[name] = annotatedProviderResponse{
			DisplayName:  spec.DisplayName,
			Command:      spec.Command,
			ACPCommand:   spec.ACPCommand,
			Args:         spec.Args,
			ACPArgs:      optionalStringSlice(spec.ACPArgs),
			PromptMode:   spec.PromptMode,
			PromptFlag:   spec.PromptFlag,
			ReadyDelayMs: spec.ReadyDelayMs,
			Env:          spec.Env,
			Origin:       origin,
		}
	}
	// Builtins not overridden.
	for name, spec := range builtins {
		if _, ok := provMap[name]; !ok {
			provMap[name] = annotatedProviderResponse{
				DisplayName:  spec.DisplayName,
				Command:      spec.Command,
				ACPCommand:   spec.ACPCommand,
				Args:         spec.Args,
				ACPArgs:      optionalStringSlice(spec.ACPArgs),
				PromptMode:   spec.PromptMode,
				PromptFlag:   spec.PromptFlag,
				ReadyDelayMs: spec.ReadyDelayMs,
				Env:          spec.Env,
				Origin:       "builtin",
			}
		}
	}

	resp := configExplainResponse{
		Agents:    agents,
		Providers: provMap,
		Patches: configExplainPatches{
			Agents:    len(cfg.Patches.Agents),
			Rigs:      len(cfg.Patches.Rigs),
			Providers: len(cfg.Patches.Providers),
		},
	}

	return &IndexOutput[configExplainResponse]{
		Index: s.latestIndex(),
		Body:  resp,
	}, nil
}

// humaHandleConfigValidate is the Huma-typed handler for GET /v0/config/validate.
func (s *Server) humaHandleConfigValidate(_ context.Context, _ *ConfigValidateInput) (*ConfigValidateOutput, error) {
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

	resp := &ConfigValidateOutput{}
	resp.Body.Valid = valid
	resp.Body.Errors = errors
	resp.Body.Warnings = warnings
	return resp, nil
}

// --- Response types used by config explain ---

// annotatedAgentResponse is a config agent with provenance annotation.
// Defined as a flat struct so the OpenAPI spec and the wire shape match
// exactly (no custom MarshalJSON needed).
type annotatedAgentResponse struct {
	Name      string `json:"name"`
	Dir       string `json:"dir,omitempty"`
	Provider  string `json:"provider,omitempty"`
	IsPool    bool   `json:"is_pool,omitempty"`
	Scope     string `json:"scope,omitempty"`
	Suspended bool   `json:"suspended"`
	Origin    string `json:"origin" doc:"Agent origin: inline or pack-derived."`
}

// annotatedProviderResponse is a provider spec with provenance annotation.
// Defined as a flat struct so the OpenAPI spec and the wire shape match
// exactly (no custom MarshalJSON needed).
type annotatedProviderResponse struct {
	DisplayName  string            `json:"display_name,omitempty"`
	Command      string            `json:"command,omitempty"`
	ACPCommand   string            `json:"acp_command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	ACPArgs      *[]string         `json:"acp_args,omitempty"`
	PromptMode   string            `json:"prompt_mode,omitempty"`
	PromptFlag   string            `json:"prompt_flag,omitempty"`
	ReadyDelayMs int               `json:"ready_delay_ms,omitempty"`
	Env          map[string]string `json:"env,omitempty"`
	Origin       string            `json:"origin" doc:"Provider origin: builtin, city, or builtin+city."`
}

// configExplainResponse is the full response for GET /v0/config/explain.
type configExplainResponse struct {
	Agents    []annotatedAgentResponse             `json:"agents"`
	Providers map[string]annotatedProviderResponse `json:"providers"`
	Patches   configExplainPatches                 `json:"patches"`
}

// configExplainPatches is the patch counts in the explain response.
type configExplainPatches struct {
	Agents    int `json:"agents"`
	Rigs      int `json:"rigs"`
	Providers int `json:"providers"`
}
