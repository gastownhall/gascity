package orderdiscovery

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestScanAllNilConfigUsesDefaultCityRootsAndOSFS(t *testing.T) {
	cityPath, _ := orderDiscoveryCity(t)
	writeOrderDiscoveryFile(t, filepath.Join(cityPath, "orders"), "heartbeat", `[order]
exec = "scripts/heartbeat.sh"
trigger = "cooldown"
interval = "5m"
`)

	aa, err := ScanAll(cityPath, nil, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	if len(aa) != 1 {
		t.Fatalf("got %d orders, want 1", len(aa))
	}
	if aa[0].Name != "heartbeat" {
		t.Fatalf("Name = %q, want %q", aa[0].Name, "heartbeat")
	}
	if aa[0].Rig != "" {
		t.Fatalf("Rig = %q, want city-scoped order", aa[0].Rig)
	}
}

func TestScanAllScansRigExclusiveLayersInDeterministicRigOrder(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{},
		},
	}

	for _, rigName := range []string{"zeta", "alpha", "beta"} {
		rigLayer := orderDiscoveryRigLayer(t, rigName)
		writeOrderDiscoveryFile(t, filepath.Join(filepath.Dir(rigLayer), "orders"), rigName+"-health", `[order]
exec = "scripts/health.sh"
trigger = "cooldown"
interval = "5m"
`)
		cfg.FormulaLayers.Rigs[rigName] = []string{cityLayer, rigLayer}
	}

	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	got := make([]string, len(aa))
	for i, a := range aa {
		got[i] = a.Rig
	}
	want := []string{"alpha", "beta", "zeta"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("rig order = %v, want %v", got, want)
	}
}

