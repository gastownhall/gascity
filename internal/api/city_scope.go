package api

import (
	"context"
	"fmt"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/sse"
)

// CityScope is the path-parameter mixin embedded by every city-scoped
// Huma input type. It declares `{cityName}` as a required path segment
// so the OpenAPI spec describes the real URL shape.
//
// Register city-scoped operations via the cityGet/Post/Patch/Delete/
// Put/Register helpers below; they prepend the /v0/city/{cityName}
// prefix and wrap the handler with bindCity so the supervisor
// resolves the target per-city Server before calling through.
type CityScope struct {
	CityName string `path:"cityName" minLength:"1" pattern:"\\S" doc:"City name."`
}

// GetCityName returns the value of the cityName path parameter.
// Declared on a pointer receiver so types that embed CityScope by
// value satisfy the cityNamer interface via *T method promotion.
func (c *CityScope) GetCityName() string { return c.CityName }

// cityNamer is satisfied by every type that embeds CityScope.
// bindCity uses it to extract the target city name. The assertion
// in bindCity is a runtime check rather than a generic constraint
// because Go's type inference cannot bridge between huma's
// `func(context.Context, *I)` and a constrained `*I` parameter
// across nested generic calls. In practice every per-city input
// type embeds CityScope, so the assertion always succeeds — the
// runtime check is a tripwire for misuse, not a normal failure mode.
type cityNamer interface {
	GetCityName() string
}

// cityScopePrefix is the URL prefix every city-scoped operation
// registers under.
const cityScopePrefix = "/v0/city/{cityName}"

const cityNotFoundOrNotRunningDetailPrefix = "not_found: city not found or not running: "

// CityNotFoundOrNotRunningDetail returns the stable 404 detail used when a
// city-scoped route targets a city that is not currently running.
func CityNotFoundOrNotRunningDetail(name string) string {
	return cityNotFoundOrNotRunningDetailPrefix + name
}

// IsCityNotFoundOrNotRunningDetail reports whether detail is the stable 404
// payload used for city-scoped requests when the target city is not running.
func IsCityNotFoundOrNotRunningDetail(detail string) bool {
	return strings.HasPrefix(strings.TrimSpace(detail), cityNotFoundOrNotRunningDetailPrefix)
}

// bindCity wraps a per-city handler method expression as a Huma
// handler registered on the supervisor API. The returned function
// resolves the per-city Server for input.GetCityName() and delegates.
// Returns 404 Problem Details when the named city is not running.
func bindCity[I any, O any](
	sm *SupervisorMux,
	fn func(*Server, context.Context, *I) (*O, error),
) func(context.Context, *I) (*O, error) {
	return func(ctx context.Context, input *I) (*O, error) {
		named, ok := any(input).(cityNamer)
		if !ok {
			return nil, fmt.Errorf("internal: input %T does not embed CityScope", input)
		}
		name := named.GetCityName()
		srv := sm.resolveCityServer(name)
		if srv == nil {
			return nil, huma.Error404NotFound(CityNotFoundOrNotRunningDetail(name))
		}
		return fn(srv, ctx, input)
	}
}

// csrfHeaderName is the anti-CSRF header required on every mutation
// request. Any non-empty value satisfies the check; the header's
// presence is what matters, because cross-origin XHR from an attacker
// origin cannot set custom request headers without triggering a CORS
// preflight the API does not grant. See OWASP's "Use of Custom Request
// Headers" defense.
const csrfHeaderName = "X-GC-Request"

// csrfHeaderDescription is the shared description used for the header
// in generated OpenAPI specs so the spec and runtime enforcement agree.
const csrfHeaderDescription = "Anti-CSRF header required on mutation requests. Any non-empty value is accepted; the header's presence is what the server checks."

// addMutationCSRFParam is an operationHandler (see huma.Post et al.)
// that appends the X-GC-Request required header parameter to op.
// Mutation-verb registration helpers pass this handler so the spec
// describes the middleware's enforcement rather than advertising a
// false "no special headers needed" contract.
//
// The header is declared once per mutation operation (OpenAPI 3.1 has
// no mechanism for global per-verb parameters; see
// speakeasy.com/openapi/responses/headers). Idempotent so handlers
// whose input struct happens to declare the header explicitly are not
// double-registered.
func addMutationCSRFParam(op *huma.Operation) {
	for _, p := range op.Parameters {
		if p != nil && p.In == "header" && p.Name == csrfHeaderName {
			return
		}
	}
	minLen := 1
	op.Parameters = append(op.Parameters, &huma.Param{
		Name:        csrfHeaderName,
		In:          "header",
		Required:    true,
		Description: csrfHeaderDescription,
		Schema: &huma.Schema{
			Type:        "string",
			MinLength:   &minLen,
			Description: csrfHeaderDescription,
		},
	})
}

