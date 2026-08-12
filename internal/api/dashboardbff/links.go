package dashboardbff

import (
	"net/url"
	"regexp"
)

// The dashboard SPA is served by the supervisor listener, same-origin with
// this /api plane, and addresses one city at a time under a
// `/city/:cityName` router basename
// (internal/api/dashboardspa/web/frontend/src/CityBootstrap.tsx). The helpers
// below are the single source of truth for building deep links into that SPA
// from Go — the CLI and the HTTP API layer both import them — so the paths
// they return MUST mirror the SPA routes declared in App.tsx.

// cityNameRE matches a managed city name: alphanumeric with internal hyphens,
// no path separators and no leading/trailing hyphen.
var cityNameRE = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)

// ValidCityName reports whether name is a city name the dashboard serves:
// alphanumeric with internal hyphens, no leading/trailing hyphen, at most 64
// characters. This is the exact grammar the /api plane checks before any
// resolver lookup (resolveCityPath) as a defensive measure — the
// authoritative path always comes from the resolver, never from joining the
// name — so a name this rejects is dashboard-unreachable and callers should
// not emit dashboard links for it.
func ValidCityName(name string) bool {
	return name != "" && len(name) <= 64 && cityNameRE.MatchString(name)
}

// RunDetailPath returns the dashboard SPA path for one run's detail view:
// /city/{cityName}/runs/{runID}. It mirrors the SPA's `/runs/:runId` route
// (internal/api/dashboardspa/web/frontend/src/App.tsx) under the
// `/city/:cityName` basename (CityBootstrap.tsx). runID is the run-root bead
// ID; only graph.v2 run roots render a detail view there (other roots show
// the list-only not_run_view page). Both segments are path-escaped.
func RunDetailPath(cityName, runID string) string {
	return "/city/" + url.PathEscape(cityName) + "/runs/" + url.PathEscape(runID)
}

// RunsListPath returns the dashboard SPA path for a city's runs list:
// /city/{cityName}/runs. It mirrors the SPA's `/runs` route
// (internal/api/dashboardspa/web/frontend/src/App.tsx) under the
// `/city/:cityName` basename (CityBootstrap.tsx). The segment is
// path-escaped.
func RunsListPath(cityName string) string {
	return "/city/" + url.PathEscape(cityName) + "/runs"
}
