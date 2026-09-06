package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

func TestQualityGateFallbackInFormulas(t *testing.T) {
	searchPaths := []string{"../../internal/bootstrap/packs/core/formulas/"}

	recipe, err := formula.Compile(context.Background(), "mol-polecat-base", searchPaths, nil)
	if err != nil {
		t.Fatalf("compile mol-polecat-base: %v", err)
	}

	emptyVars := map[string]string{
		"setup_command": "", "typecheck_command": "", "lint_command": "",
		"build_command": "", "test_command": "", "base_branch": "main",
		"issue": "test-123",
	}
	explicitVars := map[string]string{
		"setup_command": "pnpm install", "typecheck_command": "tsc --noEmit",
		"lint_command": "eslint .", "build_command": "pnpm build",
		"test_command": "pnpm test", "base_branch": "main", "issue": "test-123",
	}

	t.Run("preflight-tests fallback when commands empty", func(t *testing.T) {
		step := findStep(recipe.Steps, "mol-polecat-base.preflight-tests")
		if step == nil {
			t.Fatal("preflight-tests step not found")
		}
		rendered := formula.Substitute(step.Description, emptyVars)
		if !strings.Contains(rendered, "CLAUDE.md or AGENTS.md") {
			t.Error("fallback text missing when all commands are empty")
		}
	})

	t.Run("self-review fallback when commands empty", func(t *testing.T) {
		step := findStep(recipe.Steps, "mol-polecat-base.self-review")
		if step == nil {
			t.Fatal("self-review step not found")
		}
		rendered := formula.Substitute(step.Description, emptyVars)
		if !strings.Contains(rendered, "CLAUDE.md or AGENTS.md") {
			t.Error("fallback text missing when all commands are empty")
		}
	})

	t.Run("explicit vars render in self-review", func(t *testing.T) {
		step := findStep(recipe.Steps, "mol-polecat-base.self-review")
		if step == nil {
			t.Fatal("self-review step not found")
		}
		rendered := formula.Substitute(step.Description, explicitVars)
		if !strings.Contains(rendered, "pnpm test") {
			t.Error("explicit test_command not rendered")
		}
		if !strings.Contains(rendered, "eslint .") {
			t.Error("explicit lint_command not rendered")
		}
	})
}

func findStep(steps []formula.RecipeStep, id string) *formula.RecipeStep {
	for i := range steps {
		if steps[i].ID == id {
			return &steps[i]
		}
	}
	return nil
}
