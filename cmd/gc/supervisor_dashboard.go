package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
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
	plane := dashboardbff.New(dashboardDeps(resolver, readOnly, bind, port, mux.LoopbackTransport()))
	mux.WithAPIPlane(plane.Handler()).WithStaticHandler(spa)
	// Install the listener's link base alongside the SPA so per-city handlers
	// can mint dashboard deep links (the sling response's dashboard_url).
	// Standalone controller processes never call attachDashboard, so their
	// /v0 responses omit the link instead of pointing at a dead origin.
	// Wildcard binds also skip the base: dashboardLoopbackBaseURL would yield
	// a loopback literal that is browser-reachable only on the supervisor
	// host, so a remote /v0 caller would receive a dashboard_url pointing at
	// its own machine. Omitting the link is the decided degradation — do NOT
	// derive a base from request Host headers, which are spoofable.
	if !wildcardBind(bind) {
		base := dashboardLoopbackBaseURL(bind, port)
		mux.WithDashboardBase(func() string { return base })
	}
	return plane, nil
}

// dashboardDeps builds the plane's dependencies. Extracted so a regression test
// can assert the wiring (notably a non-empty SupervisorBaseURL, without which
// the host-side samplers would silently ship permanently degraded, and a
// non-nil SelfReadTransport, without which the samplers' loopback self-reads
// would 401 under read-auth). selfRead is the supervisor's in-process loopback
// transport so those trusted self-reads bypass the read-auth gate.
func dashboardDeps(resolver api.CityResolver, readOnly bool, bind string, port int, selfRead http.RoundTripper) dashboardbff.Deps {
	return dashboardbff.Deps{
		Resolver:           dashboardCityResolver{resolver},
		ReadOnly:           readOnly,
		SupervisorBaseURL:  dashboardLoopbackBaseURL(bind, port),
		SelfReadTransport:  selfRead,
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

// wildcardBind reports whether bind is a wildcard listener address (every
// spelling dashboardLoopbackBaseURL normalizes as wildcard; the empty string
// is NOT one — it means the config default, which BindOrDefault resolves to
// loopback). Wildcard binds have no single browser-reachable origin, so
// attachDashboard skips the dashboard link base for them.
func wildcardBind(bind string) bool {
	switch bind {
	case "0.0.0.0", "::", "[::]":
		return true
	}
	return false
}

// dashboardLoopbackBaseURL builds the base URL the host-side samplers use to
// read the supervisor's own /v0 API in-process. The supervisor may bind a
// wildcard or non-loopback address, but the self-read must always dial
// loopback, so wildcard/localhost binds are normalized to a loopback literal.
// This is the samplers' self-read address, not necessarily a browser-reachable
// origin — for wildcard binds attachDashboard must not reuse it as the
// dashboard link base.
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
