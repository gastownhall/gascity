package sling

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// This file pins #7163: a graph workflow launched from a source bead with open
// ready-blocking dependencies waits for them. The workflow routes its own
// steps, not the source bead, so every workflow bead carries the source
// bead's open blockers as blocks edges from the moment it is created.

const blockedSourceChainFormula = `
formula = "graph-chain"
version = 2
contract = "graph.v2"

[[steps]]
id = "first"
title = "First step"

[[steps]]
id = "second"
title = "Second step"
needs = ["first"]
`

// blockedSourceSlingDeps returns single-store graph.v2 sling deps whose
// formula layer holds graph-work (one step) and graph-chain (two steps, the
// second needing the first).
func blockedSourceSlingDeps(t *testing.T) SlingDeps {
	t.Helper()
	formulaDir := t.TempDir()
	writeGraphV2ConvoyFormula(t, formulaDir)
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-chain.formula.toml"), []byte(blockedSourceChainFormula), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := graphV2SlingTestConfig(t, formulaDir)
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.CityPath = t.TempDir()
	return deps
}

// newBlockedSource creates an open blocker and a source bead blocked by it.
func newBlockedSource(t *testing.T, store beads.Store, depType string) (blocker, source beads.Bead) {
	t.Helper()
	blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	source, err = store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(source.ID, blocker.ID, depType); err != nil {
		t.Fatal(err)
	}
	return blocker, source
}

// workflowBeads returns the root and every bead stamped with its root id.
func workflowBeads(t *testing.T, store beads.Store, rootID string) []beads.Bead {
	t.Helper()
	root, err := store.Get(rootID)
	if err != nil {
		t.Fatalf("Get(root %s): %v", rootID, err)
	}
	members, err := store.ListByMetadata(map[string]string{beadmeta.RootBeadIDMetadataKey: rootID}, 0, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("listing workflow %s: %v", rootID, err)
	}
	if len(members) == 0 {
		t.Fatalf("workflow %s has no member beads", rootID)
	}
	return append([]beads.Bead{root}, members...)
}

// readyWorkflowBeads returns the Ready beads that belong to the workflow.
func readyWorkflowBeads(t *testing.T, store beads.Store, rootID string) []beads.Bead {
	t.Helper()
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	var out []beads.Bead
	for _, b := range ready {
		if b.ID == rootID || b.Metadata[beadmeta.RootBeadIDMetadataKey] == rootID {
			out = append(out, b)
		}
	}
	return out
}

// dependsOn reports whether id has a dependency of depType on target.
func dependsOn(t *testing.T, store beads.Store, id, target, depType string) bool {
	t.Helper()
	deps, err := store.DepList(id, "down")
	if err != nil {
		t.Fatalf("DepList(%s): %v", id, err)
	}
	return slices.ContainsFunc(deps, func(d beads.Dep) bool {
		return d.DependsOnID == target && d.Type == depType
	})
}

func beadTitles(bs []beads.Bead) []string {
	titles := make([]string, 0, len(bs))
	for _, b := range bs {
		titles = append(titles, b.ID+" "+b.Title)
	}
	return titles
}

