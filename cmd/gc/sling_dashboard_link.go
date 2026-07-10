package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/dashboardbff"
	"github.com/gastownhall/gascity/internal/sling"
)

// slingDashboardHealthTimeout bounds the dashboard health probe so link
// resolution can never noticeably slow down a successful sling.
const slingDashboardHealthTimeout = time.Second

// slingDashboardURLHook resolves the dashboard deep link surfaced after a
// successful sling. Package var so tests can stub the whole chain.
var slingDashboardURLHook = slingDashboardURL

// dashboardHealthOKHook probes the dashboard /api plane. Package var so
// resolver tests can fake the probe without a live supervisor.
var dashboardHealthOKHook = dashboardHealthOK

// slingDashboardURL returns the absolute dashboard URL for a successful
// sling result, or "" when no live link can be minted. It never returns an
// error: any resolution failure degrades silently to no link, because the
// link is a convenience and must not fail or slow the sling itself.
//
// The dashboard SPA is served only by the supervisor listener (same-origin
// with the /api BFF plane), so resolution is supervisor-only — the
// standalone controller's [api] port serves /v0 without the SPA and would
// mint dead links. The chain: supervisor alive → supervisor base URL →
// city registered with the supervisor (the SPA routes by registry name,
// not config city name) → name passes the BFF grammar → dashboard actually
// mounted (GET /api/health) → deep link. A single result carrying a
// graph.v2 workflow root links straight to that run's detail view; every
// other successful shape (wisps, plain beads, batches, idempotent skips)
// links to the runs list, since only graph.v2 roots render run detail.
func slingDashboardURL(cityPath string, result sling.SlingResult) string {
	if supervisorAliveHook() == 0 {
		return ""
	}
	baseURL, err := supervisorAPIBaseURLHook()
	if err != nil {
		return ""
	}
	baseURL = strings.TrimRight(baseURL, "/")
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil || !registered {
		return ""
	}
	name := entry.EffectiveName()
	if !dashboardbff.ValidCityName(name) {
		return ""
	}
	if !dashboardHealthOKHook(baseURL) {
		return ""
	}
	if result.WorkflowID != "" && len(result.Children) == 0 && result.ContainerType == "" {
		return baseURL + dashboardbff.RunDetailPath(name, result.WorkflowID)
	}
	return baseURL + dashboardbff.RunsListPath(name)
}

// dashboardHealthOK reports whether the dashboard /api plane is mounted at
// baseURL by probing its unauthenticated GET /api/health endpoint. The
// endpoint exists only when the dashboard is mounted, so anything but a
// fast 200 means no link should be emitted.
func dashboardHealthOK(baseURL string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), slingDashboardHealthTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/api/health", nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close() //nolint:errcheck // read-only probe
	return resp.StatusCode == http.StatusOK
}
