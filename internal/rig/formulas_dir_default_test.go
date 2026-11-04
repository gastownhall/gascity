package rig

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func nextRigFor(t *testing.T, deps Deps, name string, reAdd bool, idx int) config.Rig {
	t.Helper()
	req := ProvisionRequest{Name: name, Path: "/rigs/" + name}
	next, _, _, err := buildNextRigConfig(deps, req, req.Path, "", "", reAdd, reAdd, idx, nil, nil)
	if err != nil {
		t.Fatalf("buildNextRigConfig: %v", err)
	}
	for _, r := range next.Rigs {
		if r.Name == name {
			return r
		}
	}
	t.Fatalf("rig %q missing from next config", name)
	return config.Rig{}
}

func TestBuildNextRigConfigInheritsDefaultFormulasDir(t *testing.T) {
	deps := stubDeps(t.TempDir())
	deps.Cfg = &config.City{Defaults: config.PackDefaults{Rig: config.PackRigDefaults{FormulasDir: "formula-overrides"}}}

	got := nextRigFor(t, deps, "fresh", false, -1)
	if got.FormulasDir != "formula-overrides" {
		t.Fatalf("fresh rig FormulasDir = %q, want %q", got.FormulasDir, "formula-overrides")
	}
}

func TestBuildNextRigConfigNoDefaultFormulasDirLeavesRigUnset(t *testing.T) {
	deps := stubDeps(t.TempDir())

	got := nextRigFor(t, deps, "fresh", false, -1)
	if got.FormulasDir != "" {
		t.Fatalf("fresh rig FormulasDir = %q, want empty", got.FormulasDir)
	}
}

func TestBuildNextRigConfigReAddKeepsExistingFormulasDir(t *testing.T) {
	deps := stubDeps(t.TempDir())
	deps.Cfg = &config.City{
		Defaults: config.PackDefaults{Rig: config.PackRigDefaults{FormulasDir: "formula-overrides"}},
		Rigs:     []config.Rig{{Name: "old", Path: ""}},
	}

	got := nextRigFor(t, deps, "old", true, 0)
	if got.FormulasDir != "" {
		t.Fatalf("re-added rig FormulasDir = %q, want it untouched (empty)", got.FormulasDir)
	}
}
