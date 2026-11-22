package sling

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/graphv2"
	"github.com/gastownhall/gascity/internal/runtime"
)

// This file pins #7163: a graph workflow launched from a source bead with open
// ready-blocking dependencies waits for them. The workflow routes its own
// steps, not the source bead, so every workflow bead carries the source
// bead's blocking edges, each of its own type, from the moment it is created.

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

// newBlockedSource creates an open blocker and a source bead with a blocks
// dependency on it.
func newBlockedSource(t *testing.T, store beads.Store) (blocker, source beads.Bead) {
	t.Helper()
	blocker, err := store.Create(beads.Bead{Title: "blocker", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	source, err = store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DepAdd(source.ID, blocker.ID, "blocks"); err != nil {
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

// dependsOn reports whether id has a blocks dependency on target.
func dependsOn(t *testing.T, store beads.Store, id, target string) bool {
	t.Helper()
	return dependsOnAs(t, store, id, target, "blocks")
}

// dependsOnAs reports whether id has a dependency of depType on target.
func dependsOnAs(t *testing.T, store beads.Store, id, target, depType string) bool {
	t.Helper()
	deps, err := store.DepList(id, "down")
	if err != nil {
		t.Fatalf("DepList(%s): %v", id, err)
	}
	return slices.ContainsFunc(deps, func(d beads.Dep) bool {
		return d.DependsOnID == target && d.Type == depType
	})
}

// isReady reports whether id is among store's Ready beads.
func isReady(t *testing.T, store beads.Store, id string) bool {
	t.Helper()
	ready, err := store.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	return slices.ContainsFunc(ready, func(b beads.Bead) bool { return b.ID == id })
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
	blocker, source := newBlockedSource(t, store)

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
		if !dependsOn(t, store, b.ID, blocker.ID) {
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
	blocker, source := newBlockedSource(t, store)

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
	first, second := newBlockedSource(t, store)
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

// TestDefaultFormulaOnBlockedBeadWaitsForBlocker is AC-4: a graph workflow
// launched on a blocked bead through the target's default_sling_formula waits
// for the blocker.
func TestDefaultFormulaOnBlockedBeadWaitsForBlocker(t *testing.T) {
	deps := blockedSourceSlingDeps(t)
	store := deps.Store
	blocker, source := newBlockedSource(t, store)
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("graph-chain")}

	result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: source.ID}, deps, store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.WorkflowID == "" {
		t.Fatalf("WorkflowID is empty, want the default formula's graph workflow (method %q)", result.Method)
	}
	if ready := readyWorkflowBeads(t, store, result.WorkflowID); len(ready) > 0 {
		t.Errorf("workflow beads Ready while %s is blocked by open %s: %v", source.ID, blocker.ID, beadTitles(ready))
	}
	if !slices.Equal(result.WaitingOn, []string{blocker.ID}) {
		t.Errorf("WaitingOn = %v, want [%s]", result.WaitingOn, blocker.ID)
	}
}

// TestV1FormulaOnBlockedBeadAddsNoBlockerEdges pins the v1 side of the
// default-formula path: a v1 molecule routes the source bead itself, whose own
// edges already hold it, so its beads get no copy of the blockers.
func TestV1FormulaOnBlockedBeadAddsNoBlockerEdges(t *testing.T) {
	deps := blockedSourceSlingDeps(t)
	if err := os.WriteFile(filepath.Join(deps.Cfg.FormulaLayers.City[0], "plain-work.formula.toml"), []byte(`
formula = "plain-work"

[[steps]]
id = "do"
title = "Plain step"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := deps.Store
	blocker, source := newBlockedSource(t, store)
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("plain-work")}

	result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: source.ID}, deps, store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.WispRootID == "" || result.WorkflowID != "" {
		t.Fatalf("WispRootID = %q, WorkflowID = %q, want a v1 molecule", result.WispRootID, result.WorkflowID)
	}
	if len(result.WaitingOn) != 0 {
		t.Errorf("WaitingOn = %v, want none for a v1 molecule", result.WaitingOn)
	}
	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range all {
		if b.ID != source.ID && dependsOn(t, store, b.ID, blocker.ID) {
			t.Errorf("v1 molecule bead %s (%q) got a blocks edge on %s", b.ID, b.Title, blocker.ID)
		}
	}
}

// TestFormulaOnBeadWithOnlySatisfiedBlockersStartsReady is AC-6: a source bead
// whose ready-blocking dependencies are all satisfied launches a workflow whose
// entry step is Ready at once and that depends on none of them. A blocker
// closed with gc.work_outcome=blocked is not satisfied and still holds it.
func TestFormulaOnBeadWithOnlySatisfiedBlockersStartsReady(t *testing.T) {
	t.Run("satisfied", func(t *testing.T) {
		deps := blockedSourceSlingDeps(t)
		store := deps.Store
		blocker, source := newBlockedSource(t, store)
		waited, err := store.Create(beads.Bead{Title: "waited", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(source.ID, waited.ID, "conditional-blocks"); err != nil {
			t.Fatal(err)
		}
		for _, id := range []string{blocker.ID, waited.ID} {
			if err := store.Close(id); err != nil {
				t.Fatal(err)
			}
		}

		result, err := DoSling(SlingOpts{Target: config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, BeadOrFormula: source.ID, OnFormula: "graph-chain"}, deps, store)
		if err != nil {
			t.Fatalf("DoSling: %v", err)
		}
		if got := beadTitles(readyWorkflowBeads(t, store, result.WorkflowID)); !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, " First step") }) {
			t.Errorf("Ready workflow beads = %v, want the First step", got)
		}
		for _, b := range workflowBeads(t, store, result.WorkflowID) {
			for _, id := range []string{blocker.ID, waited.ID} {
				if dependsOn(t, store, b.ID, id) || dependsOnAs(t, store, b.ID, id, "conditional-blocks") {
					t.Errorf("workflow bead %s (%q) depends on satisfied %s", b.ID, b.Title, id)
				}
			}
		}
		if len(result.WaitingOn) != 0 {
			t.Errorf("WaitingOn = %v, want none", result.WaitingOn)
		}
	})
	t.Run("a waits-for edge is the store's to judge", func(t *testing.T) {
		// bd and native Dolt hold the source while the closed spawner's child
		// is open; the in-memory store judges the spawner's own status. The
		// workflow carries the source's own waits-for edge, so either store
		// holds it exactly when it holds the source.
		deps := blockedSourceSlingDeps(t)
		store := deps.Store
		spawner, err := store.Create(beads.Bead{Title: "spawner", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		child, err := store.Create(beads.Bead{Title: "spawned child", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		source, err := store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(child.ID, spawner.ID, "parent-child"); err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(source.ID, spawner.ID, "waits-for"); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(spawner.ID); err != nil {
			t.Fatal(err)
		}

		result, err := DoSling(SlingOpts{Target: config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, BeadOrFormula: source.ID, OnFormula: "graph-chain"}, deps, store)
		if err != nil {
			t.Fatalf("DoSling: %v", err)
		}
		if !slices.Equal(result.WaitingOn, []string{spawner.ID}) {
			t.Errorf("WaitingOn = %v, want [%s]: a closed spawner may still have open children", result.WaitingOn, spawner.ID)
		}
		var entry beads.Bead
		for _, b := range workflowBeads(t, store, result.WorkflowID) {
			if !dependsOnAs(t, store, b.ID, spawner.ID, "waits-for") {
				t.Errorf("workflow bead %s (%q) has no waits-for edge on %s", b.ID, b.Title, spawner.ID)
			}
			if b.Title == "First step" {
				entry = b
			}
		}
		if got, want := isReady(t, store, entry.ID), isReady(t, store, source.ID); got != want {
			t.Errorf("entry step Ready = %v, source Ready = %v: the store must judge the workflow's waits-for edge as it judges the source's", got, want)
		}
	})
	t.Run("closed blocked still holds", func(t *testing.T) {
		deps := blockedSourceSlingDeps(t)
		store := deps.Store
		blocker, source := newBlockedSource(t, store)
		if err := store.SetMetadata(blocker.ID, beadmeta.WorkOutcomeMetadataKey, "blocked"); err != nil {
			t.Fatal(err)
		}
		if err := store.Close(blocker.ID); err != nil {
			t.Fatal(err)
		}

		result, err := DoSling(SlingOpts{Target: config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, BeadOrFormula: source.ID, OnFormula: "graph-chain"}, deps, store)
		if err != nil {
			t.Fatalf("DoSling: %v", err)
		}
		if ready := readyWorkflowBeads(t, store, result.WorkflowID); len(ready) > 0 {
			t.Errorf("workflow beads Ready while %s closed blocked: %v", blocker.ID, beadTitles(ready))
		}
		if !slices.Equal(result.WaitingOn, []string{blocker.ID}) {
			t.Errorf("WaitingOn = %v, want [%s]", result.WaitingOn, blocker.ID)
		}
	})
	t.Run("waits-for and conditional-blocks are honored", func(t *testing.T) {
		deps := blockedSourceSlingDeps(t)
		store := deps.Store
		waiter, err := store.Create(beads.Bead{Title: "waiter", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		conditional, err := store.Create(beads.Bead{Title: "conditional", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		source, err := store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(source.ID, waiter.ID, "waits-for"); err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(source.ID, conditional.ID, "conditional-blocks"); err != nil {
			t.Fatal(err)
		}

		result, err := DoSling(SlingOpts{Target: config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, BeadOrFormula: source.ID, OnFormula: "graph-chain"}, deps, store)
		if err != nil {
			t.Fatalf("DoSling: %v", err)
		}
		want := []string{waiter.ID, conditional.ID}
		slices.Sort(want)
		if !slices.Equal(result.WaitingOn, want) {
			t.Errorf("WaitingOn = %v, want sorted %v", result.WaitingOn, want)
		}
		if ready := readyWorkflowBeads(t, store, result.WorkflowID); len(ready) > 0 {
			t.Errorf("workflow beads Ready while their waits-for/conditional-blocks blockers are open: %v", beadTitles(ready))
		}
	})
}

// TestFormulaOnBeadSkipsOwnWorkflowRoot is AC-6: a source bead that blocks on
// its own workflow root (as gc formula cook --attach records it) relaunches
// with --force into a workflow that does not wait on the one it replaces.
func TestFormulaOnBeadSkipsOwnWorkflowRoot(t *testing.T) {
	deps := blockedSourceSlingDeps(t)
	store := deps.Store
	source, err := store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	opts := SlingOpts{Target: config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}, BeadOrFormula: source.ID, OnFormula: "graph-chain"}
	first, err := DoSling(opts, deps, store)
	if err != nil {
		t.Fatalf("first DoSling: %v", err)
	}
	if err := store.DepAdd(source.ID, first.WorkflowID, "blocks"); err != nil {
		t.Fatal(err)
	}

	opts.Force = true
	second, err := DoSling(opts, deps, store)
	if err != nil {
		t.Fatalf("forced DoSling: %v", err)
	}
	if second.WorkflowID == first.WorkflowID {
		t.Fatalf("forced relaunch reused workflow %s", first.WorkflowID)
	}
	if got := beadTitles(readyWorkflowBeads(t, store, second.WorkflowID)); !slices.ContainsFunc(got, func(s string) bool { return strings.HasSuffix(s, " First step") }) {
		t.Errorf("replacement Ready workflow beads = %v, want its First step", got)
	}
	for _, b := range workflowBeads(t, store, second.WorkflowID) {
		if dependsOn(t, store, b.ID, first.WorkflowID) {
			t.Errorf("replacement bead %s (%q) waits on replaced root %s", b.ID, b.Title, first.WorkflowID)
		}
	}
	if len(second.WaitingOn) != 0 {
		t.Errorf("WaitingOn = %v, want none", second.WaitingOn)
	}
}

// TestFormulaOnBlockedBeadSplitCityRefuses is AC-8: on a city whose graph
// store is relocated away from the work store, a blocker only the work store
// holds cannot be an edge on a workflow bead, so the sling refuses, naming
// the source bead and the blocker, and leaves no trace: no graph-store bead,
// a closed synthetic input convoy, and the source bead's routing unchanged.
// A blocker the graph store does hold is wired normally.
func TestFormulaOnBlockedBeadSplitCityRefuses(t *testing.T) {
	splitDeps := func(t *testing.T) (SlingDeps, beads.Store, beads.Store) {
		t.Helper()
		formulaDir := t.TempDir()
		writeGraphV2ConvoyFormula(t, formulaDir)
		deps, work, graph := splitSlingDeps(t, graphV2SlingTestConfig(t, formulaDir))
		deps.CityPath = t.TempDir()
		return deps, work, graph
	}
	listAll := func(t *testing.T, store beads.Store) []beads.Bead {
		t.Helper()
		all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
		if err != nil {
			t.Fatal(err)
		}
		return all
	}
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1)}

	t.Run("work-only blocker refuses", func(t *testing.T) {
		deps, work, graph := splitDeps(t)
		blocker, source := newBlockedSource(t, work)
		graphBefore := len(listAll(t, graph))

		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: source.ID, OnFormula: "graph-work"}, deps, work)
		var crossErr *graphv2.CrossStoreBlockersError
		if !errors.As(err, &crossErr) {
			t.Fatalf("DoSling error = %v, want a *graphv2.CrossStoreBlockersError", err)
		}
		if crossErr.SourceID != source.ID || !slices.Equal(crossErr.BlockerIDs, []string{blocker.ID}) {
			t.Errorf("CrossStoreBlockersError = %+v, want source %s blockers [%s]", crossErr, source.ID, blocker.ID)
		}
		for _, id := range []string{source.ID, blocker.ID} {
			if !strings.Contains(err.Error(), id) {
				t.Errorf("error %q does not name %s", err, id)
			}
		}
		if got := len(listAll(t, graph)); got != graphBefore {
			t.Errorf("graph store holds %d beads after the refusal, want %d", got, graphBefore)
		}
		roots, err := graph.ListByMetadata(map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if len(roots) != 0 {
			t.Errorf("refused launch created %d workflow roots in the graph store, want none", len(roots))
		}
		// The launch's synthetic input convoy is minted before the refusal and
		// must be closed. It is found through its tracks edge on the source,
		// since a closed bead is not listed by metadata.
		up, err := work.DepList(source.ID, "up")
		if err != nil {
			t.Fatal(err)
		}
		var convoyID string
		for _, dep := range up {
			if dep.Type == "tracks" {
				convoyID = dep.IssueID
			}
		}
		if convoyID == "" {
			t.Fatal("launch minted no synthetic input convoy tracking the source")
		}
		convoy, err := work.Get(convoyID)
		if err != nil {
			t.Fatal(err)
		}
		if convoy.Type != "convoy" || convoy.Metadata[beadmeta.SyntheticMetadataKey] != "true" {
			t.Fatalf("bead %s tracking the source is not a synthetic convoy", convoyID)
		}
		if convoy.Status != "closed" {
			t.Errorf("synthetic input convoy %s is %q, want closed", convoy.ID, convoy.Status)
		}
		after, err := work.Get(source.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got := after.Metadata[beadmeta.RoutedToMetadataKey]; got != "" {
			t.Errorf("source gc.routed_to = %q after the refusal, want unchanged (empty)", got)
		}
	})

	t.Run("graph-held blocker is wired", func(t *testing.T) {
		deps, work, graph := splitDeps(t)
		blocker, err := graph.Create(beads.Bead{Title: "graph blocker", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		source, err := work.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		// bd refuses a cross-prefix edge, but a SQLite-served work ledger
		// records it as written; the strict leaf refuses it, so write past it.
		if err := work.(*splittest.StrictStore).Store.DepAdd(source.ID, blocker.ID, "blocks"); err != nil {
			t.Fatal(err)
		}

		result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: source.ID, OnFormula: "graph-work"}, deps, work)
		if err != nil {
			t.Fatalf("DoSling: %v", err)
		}
		if !slices.Equal(result.WaitingOn, []string{blocker.ID}) {
			t.Errorf("WaitingOn = %v, want [%s]", result.WaitingOn, blocker.ID)
		}
		if ready := readyWorkflowBeads(t, graph, result.WorkflowID); len(ready) > 0 {
			t.Errorf("workflow beads Ready while %s is open: %v", blocker.ID, beadTitles(ready))
		}
		for _, b := range workflowBeads(t, graph, result.WorkflowID) {
			if !dependsOn(t, graph, b.ID, blocker.ID) {
				t.Errorf("workflow bead %s (%q) has no blocks edge on %s", b.ID, b.Title, blocker.ID)
			}
		}
	})
}

// TestDefaultFormulaOnSplitCityRoutesBlockedBeadPlainly pins the split-city
// stopgap for a launch the caller never asked for: a target's
// default_sling_formula whose workflow's store cannot hold the bead's blocker
// is skipped with a warning and the bead routed plainly, as its own edges hold
// it. An explicit --on still refuses (TestFormulaOnBlockedBeadSplitCityRefuses).
func TestDefaultFormulaOnSplitCityRoutesBlockedBeadPlainly(t *testing.T) {
	formulaDir := t.TempDir()
	writeGraphV2ConvoyFormula(t, formulaDir)
	deps, work, graph := splitSlingDeps(t, graphV2SlingTestConfig(t, formulaDir))
	deps.CityPath = t.TempDir()
	blocker, source := newBlockedSource(t, work)
	a := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("graph-work")}

	result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: source.ID}, deps, work)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.WorkflowID != "" || result.Method != "bead" {
		t.Errorf("WorkflowID = %q, Method = %q, want the plain bead route", result.WorkflowID, result.Method)
	}
	if !slices.ContainsFunc(result.BeadWarnings, func(w string) bool {
		return strings.Contains(w, "routed as a plain bead instead") && strings.Contains(w, blocker.ID)
	}) {
		t.Errorf("BeadWarnings = %v, want the skipped default formula named with blocker %s", result.BeadWarnings, blocker.ID)
	}
	roots, err := graph.ListByMetadata(map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 0 {
		t.Errorf("the skipped default formula created %d workflow roots in the graph store, want none", len(roots))
	}
	// The launch minted a synthetic input convoy before the blockers were
	// read; the plain route must not leave it open.
	up, err := work.DepList(source.ID, "up")
	if err != nil {
		t.Fatal(err)
	}
	for _, dep := range up {
		if dep.Type != "tracks" {
			continue
		}
		convoy, err := work.Get(dep.IssueID)
		if err != nil {
			t.Fatal(err)
		}
		if convoy.Metadata[beadmeta.SyntheticMetadataKey] == "true" && convoy.Status != "closed" {
			t.Errorf("synthetic input convoy %s is %q after the plain route, want closed", convoy.ID, convoy.Status)
		}
	}
}
