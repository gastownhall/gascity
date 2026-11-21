package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestFormulaCookRigScopeMaterializesNamedSessionInTheCityStore is AC-6 for
// `gc formula cook`: a rig-scoped graph.v2 cook whose step names an on-demand
// configured named session binds it in the city's store, never in the rig
// store the cook opened for its workflow (#8436).
func TestFormulaCookRigScopeMaterializesNamedSessionInTheCityStore(t *testing.T) {
	cityDir := oneShotCookCity(t)
	rigDir := filepath.Join(cityDir, "wf")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("mkdir rig: %v", err)
	}
	cityTOML := filepath.Join(cityDir, "city.toml")
	declared, err := os.ReadFile(cityTOML)
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	declared = append(declared, []byte(testControlDispatcherAgentTOML("wf")+`
[[rigs]]
name = "wf"

[[agent]]
name = "refinery"
dir = "wf"
start_command = "true"
max_active_sessions = 1

[[named_session]]
template = "refinery"
dir = "wf"
mode = "on_demand"
`)...)
	if err := os.WriteFile(cityTOML, declared, 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	writeCatalogFile(t, cityDir, ".gc/site.toml", fmt.Sprintf("[[rig]]\nname = \"wf\"\npath = %q\n", rigDir))
	if err := os.WriteFile(filepath.Join(cityDir, "formulas", "refine.formula.toml"), []byte(`
formula = "refine"
version = 2
contract = "graph.v2"

[[steps]]
id = "work"
title = "Refine"
assignee = "wf/refinery"
`), 0o644); err != nil {
		t.Fatalf("write formula: %v", err)
	}
	// Scoped file-store roots, so the rig has a ledger of its own rather than
	// aliasing the city's.
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	for _, root := range []string{cityDir, rigDir} {
		if err := ensurePersistedScopeLocalFileStore(root); err != nil {
			t.Fatalf("ensurePersistedScopeLocalFileStore(%s): %v", root, err)
		}
	}
	restoreFlag(t, &rigFlag)
	restoreFlag(t, &cityFlag)
	rigFlag = "wf"
	var opened []*closeStoreSpy
	prevOpen := slingOpenCityStore
	slingOpenCityStore = func(path string) (beads.Store, error) {
		store, err := prevOpen(path)
		if err != nil {
			return nil, err
		}
		spy := &closeStoreSpy{Store: store}
		opened = append(opened, spy)
		return spy, nil
	}
	t.Cleanup(func() { slingOpenCityStore = prevOpen })

	res := cookFormula(t, "refine")
	if len(opened) != 1 || opened[0].closeCount() != 1 {
		closes := -1
		if len(opened) == 1 {
			closes = opened[0].closeCount()
		}
		t.Errorf("the cook opened the city store %d time(s) (closes %d), want it opened once and closed once on return", len(opened), closes)
	}

	rig, err := openStoreAtForCity(rigDir, cityDir)
	if err != nil {
		t.Fatalf("open rig store: %v", err)
	}
	city, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	if _, err := rig.Get(res.RootID); err != nil {
		t.Fatalf("cooked root %s is not in the rig store: %v", res.RootID, err)
	}
	if n := countSessionBeads(t, rig); n != 0 {
		t.Errorf("the rig workflow store holds %d session bead(s); the cook materialized the named session where no session reader reads", n)
	}
	if n := countSessionBeads(t, city); n != 1 {
		t.Errorf("the city store holds %d session bead(s), want the one named session", n)
	}
}
