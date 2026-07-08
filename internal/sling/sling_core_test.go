package sling

import (
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