// cityGet registers a per-city GET op at /v0/city/{cityName}+tail.
// The tail starts with "/" (e.g. "/agents") or is "" for the
// city-detail base path.
func cityGet[I any, O any](sm *SupervisorMux, tail string,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	huma.Get(sm.humaAPI, cityScopePrefix+tail, bindCity(sm, fn))
}

// cityPost is the POST sibling of cityGet. Every city-scoped POST
// mutation flows through this helper, so declaring the X-GC-Request
// header param here covers every current and future mutation without
// per-input-struct boilerplate.
func cityPost[I any, O any](sm *SupervisorMux, tail string,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	huma.Post(sm.humaAPI, cityScopePrefix+tail, bindCity(sm, fn), addMutationCSRFParam)
}

// cityPut is the PUT sibling of cityGet. See cityPost for the CSRF
// header rationale.
func cityPut[I any, O any](sm *SupervisorMux, tail string,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	huma.Put(sm.humaAPI, cityScopePrefix+tail, bindCity(sm, fn), addMutationCSRFParam)
}

// cityPatch is the PATCH sibling of cityGet. See cityPost for the CSRF
// header rationale.
func cityPatch[I any, O any](sm *SupervisorMux, tail string,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	huma.Patch(sm.humaAPI, cityScopePrefix+tail, bindCity(sm, fn), addMutationCSRFParam)
}

// cityDelete is the DELETE sibling of cityGet. See cityPost for the
// CSRF header rationale.
func cityDelete[I any, O any](sm *SupervisorMux, tail string,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	huma.Delete(sm.humaAPI, cityScopePrefix+tail, bindCity(sm, fn), addMutationCSRFParam)
}

// cityRegister is the per-city analog of huma.Register. Use it when
// the op needs explicit OperationID, DefaultStatus, Summary, etc.
// op.Path is the tail after /v0/city/{cityName}. CSRF-header declaration
// is applied automatically for mutation verbs.
func cityRegister[I any, O any](sm *SupervisorMux, op huma.Operation,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	op.Path = cityScopePrefix + op.Path
	if isMutationMethod(op.Method) {
		addMutationCSRFParam(&op)
	}
	huma.Register(sm.humaAPI, op, bindCity(sm, fn))
}

// sseCityPrecheck wraps an SSE precheck method on Server with
// per-request city resolution. registerSSE runs the precheck before
// committing response headers, so a missing city translates into a
// 404 Problem Details on the wire.
func sseCityPrecheck[I any](sm *SupervisorMux,
	fn func(*Server, context.Context, *I) error,
) func(context.Context, *I) error {
	return func(ctx context.Context, input *I) error {
		name := cityScopeName(input)
		srv := sm.resolveCityServer(name)
		if srv == nil {
			return huma.Error404NotFound(CityNotFoundOrNotRunningDetail(name))
		}
		return fn(srv, ctx, input)
	}
}

// sseCityStream wraps an SSE stream method on Server with per-request
// city resolution. If the city has disappeared between precheck and
// stream start (race), the stream returns silently — clients see EOF.
func sseCityStream[I any](sm *SupervisorMux,
	fn func(*Server, huma.Context, *I, sse.Sender),
) func(huma.Context, *I, sse.Sender) {
	return func(hctx huma.Context, input *I, send sse.Sender) {
		srv := sm.resolveCityServer(cityScopeName(input))
		if srv == nil {
			return
		}
		fn(srv, hctx, input, send)
	}
}

// cityScopeName extracts the city name from any city-scoped Huma input.
// The type assertion is a programmer-bug tripwire — every city-scoped
// input embeds CityScope by construction, so a failure here means
// someone registered a handler whose input type does not embed it.
// Panic rather than silently returning so the mistake surfaces
// immediately in tests instead of as a confusing EOF from SSE clients.
func cityScopeName[I any](input *I) string {
	named, ok := any(input).(cityNamer)
	if !ok {
		panic(fmt.Sprintf("api: input type %T does not embed CityScope", input))
	}
	return named.GetCityName()
}

// resolveCityServer looks up (or constructs + caches) the per-city
// Server for the named city. Returns nil when the city is not known
// or not running; callers should translate nil into a 404.
func (sm *SupervisorMux) resolveCityServer(name string) *Server {
	state := sm.resolver.CityState(name)
	if state == nil {
		sm.cacheMu.Lock()
		delete(sm.cache, name)
		sm.cacheMu.Unlock()
		return nil
	}
	return sm.getCityServer(name, state)
}
