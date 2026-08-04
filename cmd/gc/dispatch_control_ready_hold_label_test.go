package main

import (
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