func TestScanAllRigScanHandlerCanSkipFailedRig(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	brokenRigLayer := orderDiscoveryRigLayer(t, "broken")
	writeOrderDiscoveryFile(t, filepath.Join(filepath.Dir(brokenRigLayer), "orders"), "bad", "not toml")
	workingRigLayer := orderDiscoveryRigLayer(t, "working")
	writeOrderDiscoveryFile(t, filepath.Join(filepath.Dir(workingRigLayer), "orders"), "health", `[order]
exec = "scripts/health.sh"
trigger = "cooldown"
interval = "5m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"broken":  {cityLayer, brokenRigLayer},
				"working": {cityLayer, workingRigLayer},
			},
		},
	}

	var skipped []string
	aa, err := ScanAll(cityPath, cfg, ScanOptions{
		OnRigScanError: func(rigName string, _ error) error {
			skipped = append(skipped, rigName)
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	if strings.Join(skipped, ",") != "broken" {
		t.Fatalf("skipped rigs = %v, want [broken]", skipped)
	}
	if len(aa) != 1 {
		t.Fatalf("got %d orders, want 1", len(aa))
	}
	if aa[0].Name != "health" || aa[0].Rig != "working" {
		t.Fatalf("order = %+v, want health scoped to working rig", aa[0])
	}
}

func TestScanAllRigScanHandlerCanAbortFailedRig(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	brokenRigLayer := orderDiscoveryRigLayer(t, "broken")
	writeOrderDiscoveryFile(t, filepath.Join(filepath.Dir(brokenRigLayer), "orders"), "bad", "not toml")
	handlerErr := errors.New("stop scanning rigs")

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"broken": {cityLayer, brokenRigLayer},
			},
		},
	}

	_, err := ScanAll(cityPath, cfg, ScanOptions{
		OnRigScanError: func(_ string, _ error) error {
			return handlerErr
		},
	})
	if !errors.Is(err, handlerErr) {
		t.Fatalf("ScanAll error = %v, want handler error", err)
	}
}

func TestScanAllDefaultRigScanErrorPropagatesWithRigName(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	brokenRigLayer := orderDiscoveryRigLayer(t, "broken")
	writeOrderDiscoveryFile(t, filepath.Join(filepath.Dir(brokenRigLayer), "orders"), "bad", "not toml")

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"broken": {cityLayer, brokenRigLayer},
			},
		},
	}

	_, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err == nil {
		t.Fatal("ScanAll succeeded; want rig scan error")
	}
	if !strings.Contains(err.Error(), "rig broken:") {
		t.Fatalf("ScanAll error = %q, want rig name context", err.Error())
	}
}

func TestScanAllOverrideHandlerCanReturnPartiallyModifiedOrders(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	writeOrderDiscoveryFile(t, filepath.Join(cityPath, "orders"), "backup", `[order]
exec = "scripts/backup.sh"
trigger = "cooldown"
interval = "1h"
`)

	interval := "15m"
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
		},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{
				{Name: "backup", Interval: &interval},
				{Name: "missing"},
			},
		},
	}

	var handled string
	aa, err := ScanAll(cityPath, cfg, ScanOptions{
		OnOverrideError: func(err error) error {
			handled = err.Error()
			return nil
		},
	})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	if !strings.Contains(handled, `order "missing" not found`) {
		t.Fatalf("handled override error = %q, want missing-order error", handled)
	}
	if len(aa) != 1 {
		t.Fatalf("got %d orders, want 1", len(aa))
	}
	if aa[0].Interval != "15m" {
		t.Fatalf("Interval = %q, want partially applied override %q", aa[0].Interval, "15m")
	}
}

func TestScanAllOverrideHandlerCanAbortInvalidOverride(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	writeOrderDiscoveryFile(t, filepath.Join(cityPath, "orders"), "backup", `[order]
exec = "scripts/backup.sh"
trigger = "cooldown"
interval = "1h"
`)
	handlerErr := errors.New("stop applying overrides")

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
		},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{{Name: "missing"}},
		},
	}

	_, err := ScanAll(cityPath, cfg, ScanOptions{
		OnOverrideError: func(error) error {
			return handlerErr
		},
	})
	if !errors.Is(err, handlerErr) {
		t.Fatalf("ScanAll error = %v, want handler error", err)
	}
}

func TestScanAllRejectsSchema1PackLegacyOrderDirectory(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte("[pack]\nname = \"legacy-city\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	legacyOrderDir := filepath.Join(cityPath, "orders", "heartbeat")
	if err := os.MkdirAll(legacyOrderDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacyOrderDir, "order.toml"), []byte(`[order]
exec = "scripts/heartbeat.sh"
trigger = "cooldown"
interval = "5m"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := ScanAll(cityPath, &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
		},
	}, ScanOptions{})
	if err == nil {
		t.Fatal("ScanAll succeeded, want schema-1 legacy order directory rejection")
	}
	if !strings.Contains(err.Error(), "rename to orders/heartbeat.toml") {
		t.Fatalf("ScanAll error = %v, want schema-1 flat-file migration guidance", err)
	}
}

func TestCityOrderRootsUsesLocalAndPackFormulaLayersOnce(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	packLayer := filepath.Join(t.TempDir(), "formulas")

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer, packLayer, cityLayer},
		},
	}

	roots := CityOrderRoots(cityPath, cfg)
	if len(roots) != 2 {
		t.Fatalf("got %d roots, want 2: %#v", len(roots), roots)
	}
	if roots[0].Dir != filepath.Join(filepath.Dir(packLayer), "orders") || roots[0].FormulaLayer != packLayer {
		t.Fatalf("first root = %+v, want pack orders root", roots[0])
	}
	if roots[1].Dir != filepath.Join(cityPath, "orders") || roots[1].FormulaLayer != cityLayer {
		t.Fatalf("second root = %+v, want city orders root", roots[1])
	}
}

