package api

// Per-domain Huma input/output types for the services handler
// group. Split out of the original huma_types.go; mirrors the layout
// of huma_handlers_services.go.

// --- Service types ---

// ServiceListInput is the Huma input for GET /v0/city/{cityName}/services.
type ServiceListInput struct {
	CityScope
}

// ServiceGetInput is the Huma input for GET /v0/city/{cityName}/service/{name}.
type ServiceGetInput struct {
	CityScope
	Name string `path:"name" doc:"Service name."`
}

// ServiceRestartInput is the Huma input for POST /v0/city/{cityName}/service/{name}/restart.
type ServiceRestartInput struct {
	CityScope
	Name string `path:"name" doc:"Service name."`
}

// ServiceRestartOutput is the Huma output for POST /v0/service/{name}/restart.
type ServiceRestartOutput struct {
	Body struct {
		Status  string `json:"status" doc:"Operation result." example:"ok"`
		Action  string `json:"action" doc:"Action performed." example:"restart"`
		Service string `json:"service" doc:"Service name."`
	}
}

