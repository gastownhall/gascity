package main

import (
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

func coreFormulaDir(t *testing.T) string {
	t.Helper()
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..", "internal", "bootstrap", "packs", "core", "formulas")
}

// TestPolecatReportFormulaParsesAndHasNoGHPRCreate verifies that the
// mol-polecat-report formula parses without error, contains a write-report
// terminal step, and never instructs the agent to call gh pr create or
// git push in any step description.
func TestPolecatReportFormulaParsesAndHasNoGHPRCreate(t *testing.T) {
	dir := coreFormulaDir(t)
	path := filepath.Join(dir, "mol-polecat-report.toml")

	parser := formula.NewParser(dir)
	f, err := parser.ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile mol-polecat-report.toml: %v", err)
	}

	var hasWriteReport, hasWriteNotes, hasClose, hasDrainAck, hasImplement bool
	for _, step := range f.Steps {
		if strings.Contains(step.Description, "gh pr create") {
			t.Errorf("step %q must not invoke 'gh pr create'", step.ID)
		}
		if strings.Contains(step.Description, "git push") {
			t.Errorf("step %q must not invoke 'git push'", step.ID)
		}
		if strings.Contains(step.Description, "git checkout -- .") {
			t.Errorf("step %q must not run destructive 'git checkout -- .' in a shared checkout", step.ID)
		}
		if step.ID == "implement" {
			hasImplement = true
			desc := strings.ToLower(step.Description)
			if !strings.Contains(desc, "no code changes") && !strings.Contains(desc, "analysis only") {
				t.Errorf("implement step must forbid code changes (expected 'no code changes' or 'analysis only'), got: %q", step.Description)
			}
		}
		if step.ID == "write-report" {
			hasWriteReport = true
			if strings.Contains(step.Description, "bd update {{issue}} --notes") {
				hasWriteNotes = true
			}
			if strings.Contains(step.Description, "bd close {{issue}}") {
				hasClose = true
			}
			if strings.Contains(step.Description, "gc runtime drain-ack") {
				hasDrainAck = true
			}
		}
	}
	if !hasImplement {
		t.Error("mol-polecat-report formula missing 'implement' step override")
	}
	if !hasWriteReport {
		t.Error("mol-polecat-report formula missing 'write-report' step")
	}
	if !hasWriteNotes {
		t.Error("write-report step must write findings with 'bd update {{issue}} --notes'")
	}
	if !hasClose {
		t.Error("write-report step must close the bead with 'bd close {{issue}}'")
	}
	if !hasDrainAck {
		t.Error("write-report step must signal the reconciler with 'gc runtime drain-ack'")
	}
}