func TestScanAllCityLocalOrderOverridesOrdersOnlyPack(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	packDir := filepath.Join(t.TempDir(), "audit-pack")
	writeOrderDiscoveryFile(t, filepath.Join(packDir, "orders"), "audit", `[order]
exec = "scripts/pack.sh"
trigger = "cooldown"
interval = "5m"
`)
	writeOrderDiscoveryFile(t, filepath.Join(cityPath, "orders"), "audit", `[order]
exec = "scripts/city.sh"
trigger = "cooldown"
interval = "5m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
		},
		PackDirs: []string{packDir},
	}

	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	if len(aa) != 1 {
		t.Fatalf("got %d orders, want 1: %#v", len(aa), aa)
	}
	if aa[0].Exec != "scripts/city.sh" {
		t.Fatalf("Exec = %q, want city-local order to win", aa[0].Exec)
	}
}

func TestScanAllRigLocalOrderOverridesOrdersOnlyPack(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	rigLayer := orderDiscoveryRigLayer(t, "frontend")
	packDir := filepath.Join(t.TempDir(), "watch-pack")
	writeOrderDiscoveryFile(t, filepath.Join(packDir, "orders"), "watch", `[order]
exec = "scripts/pack.sh"
trigger = "cooldown"
interval = "5m"
`)
	writeOrderDiscoveryFile(t, filepath.Join(filepath.Dir(rigLayer), "orders"), "watch", `[order]
exec = "scripts/rig.sh"
trigger = "cooldown"
interval = "5m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"frontend": {cityLayer, rigLayer},
			},
		},
		Rigs: []config.Rig{
			{Name: "frontend", FormulasDir: rigLayer},
		},
		RigPackDirs: map[string][]string{
			"frontend": {packDir},
		},
	}

	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	for _, a := range aa {
		if a.Name != "watch" || a.Rig != "frontend" {
			continue
		}
		if a.Exec != "scripts/rig.sh" {
			t.Fatalf("Exec = %q, want rig-local order to win", a.Exec)
		}
		return
	}
	t.Fatalf("missing rig-scoped watch order in %#v", aa)
}

func TestScanAllSameOrdersOnlyPackImportedAtCityAndRigScopes(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	packDir := filepath.Join(t.TempDir(), "shared-pack")
	writeOrderDiscoveryFile(t, filepath.Join(packDir, "orders"), "sweep", `[order]
exec = "scripts/sweep.sh"
trigger = "cooldown"
interval = "5m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"frontend": {cityLayer},
			},
		},
		PackDirs: []string{packDir},
		RigPackDirs: map[string][]string{
			"frontend": {packDir},
		},
	}

	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	var cityFound, rigFound bool
	for _, a := range aa {
		if a.Name != "sweep" {
			continue
		}
		switch a.Rig {
		case "":
			cityFound = true
		case "frontend":
			rigFound = true
		}
	}
	if !cityFound || !rigFound {
		t.Fatalf("found city=%v rig=%v in %#v, want both city and rig orders", cityFound, rigFound, aa)
	}
}

func TestScanAllCityPackRootsPreserveTopoOrderAcrossOrdersOnlyPacks(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	packA, layerA := orderDiscoveryPackLayer(t, "pack-a")
	packB := filepath.Join(t.TempDir(), "pack-b")
	packC, layerC := orderDiscoveryPackLayer(t, "pack-c")
	writeOrderDiscoveryFile(t, filepath.Join(packA, "orders"), "audit", `[order]
exec = "scripts/a.sh"
trigger = "cooldown"
interval = "5m"
`)
	writeOrderDiscoveryFile(t, filepath.Join(packB, "orders"), "audit", `[order]
exec = "scripts/b.sh"
trigger = "cooldown"
interval = "5m"
`)
	writeOrderDiscoveryFile(t, filepath.Join(packC, "orders"), "audit", `[order]
exec = "scripts/c.sh"
trigger = "cooldown"
interval = "5m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{layerA, layerC, cityLayer},
		},
		PackDirs: []string{packA, packB, packC},
	}

	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	if len(aa) != 1 {
		t.Fatalf("got %d orders, want 1: %#v", len(aa), aa)
	}
	if aa[0].Exec != "scripts/c.sh" {
		t.Fatalf("Exec = %q, want later formula pack to override earlier orders-only pack", aa[0].Exec)
	}
}

