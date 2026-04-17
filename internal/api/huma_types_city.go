package api

// Per-domain Huma input/output types for the city handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_city.go.

// --- City types ---

// CityGetInput is the Huma input for GET /v0/city/{cityName}.
type CityGetInput struct {
	CityScope
}

// CityPatchInput is the Huma input for PATCH /v0/city/{cityName}.
type CityPatchInput struct {
	CityScope
	Body struct {
		Suspended *bool `json:"suspended,omitempty" doc:"Whether the city is suspended."`
	}
}

// ProviderReadinessInput is the Huma input for GET /v0/city/{cityName}/provider-readiness.
type ProviderReadinessInput struct {
	CityScope
	Providers string `query:"providers" required:"false" doc:"Comma-separated provider names to check (default: claude,codex,gemini)."`
	Fresh     string `query:"fresh" required:"false" doc:"Force fresh probe (0 or 1)."`
}

// ProviderReadinessOutput is the response body for GET /v0/provider-readiness.
type ProviderReadinessOutput struct {
	Body providerReadinessResponse
}

// ReadinessInput is the Huma input for GET /v0/city/{cityName}/readiness.
type ReadinessInput struct {
	CityScope
	Items string `query:"items" required:"false" doc:"Comma-separated readiness items to check (default: claude,codex,gemini,github_cli)."`
	Fresh string `query:"fresh" required:"false" doc:"Force fresh probe (0 or 1)."`
}

// ReadinessOutput is the response body for GET /v0/readiness.
type ReadinessOutput struct {
	Body readinessResponse
}

