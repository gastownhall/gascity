package api

import (
	"context"

	"github.com/danielgtaylor/huma/v2"
)

// CityScope is the path-parameter mixin embedded by every city-scoped
// Huma input type. It declares `{cityName}` as a required path segment
// so the OpenAPI spec describes the real URL shape.
//
// Register city-scoped operations at "/v0/city/{cityName}/..." and
// wrap the handler with bindCity so the supervisor resolves the
// target per-city Server before calling through to the underlying
// handler method.
type CityScope struct {
	CityName string `path:"cityName" minLength:"1" pattern:"\\S" doc:"City name."`
}

// GetCityName returns the value of the cityName path parameter.
// Declared on a pointer receiver so types that embed CityScope by
// value satisfy the cityNamer interface via *T method promotion.
func (c *CityScope) GetCityName() string { return c.CityName }

// cityNamer is satisfied by every type that embeds CityScope.
// bindCity uses it to extract the target city name without reflection.
type cityNamer interface {
	GetCityName() string
}

// bindCity wraps a per-city handler method expression as a Huma
// handler registered on the supervisor API. The returned function
// resolves the per-city Server for input.GetCityName() and delegates.
// Returns 404 Problem Details when the named city is not running.
func bindCity[I cityNamer, O any](
	sm *SupervisorMux,
	fn func(*Server, context.Context, I) (*O, error),
) func(context.Context, I) (*O, error) {
	return func(ctx context.Context, input I) (*O, error) {
		name := input.GetCityName()
		srv := sm.resolveCityServer(name)
		if srv == nil {
			return nil, huma.Error404NotFound("not_found: city not found or not running: " + name)
		}
		return fn(srv, ctx, input)
	}
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
