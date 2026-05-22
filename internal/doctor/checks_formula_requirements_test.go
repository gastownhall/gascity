package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestFormulaRequirementsCheckOK(t *testing.T) {
	dir := t.TempDir()
	writeDoctorFormula(t, dir, "review", `
formula = "review"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "review"
title = "Review"
`)

	check := NewFormulaRequirementsCheck(&config.City{
		Daemon: config.DaemonConfig{FormulaV2: true},
		FormulaLayers: config.FormulaLayers{
			City: []string{dir},
		},
	}, t.TempDir())

	result := check.Run(&CheckContext{})
	if result.Status != StatusOK {
		t.Fatalf("Status = %v, want OK; details:\n%s", result.Status, strings.Join(result.Details, "\n"))
	}
}

func TestFormulaRequirementsCheckReportsRequirementDiagnosticsAcrossLayers(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := t.TempDir()
	writeDoctorFormula(t, cityDir, "legacy-contract", `
formula = "legacy-contract"
contract = "graph.v2"

[[steps]]
id = "work"
title = "Work"
`)
	writeDoctorFormula(t, cityDir, "missing-requirement", `
formula = "missing-requirement"

[[steps]]
id = "work"
title = "Work"
metadata = { "gc.on_fail" = "abort_scope" }
`)
	writeDoctorFormula(t, cityDir, "disabled-v2", `
formula = "disabled-v2"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "work"
title = "Work"
`)
	writeDoctorFormula(t, cityDir, "unknown-axis", `
formula = "unknown-axis"

[requires]
state_store = ">=2.0.0"

[[steps]]
id = "work"
title = "Work"
`)
	writeDoctorFormula(t, cityDir, "legacy-parent", `
formula = "legacy-parent"

[requires]
formula_compiler = "<2.0.0"

[[steps]]
id = "legacy"
title = "Legacy"
`)
	writeDoctorFormula(t, cityDir, "v2-parent", `
formula = "v2-parent"

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "v2"
title = "V2"
`)
	writeDoctorFormula(t, cityDir, "conflict-child", `
formula = "conflict-child"
extends = ["legacy-parent", "v2-parent"]

[[steps]]
id = "work"
title = "Work"
`)
	writeDoctorFormula(t, rigDir, "invalid-rig", `
formula = "invalid-rig"

[requires]
formula_compiler = "not-a-comparator"

[[steps]]
id = "work"
title = "Work"
`)

	check := NewFormulaRequirementsCheck(&config.City{
		Daemon: config.DaemonConfig{FormulaV2: false},
		FormulaLayers: config.FormulaLayers{
			City: []string{cityDir},
			Rigs: map[string][]string{"proj": {rigDir}},
		},
	}, t.TempDir())

	result := check.Run(&CheckContext{})
	if result.Status != StatusError {
		t.Fatalf("Status = %v, want error; details:\n%s", result.Status, strings.Join(result.Details, "\n"))
	}
	details := strings.Join(result.Details, "\n")
	for _, want := range []string{
		`deprecated contract = "graph.v2"`,
		"graph-only constructs",
		"[daemon] formula_v2 is disabled",
		"formula.requirement_unknown",
		"formula.compiler_requirement_invalid",
		"formula.compiler_requirement_conflict",
		"rig:proj",
	} {
		if !strings.Contains(details, want) {
			t.Fatalf("details missing %q:\n%s", want, details)
		}
	}
	if result.FixHint == "" {
		t.Fatal("FixHint is empty")
	}
}

func writeDoctorFormula(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
