package api

// Per-domain Huma input/output types for the providers handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_providers.go.

// --- Provider types ---

// ProviderListInput is the Huma input for GET /v0/city/{cityName}/providers.
// Admin view; the browser-safe projection lives at
// GET /v0/city/{cityName}/providers/public.
type ProviderListInput struct {
	CityScope
}

// ProviderPublicListInput is the Huma input for GET
// /v0/city/{cityName}/providers/public.
type ProviderPublicListInput struct {
	CityScope
}

// ProviderPublicResponse is the browser-safe DTO for a single provider.
// Unlike ProviderResponse it exposes only fields safe for untrusted
// clients — option schemas and defaults — and omits command/args/env and
// prompt-delivery details.
type ProviderPublicResponse struct {
	Name              string              `json:"name"`
	DisplayName       string              `json:"display_name,omitempty"`
	Builtin           bool                `json:"builtin"`
	CityLevel         bool                `json:"city_level"`
	OptionsSchema     []providerOptionDTO `json:"options_schema,omitempty"`
	EffectiveDefaults map[string]string   `json:"effective_defaults,omitempty"`
}

// ProviderPublicListBody is the response body for GET
// /v0/city/{cityName}/providers/public.
type ProviderPublicListBody struct {
	Items      []ProviderPublicResponse `json:"items" doc:"The list of browser-safe provider summaries."`
	Total      int                      `json:"total" doc:"Total number of providers in the list."`
	NextCursor string                   `json:"next_cursor,omitempty" doc:"Cursor for the next page of results."`
}

// ProviderPublicListOutput is the response envelope for GET
// /v0/city/{cityName}/providers/public.
type ProviderPublicListOutput struct {
	Index uint64 `header:"X-GC-Index" doc:"Latest event sequence number."`
	Body  ProviderPublicListBody
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
		Name               string            `json:"name" doc:"Provider name." minLength:"1"`
		DisplayName        string            `json:"display_name,omitempty" doc:"Human-readable display name."`
		Base               *string           `json:"base,omitempty" doc:"Optional provider base for inheritance."`
		Command            string            `json:"command,omitempty" doc:"Provider command binary. Omit for base-only descendants."`
		Args               []string          `json:"args,omitempty" doc:"Command arguments."`
		ArgsAppend         []string          `json:"args_append,omitempty" doc:"Arguments appended after inherited/base args."`
		PromptMode         string            `json:"prompt_mode,omitempty" doc:"Prompt delivery mode."`
		PromptFlag         string            `json:"prompt_flag,omitempty" doc:"Flag for prompt delivery."`
		ReadyDelayMs       int               `json:"ready_delay_ms,omitempty" doc:"Milliseconds to wait before probing readiness."`
		Env                map[string]string `json:"env,omitempty" doc:"Environment variables."`
		OptionsSchemaMerge *string           `json:"options_schema_merge,omitempty" doc:"Options schema merge mode across inheritance chain."`
	}
}

// ProviderUpdateInput is the Huma input for PATCH /v0/city/{cityName}/provider/{name}.
type ProviderUpdateInput struct {
	CityScope
	Name string `path:"name" doc:"Provider name."`
	Body struct {
		DisplayName        *string           `json:"display_name,omitempty" doc:"Human-readable display name."`
		Base               *string           `json:"base,omitempty" doc:"Provider base for inheritance."`
		Command            *string           `json:"command,omitempty" doc:"Provider command binary."`
		Args               []string          `json:"args,omitempty" doc:"Command arguments."`
		ArgsAppend         []string          `json:"args_append,omitempty" doc:"Arguments appended after inherited/base args."`
		PromptMode         *string           `json:"prompt_mode,omitempty" doc:"Prompt delivery mode."`
		PromptFlag         *string           `json:"prompt_flag,omitempty" doc:"Flag for prompt delivery."`
		ReadyDelayMs       *int              `json:"ready_delay_ms,omitempty" doc:"Milliseconds to wait before probing readiness."`
		Env                map[string]string `json:"env,omitempty" doc:"Environment variables."`
		OptionsSchemaMerge *string           `json:"options_schema_merge,omitempty" doc:"Options schema merge mode across inheritance chain."`
	}
}

// ProviderDeleteInput is the Huma input for DELETE /v0/city/{cityName}/provider/{name}.
type ProviderDeleteInput struct {
	CityScope
	Name string `path:"name" doc:"Provider name."`
}
