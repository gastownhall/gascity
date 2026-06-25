package main

import (
	"os"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/dashboardbff"
	"github.com/gastownhall/gascity/internal/api/dashboardspa"
)

// dashboardCityResolver adapts the supervisor city registry to the dashboard
// /api plane's CityResolver. It resolves a city name to the host root path the
// registry already tracks, so the plane never joins an untrusted name onto a
// base path.
type dashboardCityResolver struct{ resolver api.CityResolver }

func (d dashboardCityResolver) CityPath(name string) (string, bool) {
	for _, c := range d.resolver.ListCities() {
		if c.Name == name {
			return c.Path, true
		}
	}
	return "", false
}

// dashboardEnabled reports whether the supervisor hosts the embedded dashboard.
// On by default; set GC_SUPERVISOR_DASHBOARD=0 to disable (revert to a
// typed-API-only supervisor with no static or /api surface).
func dashboardEnabled() bool {
	return os.Getenv("GC_SUPERVISOR_DASHBOARD") != "0"
}

// attachDashboard mounts the embedded SPA and the host-side /api plane onto the
// supervisor mux so the supervisor serves the dashboard same-origin. It returns
// the plane (whose samplers the caller must Start/Stop) or nil when the
// dashboard is disabled. Operator identity is read from env with neutral
// defaults applied inside the plane (ZERO hardcoded roles).
func attachDashboard(mux *api.SupervisorMux, resolver api.CityResolver, readOnly bool) (*dashboardbff.Plane, error) {
	if !dashboardEnabled() {
		return nil, nil
	}
	spa, err := dashboardspa.NewStaticHandler()
	if err != nil {
		return nil, err
	}
	plane := dashboardbff.New(dashboardbff.Deps{
		Resolver:          dashboardCityResolver{resolver},
		ReadOnly:          readOnly,
		OperatorAlias:     os.Getenv("DASHBOARD_OPERATOR_ALIAS"),
		OperatorWireAlias: os.Getenv("DASHBOARD_OPERATOR_WIRE_ALIAS"),
		DecisionLabel:     os.Getenv("DASHBOARD_DECISION_LABEL"),
		DefaultView:       os.Getenv("DEFAULT_VIEW"),
	})
	mux.WithAPIPlane(plane.Handler()).WithStaticHandler(spa)
	return plane, nil
}
