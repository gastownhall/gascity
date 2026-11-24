package main

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

// TestCliBeadRouterSurfacesCustomSlingQueryDiagnostic reproduces gascity#8439:
// a custom sling_query that fails must surface its stderr/stdout diagnostic
// through cliBeadRouter.Route, not just the bare "exit status N" from the
// underlying exec error.
func TestCliBeadRouterSurfacesCustomSlingQueryDiagnostic(t *testing.T) {
	const diagnostic = "ROUTE_REJECTED: queue maintenance; retry after repair"

	runner := newFakeRunner()
	runner.on("custom-dispatch", diagnostic, fmt.Errorf("exit status 42"))

	a := config.Agent{
		Name:       "worker",
		SlingQuery: "custom-dispatch {}",
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{a},
	}
	deps := &slingDeps{
		CityName: "test-city",
		CityPath: t.TempDir(),
		Cfg:      cfg,
		Runner:   runner.run,
	}
	router := cliBeadRouter{deps: deps}

	err := router.Route(context.Background(), sling.RouteRequest{
		BeadID: "BL-1",
		Target: "worker",
	})
	if err == nil {
		t.Fatal("Route returned nil error, want error from failed custom sling_query")
	}

	gcOutput := err.Error()
	if !strings.Contains(gcOutput, "exit status 42") {
		t.Errorf("gc_output = %q, want it to contain %q", gcOutput, "exit status 42")
	}
	if !strings.Contains(gcOutput, diagnostic) {
		t.Errorf("gc_output = %q, want it to contain diagnostic %q (gascity#8439: custom sling_query output swallowed)", gcOutput, diagnostic)
	}
}
