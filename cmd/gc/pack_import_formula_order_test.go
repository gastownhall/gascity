package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

func TestPackV2ImportedFormulasAndOrdersVisibleToCityAndRig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	opsPackDir := filepath.Join(cityDir, "packs", "ops")
	sidecarPackDir := filepath.Join(cityDir, "packs", "sidecar")

	for _, dir := range []string{
		filepath.Join(cityDir, ".gc"),
		rigDir,
		filepath.Join(opsPackDir, "formulas"),
		filepath.Join(opsPackDir, "orders"),
		filepath.Join(sidecarPackDir, "formulas"),
		filepath.Join(sidecarPackDir, "orders"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeFile(t, filepath.Join(cityDir, "pack.toml"), `
[pack]
name = "testcity"
schema = 2

[imports.ops]
source = "./packs/ops"
`)
	writeFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]

[[rigs]]
name = "frontend"

[rigs.imports.sidecar]
source = "./packs/sidecar"
`)
	writeFile(t, filepath.Join(cityDir, ".gc", "site.toml"), `
workspace_name = "testcity"

[[rig]]
name = "frontend"
path = "./frontend"
`)
	writeFile(t, filepath.Join(opsPackDir, "pack.toml"), `
[pack]
name = "ops"
schema = 2
`)
	writeFile(t, filepath.Join(opsPackDir, "formulas", "city-visible.toml"), `
formula = "city-visible"
`)
	writeFile(t, filepath.Join(opsPackDir, "orders", "city-order.toml"), `
[order]
formula = "city-visible"
gate = "manual"
pool = "ops.assist"
`)
	writeFile(t, filepath.Join(sidecarPackDir, "pack.toml"), `
[pack]
name = "sidecar"
schema = 2
`)
	writeFile(t, filepath.Join(sidecarPackDir, "formulas", "rig-visible.toml"), `
formula = "rig-visible"
`)
	writeFile(t, filepath.Join(sidecarPackDir, "orders", "rig-order.toml"), `
[order]
formula = "rig-visible"
gate = "manual"
pool = "sidecar.watcher"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	opsFormulaDir := filepath.Join(opsPackDir, "formulas")
	sidecarFormulaDir := filepath.Join(sidecarPackDir, "formulas")
	assertContainsString(t, cfg.FormulaLayers.City, opsFormulaDir)
	assertNotContainsString(t, cfg.FormulaLayers.City, sidecarFormulaDir)

	frontendLayers := cfg.FormulaLayers.SearchPaths("frontend")
	assertContainsString(t, frontendLayers, opsFormulaDir)
	assertContainsString(t, frontendLayers, sidecarFormulaDir)

	var stderr bytes.Buffer
	discovered, err := scanAllOrders(cityDir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v; stderr: %s", err, stderr.String())
	}
	assertOrderScope(t, discovered, "city-order", "")
	assertOrderScope(t, discovered, "rig-order", "frontend")

	if err := ResolveFormulas(cityDir, cfg.FormulaLayers.City); err != nil {
		t.Fatalf("ResolveFormulas(city): %v", err)
	}
	assertSymlinkExists(t, filepath.Join(cityDir, ".beads", "formulas", "city-visible.toml"))
	assertPathAbsent(t, filepath.Join(cityDir, ".beads", "formulas", "rig-visible.toml"))

	if err := ResolveFormulas(rigDir, frontendLayers); err != nil {
		t.Fatalf("ResolveFormulas(rig): %v", err)
	}
	assertSymlinkExists(t, filepath.Join(rigDir, ".beads", "formulas", "city-visible.toml"))
	assertSymlinkExists(t, filepath.Join(rigDir, ".beads", "formulas", "rig-visible.toml"))
}

