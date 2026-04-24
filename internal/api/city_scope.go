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

// cityGet registers a per-city GET op at /v0/city/{cityName}+tail.
// The tail starts with "/" (e.g. "/agents") or is "" for the
// city-detail base path.
func cityGet[I any, O any](sm *SupervisorMux, tail string,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	huma.Get(sm.humaAPI, cityScopePrefix+tail, bindCity(sm, fn))
}

// cityPost is the POST sibling of cityGet.
func cityPost[I any, O any](sm *SupervisorMux, tail string,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	huma.Post(sm.humaAPI, cityScopePrefix+tail, bindCity(sm, fn))
}

// cityPut is the PUT sibling of cityGet.
func cityPut[I any, O any](sm *SupervisorMux, tail string,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	huma.Put(sm.humaAPI, cityScopePrefix+tail, bindCity(sm, fn))
}

// cityPatch is the PATCH sibling of cityGet.
func cityPatch[I any, O any](sm *SupervisorMux, tail string,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	huma.Patch(sm.humaAPI, cityScopePrefix+tail, bindCity(sm, fn))
}

// cityDelete is the DELETE sibling of cityGet.
func cityDelete[I any, O any](sm *SupervisorMux, tail string,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	huma.Delete(sm.humaAPI, cityScopePrefix+tail, bindCity(sm, fn))
}

// cityRegister is the per-city analog of huma.Register. Use it when
// the op needs explicit OperationID, DefaultStatus, Summary, etc.
// op.Path is the tail after /v0/city/{cityName}.
func cityRegister[I any, O any](sm *SupervisorMux, op huma.Operation,
	fn func(*Server, context.Context, *I) (*O, error),
) {
	op.Path = cityScopePrefix + op.Path
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
