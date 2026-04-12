//go:build integration

package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGastown_FormulaList validates gc formula list shows formulas
// from the configured formulas directory.
func TestGastown_FormulaList(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	// Add formulas dir and a formula.
	formulaDir := filepath.Join(cityDir, "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("creating formulas dir: %v", err)
	}

	formula := `formula = "test-patrol"
description = "Test patrol"

[[steps]]
id = "check"
title = "Check status"

[[steps]]
id = "act"
title = "Take action"
needs = ["check"]
`
	if err := os.WriteFile(filepath.Join(formulaDir, "test-patrol.formula.toml"), []byte(formula), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	out, err := gc(cityDir, "formula", "list")
	if err != nil {
		t.Fatalf("gc formula list failed: %v\noutput: %s", err, out)
	}
	for _, want := range []string{"test-patrol", "pancakes"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in formula list:\n%s", want, out)
		}
	}
}

// TestGastown_FormulaShow validates gc formula show displays step info.
func TestGastown_FormulaShow(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	formulaDir := filepath.Join(cityDir, ".gc", "formulas")
	if err := os.MkdirAll(formulaDir, 0o755); err != nil {
		t.Fatalf("creating formulas dir: %v", err)
	}

	formula := `formula = "pancakes"
description = "Make pancakes"

[[steps]]
id = "dry"
title = "Mix dry ingredients"

[[steps]]
id = "wet"
title = "Mix wet ingredients"

[[steps]]
id = "combine"
title = "Combine"
needs = ["dry", "wet"]

[[steps]]
id = "cook"
title = "Cook"
needs = ["combine"]
`
	if err := os.WriteFile(filepath.Join(formulaDir, "pancakes.formula.toml"), []byte(formula), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	out, err := gc(cityDir, "formula", "show", "pancakes")
	if err != nil {
		t.Fatalf("gc formula show failed: %v\noutput: %s", err, out)
	}
	for _, step := range []string{"dry", "wet", "combine", "cook"} {
		if !strings.Contains(out, step) {
			t.Errorf("expected step %q in formula show output:\n%s", step, out)
		}
	}
}

// TestGastown_FormulaNonexistent validates error on showing nonexistent formula.
func TestGastown_FormulaNonexistent(t *testing.T) {
	agents := []gasTownAgent{
		{Name: "mayor", StartCommand: "sleep 3600"},
	}
	cityDir := setupGasTownCityNoGuard(t, agents)

	_, err := gc(cityDir, "formula", "show", "nonexistent")
	if err == nil {
		t.Fatal("expected error showing nonexistent formula")
	}
}