func TestTransitiveGastownPackDigestOrderResolvesAndRuns(t *testing.T) {
	cityDir := t.TempDir()
	wrapperPackDir := filepath.Join(cityDir, "packs", "wrapper")
	if err := os.MkdirAll(wrapperPackDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	gastownRoot, err := filepath.Abs(filepath.Join("..", "..", "examples", "gastown"))
	if err != nil {
		t.Fatalf("Abs(examples/gastown): %v", err)
	}
	gastownPackDir := filepath.Join(gastownRoot, "packs", "gastown")
	retiredMaintenanceFormulaLayer := filepath.Join(gastownRoot, "packs", "maintenance", "formulas")
	digestFormulaLayer := filepath.Join(gastownPackDir, "formulas")
	digestFormulaFile := filepath.Join(digestFormulaLayer, "mol-digest-generate.toml")
	shutdownFormulaFile := filepath.Join(digestFormulaLayer, "mol-shutdown-dance.toml")

	writeFile(t, filepath.Join(cityDir, "city.toml"), `
[daemon]
formula_v2 = true
`)
	writeFile(t, filepath.Join(cityDir, ".gc", "site.toml"), `
workspace_name = "wrapper-city"
`)
	writeFile(t, filepath.Join(cityDir, "pack.toml"), `
[pack]
name = "wrapper-city"
schema = 2

[imports.wrapper]
source = "./packs/wrapper"
`)
	writeFile(t, filepath.Join(wrapperPackDir, "pack.toml"), `
[pack]
name = "wrapper"
schema = 2

[imports.gastown]
source = "`+gastownPackDir+`"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	assertNotContainsString(t, cfg.FormulaLayers.City, retiredMaintenanceFormulaLayer)
	assertContainsString(t, cfg.FormulaLayers.City, digestFormulaLayer)
	assertAgentQualifiedName(t, cfg.Agents, "wrapper.dog")

	var stderr bytes.Buffer
	discovered, err := scanAllOrders(cityDir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v; stderr: %s", err, stderr.String())
	}
	digest, ok := findOrder(discovered, "digest-generate", "")
	if !ok {
		t.Fatalf("missing digest-generate order in %#v", discovered)
	}
	if digest.Source != filepath.Join(gastownPackDir, "orders", "digest-generate.toml") {
		t.Fatalf("digest source = %q, want nested gastown order", digest.Source)
	}
	if digest.FormulaLayer != digestFormulaLayer {
		t.Fatalf("digest FormulaLayer = %q, want %q", digest.FormulaLayer, digestFormulaLayer)
	}
	if digest.Formula != "mol-digest-generate" {
		t.Fatalf("digest Formula = %q, want mol-digest-generate", digest.Formula)
	}
	if digest.Pool != "dog" {
		t.Fatalf("digest Pool = %q, want portable bare dog", digest.Pool)
	}
	resolvedPool, err := qualifyOrderPool(digest, cfg)
	if err != nil {
		t.Fatalf("qualifyOrderPool(digest-generate): %v", err)
	}
	if resolvedPool != "wrapper.dog" {
		t.Fatalf("qualifyOrderPool(digest-generate) = %q, want wrapper.dog", resolvedPool)
	}
	if config.FindAgent(cfg, resolvedPool) == nil {
		t.Fatalf("resolved pool %q does not match a configured agent", resolvedPool)
	}

	if err := ResolveFormulas(cityDir, cfg.FormulaLayers.City); err != nil {
		t.Fatalf("ResolveFormulas(city): %v", err)
	}
	assertSymlinkTarget(t, filepath.Join(cityDir, ".beads", "formulas", "mol-digest-generate.toml"), digestFormulaFile)
	assertSymlinkTarget(t, filepath.Join(cityDir, ".beads", "formulas", "mol-shutdown-dance.toml"), shutdownFormulaFile)

	store := beads.NewMemStore()
	var stdout bytes.Buffer
	stderr.Reset()
	code := doOrderRun(discovered, "digest-generate", "", cityDir, store, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0; stdout: %s stderr: %s", code, stdout.String(), stderr.String())
	}
	runs, err := store.ListByLabel("order-run:digest-generate", 0, beads.WithBothTiers)
	if err != nil {
		t.Fatalf("store.ListByLabel(order-run:digest-generate): %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("order run count = %d, want 1 (%#v)", len(runs), runs)
	}
	if got := runs[0].Metadata["gc.routed_to"]; got != "wrapper.dog" {
		t.Fatalf("gc.routed_to = %q, want wrapper.dog", got)
	}
}

func assertContainsString(t *testing.T, got []string, want string) {
	t.Helper()
	for _, item := range got {
		if item == want {
			return
		}
	}
	t.Fatalf("%#v does not contain %q", got, want)
}

func assertNotContainsString(t *testing.T, got []string, want string) {
	t.Helper()
	for _, item := range got {
		if item == want {
			t.Fatalf("%#v contains %q", got, want)
		}
	}
}

func assertOrderScope(t *testing.T, got []orders.Order, name, rig string) {
	t.Helper()
	for _, order := range got {
		if order.Name == name {
			if order.Rig != rig {
				t.Fatalf("order %q rig = %q, want %q", name, order.Rig, rig)
			}
			return
		}
	}
	t.Fatalf("missing order %q in %#v", name, got)
}

// TestPackV2OrdersOnlyPackVisibleToCity reproduces ga-0vfs: a pack with
// orders/<name>.toml but NO formulas/ directory should still have its orders
// discovered. Currently the pack contributes no formula layer (because the
// formulas/ stat fails), and order discovery walks only formula layers, so
// the pack's orders are silently skipped.
func TestPackV2OrdersOnlyPackVisibleToCity(t *testing.T) {
	cityDir := t.TempDir()
	packDir := filepath.Join(cityDir, "packs", "pr-audit")

	for _, dir := range []string{
		filepath.Join(packDir, "orders"),
		filepath.Join(packDir, "scripts"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeFile(t, filepath.Join(cityDir, "pack.toml"), `
[pack]
name = "testcity"
schema = 2

[imports.pr_audit]
source = "./packs/pr-audit"
`)
	writeFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]
`)
	writeFile(t, filepath.Join(packDir, "pack.toml"), `
[pack]
name = "pr-audit"
schema = 2
`)
	writeFile(t, filepath.Join(packDir, "orders", "pr-audit.toml"), `
[order]
description = "Audit open PRs"
trigger = "cooldown"
interval = "1h"
exec = "$PACK_DIR/scripts/pr-audit.sh"
`)
	writeFile(t, filepath.Join(packDir, "scripts", "pr-audit.sh"), `#!/bin/sh
echo "audit"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	var stderr bytes.Buffer
	discovered, err := scanAllOrders(cityDir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v; stderr: %s", err, stderr.String())
	}
	assertOrderScope(t, discovered, "pr-audit", "")
}

// TestPackV2OrdersOnlyPackVisibleToRig is the rig-level analog of
// TestPackV2OrdersOnlyPackVisibleToCity — a rig-imported pack with orders/
// but no formulas/ should still have its orders discovered.
func TestPackV2OrdersOnlyPackVisibleToRig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	packDir := filepath.Join(cityDir, "packs", "watcher")

	for _, dir := range []string{
		filepath.Join(cityDir, ".gc"),
		rigDir,
		filepath.Join(packDir, "orders"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	writeFile(t, filepath.Join(cityDir, "pack.toml"), `
[pack]
name = "testcity"
schema = 2
`)
	writeFile(t, filepath.Join(cityDir, "city.toml"), `
[workspace]
name = "testcity"

[[rigs]]
name = "frontend"

[rigs.imports.watcher]
source = "./packs/watcher"
`)
	writeFile(t, filepath.Join(cityDir, ".gc", "site.toml"), `
workspace_name = "testcity"

[[rig]]
name = "frontend"
path = "./frontend"
`)
	writeFile(t, filepath.Join(packDir, "pack.toml"), `
[pack]
name = "watcher"
schema = 2
`)
	writeFile(t, filepath.Join(packDir, "orders", "watcher-poll.toml"), `
[order]
description = "Poll watcher state"
trigger = "cooldown"
interval = "5m"
exec = "$PACK_DIR/scripts/poll.sh"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	var stderr bytes.Buffer
	discovered, err := scanAllOrders(cityDir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v; stderr: %s", err, stderr.String())
	}
	assertOrderScope(t, discovered, "watcher-poll", "frontend")
}

func assertSymlinkExists(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("missing symlink %s: %v", path, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", path)
	}
}

func assertSymlinkTarget(t *testing.T, path, want string) {
	t.Helper()
	assertSymlinkExists(t, path)
	got, err := os.Readlink(path)
	if err != nil {
		t.Fatalf("Readlink(%s): %v", path, err)
	}
	if got != want {
		t.Fatalf("Readlink(%s) = %q, want %q", path, got, want)
	}
}

func assertAgentQualifiedName(t *testing.T, agents []config.Agent, want string) {
	t.Helper()
	var got []string
	for _, agent := range agents {
		got = append(got, agent.QualifiedName())
		if agent.QualifiedName() == want {
			return
		}
	}
	t.Fatalf("missing agent %q in %#v", want, got)
}

func assertPathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("%s exists, want absent", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("checking %s: %v", path, err)
	}
}
