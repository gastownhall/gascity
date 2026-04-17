package api

// Per-domain Huma input/output types for the patches handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_patches.go.

// --- Patch types ---

// AgentPatchListInput is the Huma input for GET /v0/city/{cityName}/patches/agents.
type AgentPatchListInput struct {
	CityScope
}

// AgentPatchGetInput is the Huma input for GET /v0/city/{cityName}/patches/agent/{name}.
type AgentPatchGetInput struct {
	CityScope
	Name string `path:"name" doc:"Agent patch qualified name."`
}

// AgentPatchSetInput is the Huma input for PUT /v0/city/{cityName}/patches/agents.
type AgentPatchSetInput struct {
	CityScope
	Body struct {
		Dir       string            `json:"dir,omitempty" doc:"Agent directory scope."`
		Name      string            `json:"name,omitempty" doc:"Agent name."`
		WorkDir   *string           `json:"work_dir,omitempty" doc:"Override session working directory."`
		Scope     *string           `json:"scope,omitempty" doc:"Override agent scope."`
		Suspended *bool             `json:"suspended,omitempty" doc:"Override suspended state."`
		Env       map[string]string `json:"env,omitempty" doc:"Override environment variables."`
	}
}

// AgentPatchDeleteInput is the Huma input for DELETE /v0/city/{cityName}/patches/agent/{name}.
type AgentPatchDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Agent patch qualified name."`
}

// RigPatchListInput is the Huma input for GET /v0/city/{cityName}/patches/rigs.
type RigPatchListInput struct {
	CityScope
}

// RigPatchGetInput is the Huma input for GET /v0/city/{cityName}/patches/rig/{name}.
type RigPatchGetInput struct {
	CityScope
	Name string `path:"name" doc:"Rig patch name."`
}

// RigPatchSetInput is the Huma input for PUT /v0/city/{cityName}/patches/rigs.
type RigPatchSetInput struct {
	CityScope
	Body struct {
		Name      string `json:"name,omitempty" doc:"Rig name."`
		Path      *string `json:"path,omitempty" doc:"Override filesystem path."`
		Prefix    *string `json:"prefix,omitempty" doc:"Override bead ID prefix."`
		Suspended *bool   `json:"suspended,omitempty" doc:"Override suspended state."`
	}
}

// RigPatchDeleteInput is the Huma input for DELETE /v0/city/{cityName}/patches/rig/{name}.
type RigPatchDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Rig patch name."`
}

// ProviderPatchListInput is the Huma input for GET /v0/city/{cityName}/patches/providers.
type ProviderPatchListInput struct {
	CityScope
}

// ProviderPatchGetInput is the Huma input for GET /v0/city/{cityName}/patches/provider/{name}.
type ProviderPatchGetInput struct {
	CityScope
	Name string `path:"name" doc:"Provider patch name."`
}

// ProviderPatchSetInput is the Huma input for PUT /v0/city/{cityName}/patches/providers.
type ProviderPatchSetInput struct {
	CityScope
	Body struct {
		Name         string            `json:"name,omitempty" doc:"Provider name."`
		Command      *string           `json:"command,omitempty" doc:"Override command binary."`
		Args         []string          `json:"args,omitempty" doc:"Override command arguments."`
		PromptMode   *string           `json:"prompt_mode,omitempty" doc:"Override prompt delivery mode."`
		PromptFlag   *string           `json:"prompt_flag,omitempty" doc:"Override prompt flag."`
		ReadyDelayMs *int              `json:"ready_delay_ms,omitempty" doc:"Override ready delay in milliseconds."`
		Env          map[string]string `json:"env,omitempty" doc:"Override environment variables."`
	}
}

// ProviderPatchDeleteInput is the Huma input for DELETE /v0/city/{cityName}/patches/provider/{name}.
type ProviderPatchDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Provider patch name."`
}

// --- Patch response types ---

// PatchOKResponse is a success response for patch set operations.
type PatchOKResponse struct {
	Body struct {
		Status        string `json:"status" doc:"Operation result." example:"ok"`
		AgentPatch    string `json:"agent_patch,omitempty" doc:"Agent patch qualified name."`
		RigPatch      string `json:"rig_patch,omitempty" doc:"Rig patch name."`
		ProviderPatch string `json:"provider_patch,omitempty" doc:"Provider patch name."`
	}
}

// PatchDeletedResponse is a success response for patch delete operations.
type PatchDeletedResponse struct {
	Body struct {
		Status        string `json:"status" doc:"Operation result." example:"deleted"`
		AgentPatch    string `json:"agent_patch,omitempty" doc:"Agent patch qualified name."`
		RigPatch      string `json:"rig_patch,omitempty" doc:"Rig patch name."`
		ProviderPatch string `json:"provider_patch,omitempty" doc:"Provider patch name."`
	}
}

// StatusBody is the response body for GET /v0/status.
type StatusBody struct {
	Name       string            `json:"name" doc:"City name."`
	Path       string            `json:"path" doc:"City directory path."`
	Version    string            `json:"version,omitempty" doc:"Server version."`
	UptimeSec  int               `json:"uptime_sec" doc:"Server uptime in seconds."`
	Suspended  bool              `json:"suspended" doc:"Whether the city is suspended."`
	AgentCount int               `json:"agent_count" doc:"Total agent count (deprecated, use agents.total)."`
	RigCount   int               `json:"rig_count" doc:"Total rig count (deprecated, use rigs.total)."`
	Running    int               `json:"running" doc:"Number of running agent processes."`
	Agents     StatusAgentCounts `json:"agents" doc:"Agent state counts."`
	Rigs       StatusRigCounts   `json:"rigs" doc:"Rig state counts."`
	Work       StatusWorkCounts  `json:"work" doc:"Work item counts."`
	Mail       StatusMailCounts  `json:"mail" doc:"Mail counts."`
}

// Session types moved to huma_types_sessions.go.

