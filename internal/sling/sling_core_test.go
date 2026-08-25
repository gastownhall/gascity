package sling

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
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

// TestDoSlingRefusesConflictingRoute pins the hard-refuse behavior: slinging
// a bead whose gc.routed_to metadata already names a different target must
// fail, and must leave the existing route untouched, when --force is not
// passed.
func TestDoSlingRefusesConflictingRoute(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "deacon", MaxActiveSessions: intPtr(1)}
	routed := beads.Bead{
		ID:     "BL-1",
		Title:  "BL-1",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "rig/polecat",
		},
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{routed}, nil)
	router := &fakeBeadRouter{}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Router = router
	deps.Store = store

	_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1"}, deps, store)
	if err == nil {
		t.Fatal("expected DoSling to refuse a conflicting gc.routed_to overwrite without --force")
	}

	// The actual gc.routed_to write is owned by the injected Router (in
	// production, cmd/gc's cliBeadRouter); the refusal must happen in
	// DoSling's own pre-flight, before Router.Route is ever called.
	if len(router.routed) != 0 {
		t.Fatalf("expected DoSling to refuse before delegating to the router, got %d route call(s)", len(router.routed))
	}

	got, getErr := store.Get("BL-1")
	if getErr != nil {
		t.Fatalf("store.Get: %v", getErr)
	}
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "rig/polecat" {
		t.Errorf("gc.routed_to = %q, want unchanged %q", got.Metadata[beadmeta.RoutedToMetadataKey], "rig/polecat")
	}
}

// TestDoSlingForceOverridesConflictingRoute pins the --force escape hatch:
// with --force, a conflicting gc.routed_to is overwritten and DoSling
// succeeds.
func TestDoSlingForceOverridesConflictingRoute(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test"}}
	a := config.Agent{Name: "deacon", MaxActiveSessions: intPtr(1)}
	routed := beads.Bead{
		ID:     "BL-1",
		Title:  "BL-1",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey: "rig/polecat",
		},
	}
	store := beads.NewMemStoreFrom(0, []beads.Bead{routed}, nil)
	router := &fakeBeadRouter{}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps.Router = router
	deps.Store = store

	_, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "BL-1", Force: true}, deps, store)
	if err != nil {
		t.Fatalf("DoSling with --force: %v", err)
	}

	// The gc.routed_to write itself belongs to the Router implementation (in
	// production, cmd/gc's cliBeadRouter); what DoSling owns is delegating to
	// it despite the pre-existing conflicting route.
	if len(router.routed) != 1 {
		t.Fatalf("got %d route call(s), want 1", len(router.routed))
	}
	if router.routed[0].BeadID != "BL-1" {
		t.Errorf("routed BeadID = %q, want BL-1", router.routed[0].BeadID)
	}
	if router.routed[0].Target != a.QualifiedName() {
		t.Errorf("routed Target = %q, want %q", router.routed[0].Target, a.QualifiedName())
	}
}
