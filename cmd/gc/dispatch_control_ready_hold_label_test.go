package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// This file expresses the ga-x9kptu / ga-5736js acceptance criteria at the
// Go-level control-ready evaluation path: route-scoped results
// (filterReadyByRoute, and evaluateControlReady's routed groups) must
// exclude beads carrying a beadmeta.DispatchHoldLabels value, while the
// assignee-scoped path (filterReadyByAssignee) stays hold-transparent.

func TestFilterReadyByRouteExcludesDispatchHoldLabels(t *testing.T) {
	older := time.Unix(100, 0)
	ready := []beads.Bead{
		{ID: "ga-plain", CreatedAt: older, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}},
		{ID: "ga-held-mayor", CreatedAt: older, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}, Labels: []string{beadmeta.HoldMayorLabel}},
		{ID: "ga-held-external", CreatedAt: older, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}, Labels: []string{beadmeta.HoldExternalLabel}},
		{ID: "ga-held-both", CreatedAt: older, Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "core/control-dispatcher"}, Labels: []string{beadmeta.HoldMayorLabel, beadmeta.HoldExternalLabel}},
	}
	got := filterReadyByRoute(ready, beadmeta.RunTargetMetadataKey, "core/control-dispatcher")
	want := []string{"ga-plain"}
	if !stringSlicesEqual(beadIDs(got), want) {
		t.Fatalf("filterReadyByRoute ids = %v, want %v (hold-labeled beads must be excluded, including a bead carrying both hold labels at once)", beadIDs(got), want)
	}
}

func TestFilterReadyByAssigneeDoesNotExcludeDispatchHoldLabels(t *testing.T) {
	ready := []beads.Bead{
		{ID: "ga-held-mayor", Assignee: "cand", Labels: []string{beadmeta.HoldMayorLabel}},
	}
	got := filterReadyByAssignee(ready, "cand", workflowServeScanLimit)
	want := []string{"ga-held-mayor"}
	if !stringSlicesEqual(beadIDs(got), want) {
		t.Fatalf("filterReadyByAssignee ids = %v, want %v (assignee-scoped tier must stay hold-transparent)", beadIDs(got), want)
	}
}

func TestEvaluateControlReadyExcludesDispatchHoldLabels(t *testing.T) {
	query := workflowServeControlReadyQuery(config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"})
	parsed, ok := parseControlReadyQuery(query)
	if !ok {
		t.Fatalf("parseControlReadyQuery: query not recognized: %q", query)
	}
	envList := []string{
		"GC_SESSION_NAME=gascity--control-dispatcher",
		"GC_ALIAS=gascity/control-dispatcher",
	}
	ready := []beads.Bead{
		{ID: "ga-routed", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/control-dispatcher"}},
		{ID: "ga-routed-held", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/control-dispatcher"}, Labels: []string{beadmeta.HoldMayorLabel}},
		{ID: "ga-routed-held-both", Metadata: map[string]string{beadmeta.RunTargetMetadataKey: "gascity/control-dispatcher"}, Labels: []string{beadmeta.HoldMayorLabel, beadmeta.HoldExternalLabel}},
	}
	got := evaluateControlReady(ready, parsed, envList)
	want := []string{"ga-routed"}
	if !stringSlicesEqual(beadIDs(got), want) {
		t.Fatalf("evaluateControlReady ids = %v, want %v (hold-labeled routed bead must be excluded, including a bead carrying both hold labels at once)", beadIDs(got), want)
	}
}

// TestTryControlReadyFromCacheOrFallbackExcludesDispatchHoldLabelsFromCache is
// ga-5736js end-to-end on the cache path: a routed_to-matching bead carrying a
// beadmeta.DispatchHoldLabels value must not reach the control dispatcher's
// queue when the answer is served from CachedReady(). The filter-level tests
// above pin the rule; this one pins that the cache actually carries Labels far
// enough for the rule to fire. (End-to-end coverage originates from PR #4787.)
func TestTryControlReadyFromCacheOrFallbackExcludesDispatchHoldLabelsFromCache(t *testing.T) {
	cityDir, store := setUpControlReadyFileStoreCity(t)
	noBDOnPathForTest(t)

	target := "gascity/control-dispatcher"
	routed, err := store.Create(beads.Bead{Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target}})
	if err != nil {
		t.Fatalf("create routed bead: %v", err)
	}
	heldMayor, err := store.Create(beads.Bead{
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target},
		Labels:   []string{beadmeta.HoldMayorLabel},
	})
	if err != nil {
		t.Fatalf("create %s routed bead: %v", beadmeta.HoldMayorLabel, err)
	}
	heldExternal, err := store.Create(beads.Bead{
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: target},
		Labels:   []string{beadmeta.HoldExternalLabel},
	})
	if err != nil {
		t.Fatalf("create %s routed bead: %v", beadmeta.HoldExternalLabel, err)
	}

	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"}
	query := workflowServeControlReadyQuery(agentCfg)

	queue, handled, err := tryControlReadyFromCacheOrFallback(query, cityDir, nil)
	if err != nil {
		t.Fatalf("tryControlReadyFromCacheOrFallback: %v", err)
	}
	if !handled {
		t.Fatalf("tryControlReadyFromCacheOrFallback: handled = false, want true for a control-ready query")
	}

	var gotIDs []string
	for _, b := range queue {
		gotIDs = append(gotIDs, b.ID)
	}
	wantIDs := []string{routed.ID}
	if !stringSlicesEqual(gotIDs, wantIDs) {
		t.Fatalf("queue ids = %#v, want %#v (held beads %s and %s must not be auto-routed to the pool)", gotIDs, wantIDs, heldMayor.ID, heldExternal.ID)
	}
}

