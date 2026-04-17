package api

import (
	"net/http"

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

	// Agents stay on per-city Server.registerRoutes until SSE streams
	// can migrate — the {name...} catch-all would otherwise shadow the
	// SSE stream paths via the legacyCityForwarder.

	// Providers
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/providers",
		bindCity(sm, (*Server).humaHandleProviderList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/provider/{name}",
		bindCity(sm, (*Server).humaHandleProviderGet))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "create-provider",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/providers",
		Summary:       "Create a provider",
		DefaultStatus: http.StatusCreated,
	}, bindCity(sm, (*Server).humaHandleProviderCreate))
	huma.Patch(sm.humaAPI, "/v0/city/{cityName}/provider/{name}",
		bindCity(sm, (*Server).humaHandleProviderUpdate))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/provider/{name}",
		bindCity(sm, (*Server).humaHandleProviderDelete))

	// Rigs
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/rigs",
		bindCity(sm, (*Server).humaHandleRigList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/rig/{name}",
		bindCity(sm, (*Server).humaHandleRigGet))
	huma.Register(sm.humaAPI, huma.Operation{
		OperationID:   "create-rig",
		Method:        http.MethodPost,
		Path:          "/v0/city/{cityName}/rigs",
		Summary:       "Create a rig",
		DefaultStatus: http.StatusCreated,
	}, bindCity(sm, (*Server).humaHandleRigCreate))
	huma.Patch(sm.humaAPI, "/v0/city/{cityName}/rig/{name}",
		bindCity(sm, (*Server).humaHandleRigUpdate))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/rig/{name}",
		bindCity(sm, (*Server).humaHandleRigDelete))
	huma.Post(sm.humaAPI, "/v0/city/{cityName}/rig/{name}/{action}",
		bindCity(sm, (*Server).humaHandleRigAction))

	// Patches — agent
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/agents",
		bindCity(sm, (*Server).humaHandleAgentPatchList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/agent/{name...}",
		bindCity(sm, (*Server).humaHandleAgentPatchGet))
	huma.Put(sm.humaAPI, "/v0/city/{cityName}/patches/agents",
		bindCity(sm, (*Server).humaHandleAgentPatchSet))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/patches/agent/{name...}",
		bindCity(sm, (*Server).humaHandleAgentPatchDelete))
	// Patches — rig
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/rigs",
		bindCity(sm, (*Server).humaHandleRigPatchList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/rig/{name}",
		bindCity(sm, (*Server).humaHandleRigPatchGet))
	huma.Put(sm.humaAPI, "/v0/city/{cityName}/patches/rigs",
		bindCity(sm, (*Server).humaHandleRigPatchSet))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/patches/rig/{name}",
		bindCity(sm, (*Server).humaHandleRigPatchDelete))
	// Patches — provider
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/providers",
		bindCity(sm, (*Server).humaHandleProviderPatchList))
	huma.Get(sm.humaAPI, "/v0/city/{cityName}/patches/provider/{name}",
		bindCity(sm, (*Server).humaHandleProviderPatchGet))
	huma.Put(sm.humaAPI, "/v0/city/{cityName}/patches/providers",
		bindCity(sm, (*Server).humaHandleProviderPatchSet))
	huma.Delete(sm.humaAPI, "/v0/city/{cityName}/patches/provider/{name}",
		bindCity(sm, (*Server).humaHandleProviderPatchDelete))
}
