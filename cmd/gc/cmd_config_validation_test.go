package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestSingletonSessionMigrationWarnings_SkipsNamedBackedTemplates(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1)},
			{Name: "reviewer", Dir: "frontend", MaxActiveSessions: intPtr(1)},
			{Name: "pool", MaxActiveSessions: intPtr(2)},
		},
		NamedSessions: []config.NamedSession{
			{Name: "reviewer", Template: "reviewer", Dir: "frontend"},
		},
	}

	warnings := singletonSessionMigrationWarnings(cfg)
	if len(warnings) != 1 {
		t.Fatalf("warnings = %v, want 1 warning for bare worker singleton", warnings)
	}
	if !strings.Contains(warnings[0], `agent "worker"`) {
		t.Fatalf("warning = %q, want worker guidance", warnings[0])
	}
}

func TestValidateLegacyFormulaConfigRoutes_RejectsTemplateAssignee(t *testing.T) {
	dir := t.TempDir()
	formulasDir := filepath.Join(dir, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(formulasDir, "legacy.formula.toml"), `
formula = "legacy"
version = 2

[[steps]]
id = "work"
title = "Work"
assignee = "worker"
`)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker"}},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulasDir},
		},
	}

	errs := validateLegacyFormulaConfigRoutes(cfg)
	if len(errs) != 1 {
		t.Fatalf("errs = %v, want 1", errs)
	}
	if !strings.Contains(errs[0], `assignee="worker"`) {
		t.Fatalf("err = %q, want assignee guidance", errs[0])
	}
	if !strings.Contains(errs[0], "use metadata.gc.run_target") {
		t.Fatalf("err = %q, want gc.run_target guidance", errs[0])
	}
}

func TestValidateLegacyFormulaConfigRoutes_AllowsNamedSessionAssignee(t *testing.T) {
	dir := t.TempDir()
	formulasDir := filepath.Join(dir, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(formulasDir, "named.formula.toml"), `
formula = "named"
version = 2

[[steps]]
id = "review"
title = "Review"
assignee = "reviewer"
`)

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker"}},
		NamedSessions: []config.NamedSession{
			{Name: "reviewer", Template: "worker"},
		},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulasDir},
		},
	}

	if errs := validateLegacyFormulaConfigRoutes(cfg); len(errs) != 0 {
		t.Fatalf("errs = %v, want none", errs)
	}
}