// TestTryControlReadyFromCacheOrFallbackExcludesDispatchHoldLabelsOnFallbackPath
// is ga-5736js end-to-end on the fallback path: a routed_to-matching bead
// carrying a beadmeta.DispatchHoldLabels value in the single batched
// `bd ready --json` response must not reach the control dispatcher's queue,
// matching the cache-path behavior above. The hold labels are spelled as
// literal JSON in the fake bd script on purpose -- this test pins the wire
// format bd emits, so substituting the Go constants here would defeat it.
// (End-to-end coverage originates from PR #4787.)
func TestTryControlReadyFromCacheOrFallbackExcludesDispatchHoldLabelsOnFallbackPath(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}

	tmp := t.TempDir()
	bdPath := filepath.Join(tmp, "bd")
	target := "gascity/control-dispatcher"
	script := fmt.Sprintf(`#!/bin/sh
set -eu
case "$1" in
  list)
    exit 7
    ;;
esac
printf '[{"id":"ga-fallback-routed","metadata":{"gc.routed_to":"%s"}},{"id":"ga-fallback-held-mayor","metadata":{"gc.routed_to":"%s"},"labels":["hold:mayor"]},{"id":"ga-fallback-held-external","metadata":{"gc.routed_to":"%s"},"labels":["hold:external"]}]'
`, target, target, target)
	if err := os.WriteFile(bdPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bd: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("GC_BEADS", "bd")

	agentCfg := config.Agent{Name: config.ControlDispatcherAgentName, Dir: "gascity"}
	query := workflowServeControlReadyQuery(agentCfg)

	queue, handled, err := tryControlReadyFromCacheOrFallback(query, cityDir, nil)
	if err != nil {
		t.Fatalf("tryControlReadyFromCacheOrFallback: %v", err)
	}
	if !handled {
		t.Fatalf("handled = false, want true")
	}
	var gotIDs []string
	for _, b := range queue {
		gotIDs = append(gotIDs, b.ID)
	}
	wantIDs := []string{"ga-fallback-routed"}
	if !stringSlicesEqual(gotIDs, wantIDs) {
		t.Fatalf("queue ids = %#v, want %#v (held beads ga-fallback-held-mayor and ga-fallback-held-external must not be auto-routed to the pool)", gotIDs, wantIDs)
	}
}
