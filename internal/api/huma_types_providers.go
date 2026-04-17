package api

// Per-domain Huma input/output types for the providers handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_providers.go.

// --- Provider types ---

// ProviderListInput is the Huma input for GET /v0/city/{cityName}/providers.
type ProviderListInput struct {
	CityScope
	View string `query:"view" required:"false" doc:"Response view: 'public' omits command/args/env details."`
}

// ProviderGetInput is the Huma input for GET /v0/city/{cityName}/provider/{name}.
type ProviderGetInput struct {
	CityScope
	Name string `path:"name" doc:"Provider name."`
}

// ProviderCreateInput is the Huma input for POST /v0/city/{cityName}/providers.
type ProviderCreateInput struct {
	CityScope
	Body struct {
		Name         string            `json:"name" doc:"Provider name." minLength:"1"`
		DisplayName  string            `json:"display_name,omitempty" doc:"Human-readable display name."`
		Command      string            `json:"command" doc:"Provider command binary." minLength:"1"`
		Args         []string          `json:"args,omitempty" doc:"Command arguments."`
		PromptMode   string            `json:"prompt_mode,omitempty" doc:"Prompt delivery mode."`
		PromptFlag   string            `json:"prompt_flag,omitempty" doc:"Flag for prompt delivery."`
		ReadyDelayMs int               `json:"ready_delay_ms,omitempty" doc:"Milliseconds to wait before probing readiness."`
		Env          map[string]string `json:"env,omitempty" doc:"Environment variables."`
	}
}

// ProviderUpdateInput is the Huma input for PATCH /v0/city/{cityName}/provider/{name}.
type ProviderUpdateInput struct {
	CityScope
	Name string `path:"name" doc:"Provider name."`
	Body struct {
		DisplayName  *string           `json:"display_name,omitempty" doc:"Human-readable display name."`
		Command      *string           `json:"command,omitempty" doc:"Provider command binary."`
		Args         []string          `json:"args,omitempty" doc:"Command arguments."`
		PromptMode   *string           `json:"prompt_mode,omitempty" doc:"Prompt delivery mode."`
		PromptFlag   *string           `json:"prompt_flag,omitempty" doc:"Flag for prompt delivery."`
		ReadyDelayMs *int              `json:"ready_delay_ms,omitempty" doc:"Milliseconds to wait before probing readiness."`
		Env          map[string]string `json:"env,omitempty" doc:"Environment variables."`
	}
}

// ProviderDeleteInput is the Huma input for DELETE /v0/city/{cityName}/provider/{name}.
type ProviderDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Provider name."`
}

