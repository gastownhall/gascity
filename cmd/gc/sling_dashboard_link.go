package main

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/api/dashboardbff"
	"github.com/gastownhall/gascity/internal/sling"
)

// Link resolution runs inline after a successful sling, so every network
// step is deadline-bounded: the supervisor liveness ping gets
// slingDashboardLivenessTimeout (shared across all socket candidates) and
// the dashboard health probe gets slingDashboardHealthTimeout. Worst case a
// wedged supervisor delays the sling output by ~1.5s total (0.5s liveness +
// 1s probe); the remaining steps are local file reads.
const (
	slingDashboardLivenessTimeout = 500 * time.Millisecond
	slingDashboardHealthTimeout   = time.Second
)

// slingDashboardURLHook resolves the dashboard deep link surfaced after a
// successful sling. Package var so tests can stub the whole chain.
var slingDashboardURLHook = slingDashboardURL

// slingSupervisorAliveHook probes supervisor liveness under a deadline.
// Package var so resolver tests can fake liveness without a control socket.
var slingSupervisorAliveHook = slingSupervisorAliveUntil

// dashboardHealthOKHook probes the dashboard /api plane. Package var so
// resolver tests can fake the probe without a live supervisor.
var dashboardHealthOKHook = dashboardHealthOK

// slingDashboardURL returns the absolute dashboard URL for a successful
// sling result, or "" when no live link can be minted, plus whether the
// link lands on the runs list (so callers can warn that list landings lag
// cache-reconcile) rather than a run's detail view. It never returns an
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
func slingDashboardURL(cityPath string, result sling.SlingResult) (url string, runsList bool) {
	if slingSupervisorAliveHook(time.Now().Add(slingDashboardLivenessTimeout)) == 0 {
		return "", false
	}
	baseURL, err := supervisorAPIBaseURLHook()
	if err != nil {
		return "", false
	}
	baseURL = strings.TrimRight(baseURL, "/")
	entry, registered, err := registeredCityEntry(cityPath)
	if err != nil || !registered {
		return "", false
	}
	name := entry.EffectiveName()
	if !dashboardbff.ValidCityName(name) {
		return "", false
	}
	if !dashboardHealthOKHook(baseURL) {
		return "", false
	}
	if result.WorkflowID != "" && len(result.Children) == 0 && result.ContainerType == "" {
		return baseURL + dashboardbff.RunDetailPath(name, result.WorkflowID), false
	}
	return baseURL + dashboardbff.RunsListPath(name), true
}

// slingSupervisorAliveUntil reports the running supervisor's PID by pinging
// each control-socket candidate under one shared deadline, or 0 when none
// answers in time. It is the deadline-bounded sibling of supervisorAlive,
// whose ~3s-per-socket default budget is too slow for the post-sling path:
// here a wedged socket must cost at most the caller's budget, never the
// sling.
func slingSupervisorAliveUntil(deadline time.Time) int {
	for _, sockPath := range supervisorSocketPathCandidates() {
		if pid := supervisorAliveAtPathUntil(sockPath, deadline); pid != 0 {
			return pid
		}
	}
	return 0
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
