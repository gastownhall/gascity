package api

import (
	"net/http"
	"strings"
	"time"
)

// ServeHTTP is a test-only compatibility shim for older tests that still hit
// bare per-city paths like /v0/mail. Production traffic goes through the
// supervisor at the real scoped routes.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if strings.HasPrefix(path, "/openapi") || path == "/docs" {
		s.mux.ServeHTTP(w, r)
		return
	}

	sm := NewSupervisorMux(&stateCityResolver{state: s.state}, s.readOnly, "test", time.Now())
	sm.cacheMu.Lock()
	sm.cache[s.state.CityName()] = cachedCityServer{state: s.state, srv: s}
	sm.cacheMu.Unlock()

	r2 := r.Clone(r.Context())
	r2.URL.Path = testShimRewritePath(r.URL.Path, s.state.CityName())
	r2.URL.RawPath = ""
	wrapTestSupervisorMiddleware(sm).ServeHTTP(w, r2)
}

func testShimRewritePath(path, cityName string) string {
	if strings.HasPrefix(path, "/v0/city/") {
		return path
	}
	if path == "/v0/city" {
		return "/v0/city/" + cityName
	}
	if strings.HasPrefix(path, "/openapi") || path == "/docs" {
		return path
	}
	if strings.HasPrefix(path, "/svc/") {
		return "/v0/city/" + cityName + path
	}
	if path == "/health" {
		return "/v0/city/" + cityName + "/health"
	}
	if strings.HasPrefix(path, "/v0/") {
		return "/v0/city/" + cityName + strings.TrimPrefix(path, "/v0")
	}
	return path
}
