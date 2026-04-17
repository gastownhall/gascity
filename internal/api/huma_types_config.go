package api

// Per-domain Huma input/output types for the config handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_config.go.

// --- Config types ---

// ConfigGetInput is the Huma input for GET /v0/city/{cityName}/config.
type ConfigGetInput struct {
	CityScope
}

// ConfigExplainInput is the Huma input for GET /v0/city/{cityName}/config/explain.
type ConfigExplainInput struct {
	CityScope
}

// ConfigValidateInput is the Huma input for GET /v0/city/{cityName}/config/validate.
type ConfigValidateInput struct {
	CityScope
}

// ConfigValidateOutput is the response body for GET /v0/config/validate.
type ConfigValidateOutput struct {
	Body struct {
		Valid    bool     `json:"valid" doc:"Whether the configuration is valid."`
		Errors   []string `json:"errors" doc:"Validation errors."`
		Warnings []string `json:"warnings" doc:"Validation warnings."`
	}
}

