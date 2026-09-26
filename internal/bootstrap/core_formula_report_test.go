package bootstrap

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/formula"
)

func coreFormulaSearchPaths(t *testing.T) []string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	if root := bazeltest.OverrideRoot(); root != "" {
		return []string{filepath.Join(root, "internal", "bootstrap", "packs", "core", "formulas")}
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
		"--append-notes",
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

var (
	workBeadUpdatePattern = regexp.MustCompile(`bd update "?\$\{?WORK_BEAD_ID\}?"?(\s|$)`)
	// replaceNotesFlagPattern matches the whole --notes flag, so
	// --append-notes does not count.
	replaceNotesFlagPattern = regexp.MustCompile(`(^|\s)--notes(\s|=|$)`)
)

// TestCoreFormulasNeverReplaceWorkBeadNotes guards against core formula steps
// running `bd update "$WORK_BEAD_ID" ... --notes`. --notes replaces the notes
// field and discards notes the work bead already carries (coordinator notes,
// audit lines); work-bead writes must use --append-notes.
func TestCoreFormulasNeverReplaceWorkBeadNotes(t *testing.T) {
	dir := coreFormulaSearchPaths(t)[0]
	paths, err := filepath.Glob(filepath.Join(dir, "*.toml"))
	if err != nil {
		t.Fatalf("glob core formulas: %v", err)
	}
	if len(paths) == 0 {
		t.Fatalf("no core formulas found in %s", dir)
	}
	checked := 0
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, cmd := range joinShellContinuations(string(data)) {
			if !workBeadUpdatePattern.MatchString(cmd) {
				continue
			}
			checked++
			if replaceNotesFlagPattern.MatchString(cmd) {
				t.Errorf("%s: work-bead update must use --append-notes, not --notes (it replaces existing notes):\n%s",
					filepath.Base(path), cmd)
			}
		}
	}
	if checked == 0 {
		t.Fatal(`found no 'bd update "$WORK_BEAD_ID"' commands in core formulas; guard pattern is stale`)
	}
}

// joinShellContinuations folds backslash-continued lines into one logical
// command line so multi-line bd invocations are checked as a whole.
func joinShellContinuations(text string) []string {
	var out []string
	var cur strings.Builder
	for _, line := range strings.Split(text, "\n") {
		trimmed := strings.TrimRight(line, " \t\r")
		if strings.HasSuffix(trimmed, "\\") {
			cur.WriteString(strings.TrimSuffix(trimmed, "\\"))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(line)
		out = append(out, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
