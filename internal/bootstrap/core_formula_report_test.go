package bootstrap

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/molecule"
)

func coreFormulaSearchPaths(t *testing.T) []string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return []string{filepath.Join(filepath.Dir(filename), "packs", "core", "formulas")}
}

func cookCorePolecatReport(t *testing.T) (*beads.MemStore, *molecule.Result) {
	t.Helper()
	store := beads.NewMemStore()
	result, err := molecule.Cook(context.Background(), store, "mol-polecat-report", coreFormulaSearchPaths(t), molecule.Options{
		Vars: map[string]string{
			"issue":       "ga-source",
			"base_branch": "main",
		},
	})
	if err != nil {
		t.Fatalf("molecule.Cook mol-polecat-report: %v", err)
	}
	return store, result
}

func TestCoreMolPolecatReportCookCreatesWriteReportTerminalStep(t *testing.T) {
	store, result := cookCorePolecatReport(t)

	if result.RootID == "" {
		t.Fatal("RootID is empty")
	}
	root, err := store.Get(result.RootID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if root.Ref != "mol-polecat-report" {
		t.Fatalf("root Ref = %q, want mol-polecat-report", root.Ref)
	}

	writeReportID := result.IDMapping["mol-polecat-report.write-report"]
	if writeReportID == "" {
		t.Fatalf("IDMapping missing mol-polecat-report.write-report: %#v", result.IDMapping)
	}
	writeReport, err := store.Get(writeReportID)
	if err != nil {
		t.Fatalf("get write-report: %v", err)
	}
	if writeReport.ParentID != result.RootID {
		t.Fatalf("write-report ParentID = %q, want root %q", writeReport.ParentID, result.RootID)
	}
	if writeReport.Metadata["gc.step_ref"] != "mol-polecat-report.write-report" {
		t.Fatalf("write-report gc.step_ref = %q, want mol-polecat-report.write-report", writeReport.Metadata["gc.step_ref"])
	}

	dependents, err := store.DepList(writeReportID, "up")
	if err != nil {
		t.Fatalf("DepList write-report up: %v", err)
	}
	if len(dependents) != 0 {
		t.Fatalf("write-report should be terminal, but dependents = %+v", dependents)
	}
}

func TestCoreMolPolecatReportWriteReportStepRecordsNoteAndAvoidsPRFlow(t *testing.T) {
	store, result := cookCorePolecatReport(t)
	writeReportID := result.IDMapping["mol-polecat-report.write-report"]
	writeReport, err := store.Get(writeReportID)
	if err != nil {
		t.Fatalf("get write-report: %v", err)
	}

	description := writeReport.Description
	for _, want := range []string{
		"ga-source",
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
	} {
		if strings.Contains(lowerDescription, forbidden) {
			t.Fatalf("write-report description must not contain %q:\n%s", forbidden, description)
		}
	}
}