// TestFormulaOnBlockedBeadWaitsForBlocker is AC-1: slinging a graph formula
// onto a blocked bead with --on launches a workflow whose every bead waits on
// the blocker, and reports the blocker in WaitingOn.
func TestFormulaOnBlockedBeadWaitsForBlocker(t *testing.T) {
	deps := blockedSourceSlingDeps(t)
	store := deps.Store
	blocker, source := newBlockedSource(t, store, "blocks")

	result, err := DoSling(SlingOpts{Target: config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, BeadOrFormula: source.ID, OnFormula: "graph-work"}, deps, store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.WorkflowID == "" {
		t.Fatal("WorkflowID is empty, want a graph workflow launch")
	}
	if ready := readyWorkflowBeads(t, store, result.WorkflowID); len(ready) > 0 {
		t.Errorf("workflow beads Ready while %s is blocked by open %s: %v", source.ID, blocker.ID, beadTitles(ready))
	}
	for _, b := range workflowBeads(t, store, result.WorkflowID) {
		if !dependsOn(t, store, b.ID, blocker.ID, "blocks") {
			t.Errorf("workflow bead %s (%q) has no blocks edge on %s", b.ID, b.Title, blocker.ID)
		}
	}
	if !slices.Equal(result.WaitingOn, []string{blocker.ID}) {
		t.Errorf("WaitingOn = %v, want [%s]", result.WaitingOn, blocker.ID)
	}
}

// TestFormulaOnBlockedBeadReleasesWhenBlockerCloses is AC-2: once the blocker
// closes, the entry step is Ready and routed to the target, and a later step
// still waits on its in-recipe predecessor.
func TestFormulaOnBlockedBeadReleasesWhenBlockerCloses(t *testing.T) {
	deps := blockedSourceSlingDeps(t)
	store := deps.Store
	blocker, source := newBlockedSource(t, store, "blocks")

	result, err := DoSling(SlingOpts{Target: config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, BeadOrFormula: source.ID, OnFormula: "graph-chain"}, deps, store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if ready := readyWorkflowBeads(t, store, result.WorkflowID); len(ready) > 0 {
		t.Fatalf("workflow beads Ready before %s closed: %v", blocker.ID, beadTitles(ready))
	}
	if err := store.Close(blocker.ID); err != nil {
		t.Fatalf("Close(blocker): %v", err)
	}
	ready := readyWorkflowBeads(t, store, result.WorkflowID)
	var first *beads.Bead
	for i := range ready {
		switch ready[i].Title {
		case "First step":
			first = &ready[i]
		case "Second step":
			t.Errorf("second step %s is Ready before its in-recipe predecessor closed", ready[i].ID)
		}
	}
	if first == nil {
		t.Fatalf("entry step not Ready after %s closed; ready workflow beads = %v", blocker.ID, beadTitles(ready))
	}
	if got := first.Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
		t.Errorf("entry step gc.routed_to = %q, want %q", got, "worker")
	}
}

// TestBatchFormulaMemberWaitsForPredecessorMember is AC-3: when one batch
// member blocks on another, the dependent member's graph workflow waits for
// its predecessor while the predecessor's workflow starts at once.
func TestBatchFormulaMemberWaitsForPredecessorMember(t *testing.T) {
	deps := blockedSourceSlingDeps(t)
	store := deps.Store
	first, second := newBlockedSource(t, store, "blocks")
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}
	opts := SlingOpts{Target: a, OnFormula: "graph-chain"}

	firstResult, err := attachBatchFormula(context.Background(), opts, deps, first, a, "graph-chain", "formula", "batch", true)
	if err != nil {
		t.Fatalf("attachBatchFormula(%s): %v", first.ID, err)
	}
	secondResult, err := attachBatchFormula(context.Background(), opts, deps, second, a, "graph-chain", "formula", "batch", true)
	if err != nil {
		t.Fatalf("attachBatchFormula(%s): %v", second.ID, err)
	}

	if got := beadTitles(readyWorkflowBeads(t, store, firstResult.WorkflowID)); !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, " First step") }) {
		t.Errorf("predecessor %s workflow Ready beads = %v, want its First step", first.ID, got)
	}
	if len(firstResult.WaitingOn) != 0 {
		t.Errorf("predecessor WaitingOn = %v, want none", firstResult.WaitingOn)
	}
	if ready := readyWorkflowBeads(t, store, secondResult.WorkflowID); len(ready) > 0 {
		t.Errorf("dependent %s workflow Ready while %s is open: %v", second.ID, first.ID, beadTitles(ready))
	}
	if !slices.Equal(secondResult.WaitingOn, []string{first.ID}) {
		t.Errorf("dependent WaitingOn = %v, want [%s]", secondResult.WaitingOn, first.ID)
	}

	if err := store.Close(first.ID); err != nil {
		t.Fatal(err)
	}
	if got := beadTitles(readyWorkflowBeads(t, store, secondResult.WorkflowID)); !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, " First step") }) {
		t.Errorf("dependent %s workflow Ready beads after %s closed = %v, want its First step", second.ID, first.ID, got)
	}
}