func TestScanAllRigPackRootsPreserveTopoOrderAcrossOrdersOnlyPacks(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	packA, layerA := orderDiscoveryPackLayer(t, "rig-pack-a")
	packB := filepath.Join(t.TempDir(), "rig-pack-b")
	packC, layerC := orderDiscoveryPackLayer(t, "rig-pack-c")
	writeOrderDiscoveryFile(t, filepath.Join(packA, "orders"), "watch", `[order]
exec = "scripts/a.sh"
trigger = "cooldown"
interval = "5m"
`)
	writeOrderDiscoveryFile(t, filepath.Join(packB, "orders"), "watch", `[order]
exec = "scripts/b.sh"
trigger = "cooldown"
interval = "5m"
`)
	writeOrderDiscoveryFile(t, filepath.Join(packC, "orders"), "watch", `[order]
exec = "scripts/c.sh"
trigger = "cooldown"
interval = "5m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"frontend": {cityLayer, layerA, layerC},
			},
		},
		RigPackDirs: map[string][]string{
			"frontend": {packA, packB, packC},
		},
	}

	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	for _, a := range aa {
		if a.Name != "watch" || a.Rig != "frontend" {
			continue
		}
		if a.Exec != "scripts/c.sh" {
			t.Fatalf("Exec = %q, want later rig formula pack to override earlier orders-only pack", a.Exec)
		}
		return
	}
	t.Fatalf("missing rig-scoped watch order in %#v", aa)
}

func TestScanAllRigLocalOrderUsesCanonicalFormulaLayer(t *testing.T) {
	cityPath, cityLayer := orderDiscoveryCity(t)
	rigLayer := orderDiscoveryRigLayer(t, "frontend")
	packDir := filepath.Join(t.TempDir(), "watch-pack")
	writeOrderDiscoveryFile(t, filepath.Join(packDir, "orders"), "watch", `[order]
exec = "scripts/pack.sh"
trigger = "cooldown"
interval = "5m"
`)
	writeOrderDiscoveryFile(t, filepath.Join(filepath.Dir(rigLayer), "orders"), "watch", `[order]
exec = "scripts/rig.sh"
trigger = "cooldown"
interval = "5m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityLayer},
			Rigs: map[string][]string{
				"frontend": {cityLayer, rigLayer},
			},
		},
		RigPackDirs: map[string][]string{
			"frontend": {packDir},
		},
	}

	aa, err := ScanAll(cityPath, cfg, ScanOptions{})
	if err != nil {
		t.Fatalf("ScanAll returned error: %v", err)
	}
	for _, a := range aa {
		if a.Name != "watch" || a.Rig != "frontend" {
			continue
		}
		if a.Exec != "scripts/rig.sh" {
			t.Fatalf("Exec = %q, want canonical rig-local formula layer to win", a.Exec)
		}
		return
	}
	t.Fatalf("missing rig-scoped watch order in %#v", aa)
}

func TestRigExclusiveLayersReturnsOnlyRigSuffix(t *testing.T) {
	cityLayers := []string{"/city/base", "/city/local"}
	rigLayers := []string{"/city/base", "/city/local", "/rig/base", "/rig/local"}

	got := RigExclusiveLayers(rigLayers, cityLayers)
	want := []string{"/rig/base", "/rig/local"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("RigExclusiveLayers = %v, want %v", got, want)
	}

	if got := RigExclusiveLayers(cityLayers, cityLayers); got != nil {
		t.Fatalf("RigExclusiveLayers for inherited-only rig = %v, want nil", got)
	}
}

func orderDiscoveryCity(t *testing.T) (cityPath, cityLayer string) {
	t.Helper()
	cityPath = t.TempDir()
	cityLayer = filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(cityLayer, 0o755); err != nil {
		t.Fatal(err)
	}
	return cityPath, cityLayer
}

func orderDiscoveryRigLayer(t *testing.T, rigName string) string {
	t.Helper()
	rigRoot := filepath.Join(t.TempDir(), rigName)
	rigLayer := filepath.Join(rigRoot, "formulas")
	if err := os.MkdirAll(rigLayer, 0o755); err != nil {
		t.Fatal(err)
	}
	return rigLayer
}

func orderDiscoveryPackLayer(t *testing.T, packName string) (packDir, layer string) {
	t.Helper()
	packDir = filepath.Join(t.TempDir(), packName)
	layer = filepath.Join(packDir, "formulas")
	if err := os.MkdirAll(layer, 0o755); err != nil {
		t.Fatal(err)
	}
	return packDir, layer
}

func writeOrderDiscoveryFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
