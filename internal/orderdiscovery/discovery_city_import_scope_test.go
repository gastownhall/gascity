package orderdiscovery

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// An order that declares scope = "rig" in a pack imported at CITY scope
// instantiates once per configured rig, the way an agent or named session
// declaring the same in the same pack does (config's
// expandCityImportedAgentsForRigs). Before the fix the declaration was
// validated and then silently dropped: the order registered once,
// city-wide, and resolveOrderStoreTarget routed it to the city store. An
// order that declares nothing keeps its contract: once, city-wide.
func TestScanAllCityImportedRigScopedOrderExpandsPerRig(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	packDir := filepath.Join(t.TempDir(), "review-pack")
	writeOrderDiscoveryFile(t, filepath.Join(packDir, "orders"), "watch", `[order]
scope = "rig"
exec = "scripts/watch.sh"
trigger = "cooldown"
interval = "5m"
`)
	writeOrderDiscoveryFile(t, filepath.Join(packDir, "orders"), "sweep", `[order]
exec = "scripts/sweep.sh"
trigger = "cooldown"
interval = "5m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{City: []string{cityLayer}},
		PackDirs:      []string{packDir},
		Rigs:          []config.Rig{{Name: "alpha"}, {Name: "beta"}},
	}

	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	watch := map[string]int{}
	sweep := map[string]int{}
	for _, a := range aa {
		switch a.Name {
		case "watch":
			watch[a.Rig]++
		case "sweep":
			sweep[a.Rig]++
		}
	}
	if len(watch) != 2 || watch["alpha"] != 1 || watch["beta"] != 1 {
		t.Fatalf("rig-scoped order in a city import registered as %v, want one instance per rig (alpha, beta) and no city copy", watch)
	}
	if len(sweep) != 1 || sweep[""] != 1 {
		t.Fatalf("unscoped order in a city import registered as %v, want once city-wide", sweep)
	}
}

// Importing the same pack at both scopes must not double the rig-scoped
// order: the rig that imports the pack itself keeps the instance from its
// own scan, and only the rigs that do not import it get a fan-out copy.
func TestScanAllCityImportedRigScopedOrderYieldsToRigImport(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	packDir := filepath.Join(t.TempDir(), "review-pack")
	writeOrderDiscoveryFile(t, filepath.Join(packDir, "orders"), "watch", `[order]
scope = "rig"
exec = "scripts/watch.sh"
trigger = "cooldown"
interval = "5m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{"alpha": {cityLayer}},
		},
		PackDirs:    []string{packDir},
		RigPackDirs: map[string][]string{"alpha": {packDir}},
		Rigs:        []config.Rig{{Name: "alpha"}, {Name: "beta"}},
	}

	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	watch := map[string]int{}
	for _, a := range aa {
		if a.Name == "watch" {
			watch[a.Rig]++
		}
	}
	if len(watch) != 2 || watch["alpha"] != 1 || watch["beta"] != 1 {
		t.Fatalf("rig-scoped order imported at both scopes registered as %v, want exactly one instance per rig", watch)
	}
}
