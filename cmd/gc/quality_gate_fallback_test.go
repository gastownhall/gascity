package main

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

func TestQualityGateFallbackInFormulas(t *testing.T) {
	searchPaths := []string{"formulas/"}

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

func TestQualityGateFallbackInRefineryPatrol(t *testing.T) {
	searchPaths := []string{
		"../../examples/gastown/packs/gastown/formulas/",
		"formulas/",
	}

	recipe, err := formula.Compile(context.Background(), "mol-refinery-patrol", searchPaths, nil)
	if err != nil {
		t.Fatalf("compile mol-refinery-patrol: %v", err)
	}

	// Find the run-tests step
	var runTestsStep *formula.RecipeStep
	for i := range recipe.Steps {
		if strings.HasSuffix(recipe.Steps[i].ID, "run-tests") {
			runTestsStep = &recipe.Steps[i]
			break
		}
	}
	if runTestsStep == nil {
		t.Fatal("run-tests step not found")
	}

	t.Run("fallback when commands empty", func(t *testing.T) {
		rendered := formula.Substitute(runTestsStep.Description, map[string]string{
			"setup_command": "", "typecheck_command": "", "lint_command": "",
			"build_command": "", "test_command": "", "run_tests": "true",
			"target_branch": "main",
		})
		if !strings.Contains(rendered, "CLAUDE.md or AGENTS.md") {
			t.Error("fallback text missing when all commands are empty")
		}
	})

	t.Run("explicit vars still render", func(t *testing.T) {
		rendered := formula.Substitute(runTestsStep.Description, map[string]string{
			"setup_command": "", "typecheck_command": "",
			"lint_command": "golangci-lint run", "build_command": "go build ./...",
			"test_command": "go test ./...", "run_tests": "true",
			"target_branch": "main",
		})
		if !strings.Contains(rendered, "go test ./...") {
			t.Error("explicit test_command not rendered")
		}
		if !strings.Contains(rendered, "golangci-lint run") {
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
