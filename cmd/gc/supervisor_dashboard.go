package main

import (
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/dashboardbff"
	"github.com/gastownhall/gascity/internal/api/dashboardspa"
	"github.com/gastownhall/gascity/internal/supervisor"
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

// Cities returns every managed city (name + host root path) so the dashboard
// plane can eager-warm each city's run-view fold at startup. It maps the
// supervisor registry's ListCities entries onto the plane's CityRef shape.
func (d dashboardCityResolver) Cities() []dashboardbff.CityRef {
	cities := d.resolver.ListCities()
	refs := make([]dashboardbff.CityRef, 0, len(cities))
	for _, c := range cities {
		refs = append(refs, dashboardbff.CityRef{Name: c.Name, Path: c.Path})
	}
	return refs
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
// dashboard is disabled. bind/port are the supervisor's own listener address;
// the host-side samplers read the supervisor's /v0 API back over loopback, so
// the plane must know where to reach it. Operator identity is read from env with
// neutral defaults applied inside the plane (ZERO hardcoded roles).
func attachDashboard(mux *api.SupervisorMux, resolver api.CityResolver, readOnly bool, bind string, port int) (*dashboardbff.Plane, error) {
	if !dashboardEnabled() {
		return nil, nil
	}
	spa, err := dashboardspa.NewStaticHandler()
	if err != nil {
		return nil, err
	}
	plane := dashboardbff.New(dashboardDeps(resolver, readOnly, bind, port))
	mux.WithAPIPlane(plane.Handler()).WithStaticHandler(spa)
	return plane, nil
}

// dashboardDeps builds the plane's dependencies. Extracted so a regression test
// can assert the wiring (notably a non-empty SupervisorBaseURL, without which
// the host-side samplers would silently ship permanently degraded).
func dashboardDeps(resolver api.CityResolver, readOnly bool, bind string, port int) dashboardbff.Deps {
	return dashboardbff.Deps{
		Resolver:           dashboardCityResolver{resolver},
		ReadOnly:           readOnly,
		SupervisorBaseURL:  dashboardLoopbackBaseURL(bind, port),
		RunCwdAllowedRoots: runCwdAllowedRootsFromEnv(),
		OperatorAlias:      os.Getenv("DASHBOARD_OPERATOR_ALIAS"),
		OperatorWireAlias:  os.Getenv("DASHBOARD_OPERATOR_WIRE_ALIAS"),
		DecisionLabel:      os.Getenv("DASHBOARD_DECISION_LABEL"),
		DefaultView:        os.Getenv("DEFAULT_VIEW"),
		// EnabledModules is intentionally left unset: every shipped dashboard view
		// module is core (always on), and no first-party (gated) module exists yet,
		// so the config projection emits an empty enabledModules list by design.
		// When the first gated module lands, wire its enable source here (mirroring
		// runCwdAllowedRootsFromEnv) and update TestDashboardDepsModulesCoreOnly.
	}
}

// dashboardLoopbackBaseURL builds the base URL the host-side samplers use to
// read the supervisor's own /v0 API in-process. The supervisor may bind a
// wildcard or non-loopback address, but the self-read must always dial
// loopback, so wildcard/localhost binds are normalized to a loopback literal.
func dashboardLoopbackBaseURL(bind string, port int) string {
	host := bind
	switch bind {
	case "", "0.0.0.0", "localhost":
		host = "127.0.0.1"
	case "::", "[::]":
		host = "::1"
	}
	return "http://" + net.JoinHostPort(host, strconv.Itoa(port))
}

// printDashboardStartHint prints the dashboard URL after a backgrounded
// "gc supervisor start", when the dashboard is enabled. Best-effort: a
// config-load failure simply skips the hint (the foreground supervisor log and
// "gc dashboard" still surface the URL), so it never affects start success.
func printDashboardStartHint(stdout io.Writer) {
	if !dashboardEnabled() {
		return
	}
	cfg, err := supervisorLoadConfig(supervisor.ConfigPath())
	if err != nil {
		return
	}
	url := dashboardLoopbackBaseURL(cfg.Supervisor.BindOrDefault(), cfg.Supervisor.PortOrDefault())
	fmt.Fprintf(stdout, "Dashboard:  %s/\n", url) //nolint:errcheck // best-effort stdout
}

// runCwdAllowedRootsFromEnv parses RUN_CWD_ALLOWED_ROOTS (PATH-style,
// list-separated absolute roots) that run-diff git reads are confined to, in
// addition to each request's own resolved city directory. Empty, relative, and
// whitespace-only entries are dropped.
func runCwdAllowedRootsFromEnv() []string {
	raw := os.Getenv("RUN_CWD_ALLOWED_ROOTS")
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var roots []string
	for _, p := range filepath.SplitList(raw) {
		if p = strings.TrimSpace(p); p != "" && filepath.IsAbs(p) {
			roots = append(roots, p)
		}
	}
	return roots
}
