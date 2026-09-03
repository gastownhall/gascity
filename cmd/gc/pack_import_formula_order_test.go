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

	// The example city no longer carries a checked-in gastown pack copy;
	// materialize the module-embedded pack so the wrapper import below
	// exercises transitive local-path composition against the real bytes.
	gastownPackDir := materializeEmbeddedGastownPack(t)
	retiredMaintenanceFormulaLayer := filepath.Join(filepath.Dir(gastownPackDir), "maintenance", "formulas")
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
	code := doOrderRun(discovered, "digest-generate", "", cityDir, beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
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

// TestOrderRunResolvesFormulaFromAnyConfiguredLayerNotJustItsOwn is the
// regression for #4378: a city-authored order (living in <city>/orders/,
// not inside any pack) references a formula shipped by an imported pack.
// gc formula list/show resolve the formula fine (they search the full
// aggregated city+rig layer set), but order dispatch restricted its search
// to just the order's own discovery layer (a.FormulaLayer, here the city's
// local formulas dir) — which never contains a pack-provided formula, so
// the two resolvers disagreed. The two are different things: FormulaLayer
// records where the ORDER FILE was found (for name-collision precedence),
// not which layers its FORMULA may live in.
func TestOrderRunResolvesFormulaFromAnyConfiguredLayerNotJustItsOwn(t *testing.T) {
	cityDir := t.TempDir()
	opsPackDir := filepath.Join(cityDir, "packs", "ops")

	for _, dir := range []string{
		filepath.Join(cityDir, ".gc"),
		filepath.Join(cityDir, "orders"),
		filepath.Join(opsPackDir, "formulas"),
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
`)
	writeFile(t, filepath.Join(opsPackDir, "pack.toml"), `
[pack]
name = "ops"
schema = 2
`)
	writeFile(t, filepath.Join(opsPackDir, "formulas", "pack-formula.toml"), `
formula = "pack-formula"

[[steps]]
id = "step"
title = "Do work"
`)
	// City-authored order, NOT inside any pack — its own FormulaLayer is
	// the city's local formulas dir, which does not contain pack-formula.
	writeFile(t, filepath.Join(cityDir, "orders", "my-order.toml"), `
[order]
formula = "pack-formula"
trigger = "cooldown"
interval = "24h"
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
	order, ok := findOrder(discovered, "my-order", "")
	if !ok {
		t.Fatalf("missing my-order in %#v", discovered)
	}
	packFormulaDir := filepath.Join(opsPackDir, "formulas")
	if order.FormulaLayer == packFormulaDir {
		t.Fatalf("test setup invalid: order's own FormulaLayer %q unexpectedly matches the pack formula dir — bug wouldn't reproduce", order.FormulaLayer)
	}
	assertContainsString(t, cfg.FormulaLayers.City, packFormulaDir)

	store := beads.NewMemStore()
	var stdout bytes.Buffer
	code := doOrderRun(discovered, "my-order", "", cityDir, beads.OrdersStore{Store: store}, nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doOrderRun = %d, want 0 (formula lives in an imported pack layer, not the order's own layer); stderr: %s", code, stderr.String())
	}
}

// TestOrderFormulaSearchPathsStayScopedToTheOrdersRig pins the scope
// boundary of the #4378 fix: order dispatch broadens past the order's own
// FormulaLayer to the order's own layer set
// (FormulaLayers.SearchPaths(rig), which already embeds the city layers) —
// not to every rig's layers. A rig-A order must not be able to dispatch a
// formula only rig B ships, and a city order must not reach into either
// rig's private layers. Formula resolution is last-wins over the search
// paths, so an aggregated set would also make a same-named formula in two
// rigs resolve by map iteration order.
func TestOrderFormulaSearchPathsStayScopedToTheOrdersRig(t *testing.T) {
	cityDir := t.TempDir()
	opsPackDir := filepath.Join(cityDir, "packs", "ops")
	frontendPackDir := filepath.Join(cityDir, "packs", "fe")
	backendPackDir := filepath.Join(cityDir, "packs", "be")

	for _, dir := range []string{
		filepath.Join(cityDir, ".gc"),
		filepath.Join(cityDir, "orders"),
		filepath.Join(cityDir, "frontend"),
		filepath.Join(cityDir, "backend"),
		filepath.Join(opsPackDir, "formulas"),
		filepath.Join(frontendPackDir, "formulas"),
		filepath.Join(frontendPackDir, "orders"),
		filepath.Join(backendPackDir, "formulas"),
		filepath.Join(backendPackDir, "orders"),
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

[rigs.imports.fe]
source = "./packs/fe"

[[rigs]]
name = "backend"

[rigs.imports.be]
source = "./packs/be"
`)
	writeFile(t, filepath.Join(cityDir, ".gc", "site.toml"), `
workspace_name = "testcity"

[[rig]]
name = "frontend"
path = "./frontend"

[[rig]]
name = "backend"
path = "./backend"
`)
	writeFile(t, filepath.Join(opsPackDir, "pack.toml"), `
[pack]
name = "ops"
schema = 2
`)
	writeFile(t, filepath.Join(opsPackDir, "formulas", "city-formula.toml"), `
formula = "city-formula"

[[steps]]
id = "step"
title = "Do work"
`)
	writeFile(t, filepath.Join(frontendPackDir, "pack.toml"), `
[pack]
name = "fe"
schema = 2
`)
	writeFile(t, filepath.Join(frontendPackDir, "formulas", "fe-formula.toml"), `
formula = "fe-formula"

[[steps]]
id = "step"
title = "Do work"
`)
	writeFile(t, filepath.Join(frontendPackDir, "orders", "fe-order.toml"), `
[order]
formula = "fe-formula"
trigger = "cooldown"
interval = "24h"
`)
	writeFile(t, filepath.Join(backendPackDir, "pack.toml"), `
[pack]
name = "be"
schema = 2
`)
	writeFile(t, filepath.Join(backendPackDir, "formulas", "be-formula.toml"), `
formula = "be-formula"

[[steps]]
id = "step"
title = "Do work"
`)
	writeFile(t, filepath.Join(backendPackDir, "orders", "be-order.toml"), `
[order]
formula = "be-formula"
trigger = "cooldown"
interval = "24h"
`)
	// City-authored order, outside every pack.
	writeFile(t, filepath.Join(cityDir, "orders", "city-order.toml"), `
[order]
formula = "city-formula"
trigger = "cooldown"
interval = "24h"
`)

	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	opsFormulaDir := filepath.Join(opsPackDir, "formulas")
	frontendFormulaDir := filepath.Join(frontendPackDir, "formulas")
	backendFormulaDir := filepath.Join(backendPackDir, "formulas")

	// Fixture guard: each rig must genuinely own a layer the other lacks,
	// or the isolation assertions below would pass vacuously.
	assertContainsString(t, cfg.FormulaLayers.SearchPaths("frontend"), frontendFormulaDir)
	assertNotContainsString(t, cfg.FormulaLayers.SearchPaths("frontend"), backendFormulaDir)
	assertContainsString(t, cfg.FormulaLayers.SearchPaths("backend"), backendFormulaDir)
	assertNotContainsString(t, cfg.FormulaLayers.SearchPaths("backend"), frontendFormulaDir)

	var stderr bytes.Buffer
	discovered, err := scanAllOrders(cityDir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v; stderr: %s", err, stderr.String())
	}

	assertOwnLayerHighestPriority := func(paths []string, order orders.Order) {
		t.Helper()
		if len(paths) == 0 || paths[len(paths)-1] != order.FormulaLayer {
			t.Fatalf("order %q search paths = %#v, want own layer %q last (highest priority)", order.ScopedName(), paths, order.FormulaLayer)
		}
	}

	frontendOrder, ok := findOrder(discovered, "fe-order", "frontend")
	if !ok {
		t.Fatalf("missing fe-order on rig frontend in %#v", discovered)
	}
	frontendPaths := orderFormulaSearchPaths(cfg, frontendOrder)
	assertContainsString(t, frontendPaths, opsFormulaDir)
	assertContainsString(t, frontendPaths, frontendFormulaDir)
	assertNotContainsString(t, frontendPaths, backendFormulaDir)
	assertOwnLayerHighestPriority(frontendPaths, frontendOrder)

	backendOrder, ok := findOrder(discovered, "be-order", "backend")
	if !ok {
		t.Fatalf("missing be-order on rig backend in %#v", discovered)
	}
	backendPaths := orderFormulaSearchPaths(cfg, backendOrder)
	assertContainsString(t, backendPaths, opsFormulaDir)
	assertContainsString(t, backendPaths, backendFormulaDir)
	assertNotContainsString(t, backendPaths, frontendFormulaDir)
	assertOwnLayerHighestPriority(backendPaths, backendOrder)

	cityOrder, ok := findOrder(discovered, "city-order", "")
	if !ok {
		t.Fatalf("missing city-order in %#v", discovered)
	}
	cityPaths := orderFormulaSearchPaths(cfg, cityOrder)
	assertContainsString(t, cityPaths, opsFormulaDir)
	assertNotContainsString(t, cityPaths, frontendFormulaDir)
	assertNotContainsString(t, cityPaths, backendFormulaDir)
	assertOwnLayerHighestPriority(cityPaths, cityOrder)
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
