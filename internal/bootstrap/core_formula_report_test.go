package bootstrap

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

func coreFormulaSearchPaths(t *testing.T) []string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return []string{filepath.Join(filepath.Dir(filename), "packs", "core", "formulas")}
}

func compileCorePolecatReport(t *testing.T) *formula.Recipe {
	t.Helper()
	prev := formula.IsFormulaV2Enabled()
	formula.SetFormulaV2Enabled(true)
	t.Cleanup(func() { formula.SetFormulaV2Enabled(prev) })

	recipe, err := formula.Compile(context.Background(), "mol-polecat-report", coreFormulaSearchPaths(t), map[string]string{
		"convoy_id":   "gc-convoy",
		"base_branch": "main",
	})
	if err != nil {
		t.Fatalf("compile mol-polecat-report: %v", err)
	}
	return recipe
}

func TestCoreMolPolecatReportCompilesWriteReportTerminalStep(t *testing.T) {
	recipe := compileCorePolecatReport(t)

	root := recipe.RootStep()
	if root == nil {
		t.Fatal("root step missing")
	}
	if got := root.Metadata["gc.formula_contract"]; got != "graph.v2" {
		t.Fatalf("root gc.formula_contract = %q, want graph.v2", got)
	}

	writeReport := recipe.StepByID("mol-polecat-report.write-report")
	if writeReport == nil {
		t.Fatal("recipe missing mol-polecat-report.write-report step")
	}

	// write-report must be terminal: only the synthetic graph.v2
	// workflow-finalize step may depend on it.
	for _, dep := range recipe.Deps {
		if dep.DependsOnID != writeReport.ID || dep.Type != "blocks" {
			continue
		}
		if dep.StepID == "mol-polecat-report.workflow-finalize" {
			continue
		}
		t.Fatalf("write-report should be terminal, but %s depends on it", dep.StepID)
	}
}

func TestCoreMolPolecatReportWriteReportStepRecordsNoteAndAvoidsPRFlow(t *testing.T) {
	recipe := compileCorePolecatReport(t)
	writeReport := recipe.StepByID("mol-polecat-report.write-report")
	if writeReport == nil {
		t.Fatal("recipe missing mol-polecat-report.write-report step")
	}

	description := writeReport.Description
	for _, want := range []string{
		"gc convoy status {{convoy_id}}",
		"WORK_BEAD_ID",
		"bd update",
		"--notes",
		"bd close",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("write-report description missing %q:\n%s", want, description)
		}
	}

	lowerDescription := strings.ToLower(description)
	for _, forbidden := range []string{
		"gh pr create",
		"git push",
		"{{issue}}",
	} {
		if strings.Contains(lowerDescription, forbidden) {
			t.Fatalf("write-report description must not contain %q:\n%s", forbidden, description)
		}
	}
}
