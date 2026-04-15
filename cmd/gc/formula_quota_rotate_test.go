package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

// TestFormulaQuotaRotate_Resolves verifies that the quota-rotate formula is
// included in the embedded defaultFormulas FS and can be resolved by the
// formula compiler when placed in a search path.
func TestFormulaQuotaRotate_Resolves(t *testing.T) {
	// Materialize the embedded formula to a temp directory so the
	// formula compiler can discover it via search paths.
	dir := t.TempDir()
	data, err := defaultFormulas.ReadFile("formulas/quota-rotate.formula.toml")
	if err != nil {
		t.Fatalf("quota-rotate formula not found in embedded FS: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quota-rotate.formula.toml"), data, 0o644); err != nil {
		t.Fatalf("writing formula to temp dir: %v", err)
	}

	recipe, err := formula.Compile(context.Background(), "quota-rotate", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile quota-rotate: %v", err)
	}
	if recipe.Name != "quota-rotate" {
		t.Errorf("recipe.Name = %q, want %q", recipe.Name, "quota-rotate")
	}
}

// TestFormulaQuotaRotate_StepCount verifies that the quota-rotate formula
// has exactly 2 steps: scan then rotate.
func TestFormulaQuotaRotate_StepCount(t *testing.T) {
	dir := t.TempDir()
	data, err := defaultFormulas.ReadFile("formulas/quota-rotate.formula.toml")
	if err != nil {
		t.Fatalf("quota-rotate formula not found in embedded FS: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "quota-rotate.formula.toml"), data, 0o644); err != nil {
		t.Fatalf("writing formula to temp dir: %v", err)
	}

	recipe, err := formula.Compile(context.Background(), "quota-rotate", []string{dir}, nil)
	if err != nil {
		t.Fatalf("Compile quota-rotate: %v", err)
	}

	// Compile adds a root step, so total = root + 2 user steps = 3.
	if len(recipe.Steps) != 3 {
		t.Fatalf("len(Steps) = %d, want 3 (root + 2 user steps)", len(recipe.Steps))
	}

	// Steps[0] is the root; Steps[1] and Steps[2] are the formula steps.
	scanStep := recipe.Steps[1]
	rotateStep := recipe.Steps[2]

	if scanStep.ID != "quota-rotate.scan" {
		t.Errorf("step 1 ID = %q, want %q", scanStep.ID, "quota-rotate.scan")
	}
	if rotateStep.ID != "quota-rotate.rotate" {
		t.Errorf("step 2 ID = %q, want %q", rotateStep.ID, "quota-rotate.rotate")
	}
}
