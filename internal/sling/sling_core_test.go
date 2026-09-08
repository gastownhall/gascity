package sling

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestAttachFormulaToBeadEntryShapes exercises the two attachment entry points
// that share attachFormulaToBead — --on-formula and default-formula — and
// pins the per-path pieces the wrappers select: the sling method and the
// error-label prefix ("formula" vs "default formula"). This is the drift the
// S13 consolidation eliminated: before the merge these copies could diverge
// independently, so the test asserts both success method and error prefix for
// each entry shape.
func TestAttachFormulaToBeadEntryShapes(t *testing.T) {
	newDeps := func(t *testing.T) (SlingDeps, string) {
		t.Helper()
		cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
		deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
		b, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		return deps, b.ID
	}

	t.Run("on-formula success", func(t *testing.T) {
		deps, beadID := newDeps(t)
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
		result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID, OnFormula: "code-review"}, deps, deps.Store)
		if err != nil {
			t.Fatalf("DoSling on-formula: %v", err)
		}
		if result.Method != "on-formula" {
			t.Errorf("Method = %q, want on-formula", result.Method)
		}
		if result.FormulaName != "code-review" {
			t.Errorf("FormulaName = %q, want code-review", result.FormulaName)
		}
		if result.WispRootID == "" {
			t.Error("expected non-empty WispRootID")
		}
	})

	t.Run("default-formula success", func(t *testing.T) {
		deps, beadID := newDeps(t)
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("code-review")}
		result, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID}, deps, deps.Store)
		if err != nil {
			t.Fatalf("DoSling default-formula: %v", err)
		}
		if result.Method != "default-on-formula" {
			t.Errorf("Method = %q, want default-on-formula", result.Method)
		}
		if result.FormulaName != "code-review" {
			t.Errorf("FormulaName = %q, want code-review", result.FormulaName)
		}
		if result.WispRootID == "" {
			t.Error("expected non-empty WispRootID")
		}
	})

	t.Run("on-formula error label", func(t *testing.T) {
		deps, beadID := newDeps(t)
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID, OnFormula: "nonexistent-formula"}, deps, deps.Store)
		if err == nil {
			t.Fatal("expected instantiation error for nonexistent on-formula")
		}
		if want := `instantiating formula "nonexistent-formula" on`; !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want prefix %q", err.Error(), want)
		}
	})

	t.Run("default-formula error label", func(t *testing.T) {
		deps, beadID := newDeps(t)
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("nonexistent-formula")}
		_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: beadID}, deps, deps.Store)
		if err == nil {
			t.Fatal("expected instantiation error for nonexistent default formula")
		}
		if want := `instantiating default formula "nonexistent-formula" on`; !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want prefix %q", err.Error(), want)
		}
	})
}

// TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttached pins the
// REPAIR SCOPE for ga-ueugmi: a bare sling (no explicit --on/--formula) to a
// target whose config carries a default_sling_formula must not hard-fail when
// the bead already has a live attached molecule from an unrelated workflow --
// the caller never asked for a formula attach, so the implicit default should
// yield to plain routing (and warn) instead of blocking the sling and leaving
// gc.routed_to unset. An explicit --on/--formula request must keep
// hard-failing in this situation (TestOnFormulaExistingMoleculeErrors in
// cmd/gc/cmd_sling_test.go) -- only the implicit default-formula path falls
// back.
func TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttached(t *testing.T) {
	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-1", Type: "task", Status: "open", Assignee: "reviewer-session"},
		{ID: "MOL-1", Type: "molecule", Status: "open", ParentID: "BL-1"},
	}, nil)
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = store

	a := config.Agent{Name: "builder", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("code-review")}
	opts := SlingOpts{Target: a, BeadOrFormula: "BL-1", NoConvoy: true}

	result, err := DoSling(opts, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling default-formula with attached molecule: expected no error (fallback to plain route), got %v", err)
	}
	if result.Method != "bead" {
		t.Errorf("Method = %q, want %q (fell back to plain bead routing)", result.Method, "bead")
	}
	if result.FormulaName != "" {
		t.Errorf("FormulaName = %q, want empty (formula was not attached)", result.FormulaName)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1 (plain route must still execute, so gc.routed_to gets set)", len(runner.calls))
	}

	var warned bool
	for _, w := range result.BeadWarnings {
		if strings.Contains(w, "MOL-1") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("BeadWarnings = %v, want a warning naming the skipped attachment MOL-1", result.BeadWarnings)
	}
}

// TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttachedGraphV2Formula
// extends the ga-ueugmi repair scope (see the test above) to the graph.v2
// branch of attachFormulaToBead: when the target's default_sling_formula
// resolves through the graph.v2 compiler (isGraph == true), a pre-existing
// non-workflow molecule/wisp attachment must fall back to plain routing the
// same way the legacy branch does, instead of hard-failing through
// CheckNoMoleculeChildrenAllowLiveWorkflow. It also pins that the synthetic
// input convoy prepareGraphV2FormulaInvocation mints before that check runs
// gets closed on the fallback path rather than leaked as an open,
// claim-attracting convoy (refs ga-9azvzg).
func TestDoSlingDefaultFormulaFallsBackToPlainRouteWhenMoleculeAttachedGraphV2Formula(t *testing.T) {
	dir := t.TempDir()
	content := "formula = \"graph-review\"\nversion = 1\ncontract = \"graph.v2\"\ntype = \"workflow\"\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n"
	if err := os.WriteFile(filepath.Join(dir, "graph-review.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("writing graph-review fixture: %v", err)
	}

	store := beads.NewMemStoreFrom(0, []beads.Bead{
		{ID: "BL-1", Type: "task", Status: "open", Assignee: "reviewer-session"},
		{ID: "MOL-1", Type: "molecule", Status: "open", ParentID: "BL-1"},
	}, nil)
	cfg := &config.City{
		Workspace:     config.Workspace{Name: "test"},
		FormulaLayers: config.FormulaLayers{City: []string{dir}},
	}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = store

	a := config.Agent{Name: "builder", MaxActiveSessions: intPtr(1), DefaultSlingFormula: stringPtr("graph-review")}
	opts := SlingOpts{Target: a, BeadOrFormula: "BL-1", NoConvoy: true}

	result, err := DoSling(opts, deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling default-formula (graph.v2) with attached molecule: expected no error (fallback to plain route), got %v", err)
	}
	if result.Method != "bead" {
		t.Errorf("Method = %q, want %q (fell back to plain bead routing)", result.Method, "bead")
	}
	if result.FormulaName != "" {
		t.Errorf("FormulaName = %q, want empty (formula was not attached)", result.FormulaName)
	}
	if len(runner.calls) != 1 {
		t.Fatalf("runner calls = %d, want 1 (plain route must still execute, so gc.routed_to gets set)", len(runner.calls))
	}

	var warned bool
	for _, w := range result.BeadWarnings {
		if strings.Contains(w, "MOL-1") {
			warned = true
		}
	}
	if !warned {
		t.Errorf("BeadWarnings = %v, want a warning naming the skipped attachment MOL-1", result.BeadWarnings)
	}

	convoys, err := deps.Store.List(beads.ListQuery{Type: "convoy", IncludeClosed: true})
	if err != nil {
		t.Fatalf("List(convoy): %v", err)
	}
	if len(convoys) != 1 {
		t.Fatalf("convoys = %d, want 1 (the synthetic input convoy minted by prepareGraphV2FormulaInvocation)", len(convoys))
	}
	if convoys[0].Status != "closed" {
		t.Errorf("synthetic input convoy %s status = %q, want closed (fallback must close it, not leak it)", convoys[0].ID, convoys[0].Status)
	}
}
