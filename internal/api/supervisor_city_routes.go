package api

import (
	"github.com/danielgtaylor/huma/v2"
)

// registerCityRoutes registers per-city Huma operations at their
// user-facing scoped paths ("/v0/city/{cityName}/..."). Called from
// NewSupervisorMux after registerSupervisorRoutes.
//
// Each registered route wraps a per-city handler method through
// bindCity, which resolves the target city's Server at request time.
// The input types all embed CityScope so the spec naturally describes
// {cityName} as a path parameter.
//
// As handler groups migrate off per-city Server.registerRoutes and onto
// this function, specific Huma routes take precedence over the
// transitional legacy /v0/city/ prefix forwarder via Go 1.22+ mux
// specificity rules.
func (sm *SupervisorMux) registerCityRoutes() {
	// Status + Health
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/status",
		bindCity(sm, (*Server).humaHandleStatus))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/health",
		bindCity(sm, (*Server).humaHandleHealth))

	// City detail
	huma.Get(sm.humaAPI, "/v0/city/{cityName}",
		bindCity(sm, (*Server).humaHandleCityGet))
	huma.Patch(sm.humaAPI, "/v0/city/{cityName}",
		bindCity(sm, (*Server).humaHandleCityPatch))

	// Readiness (per-city)
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/readiness",
		bindCity(sm, (*Server).humaHandleReadiness))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/provider-readiness",
		bindCity(sm, (*Server).humaHandleProviderReadiness))

	// Config
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/config",
		bindCity(sm, (*Server).humaHandleConfigGet))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/config/explain",
		bindCity(sm, (*Server).humaHandleConfigExplain))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/config/validate",
		bindCity(sm, (*Server).humaHandleConfigValidate))
}
