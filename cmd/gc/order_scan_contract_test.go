package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
)

// order_scan_contract_test.go pins the shared order scanning behavior that
// the dispatcher and doctor consumers must preserve through refactoring.
//
// Run these contract tests with:
//   go test ./cmd/gc/... -run TestOrderScanContract

// writeContractOrder writes a canonical flat order TOML to dir/name.toml.
func writeContractOrder(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".toml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// contractCitySetup creates a minimal city directory with the default formula
// layer. Returns the city path and the formulas directory.
func contractCitySetup(t *testing.T) (cityPath, formulasDir string) {
	t.Helper()
	cityPath = t.TempDir()
	formulasDir = filepath.Join(cityPath, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

// TestOrderScanContractNoCityScanDouble verifies that city orders are not
// duplicated when a rig's formula layers exactly match the city layers
// (rigExclusiveLayers returns nil in this case).
func TestOrderScanContractNoCityScanDouble(t *testing.T) {
	cityPath, cityFormulasDir := contractCitySetup(t)
	writeContractOrder(t, filepath.Join(cityPath, "orders"), "heartbeat", `[order]
exec = "scripts/heartbeat.sh"
trigger = "cooldown"
interval = "5m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityFormulasDir},
			Rigs: map[string][]string{
				// Rig shares only city layers — rigExclusiveLayers returns nil.
				"demo": {cityFormulasDir},
			},
		},
	}

	var stderr bytes.Buffer
	aa, err := scanAllOrders(cityPath, cfg, &stderr, "test")
	if err != nil {
		t.Fatalf("scanAllOrders: %v", err)
	}
	if len(aa) != 1 {
		names := make([]string, len(aa))
		for i, a := range aa {
			names[i] = a.Name + "(rig=" + a.Rig + ")"
		}
		t.Fatalf("got %d orders %v, want 1 — city order must not be double-scanned via rig path", len(aa), names)
	}
	if aa[0].Name != "heartbeat" {
		t.Errorf("Name = %q, want %q", aa[0].Name, "heartbeat")
	}
	if aa[0].Rig != "" {
		t.Errorf("city order Rig = %q, want empty", aa[0].Rig)
	}
}

// TestOrderScanContractRigExclusiveLayerStampsRigField verifies that orders
// discovered in rig-exclusive formula layers have their Rig field set to the
// owning rig name.
func TestOrderScanContractRigExclusiveLayerStampsRigField(t *testing.T) {
	cityPath, cityFormulasDir := contractCitySetup(t)
	// No city orders.

	rigDir := t.TempDir()
	rigFormulasDir := filepath.Join(rigDir, "formulas")
	if err := os.MkdirAll(rigFormulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeContractOrder(t, filepath.Join(rigDir, "orders"), "rig-db-health", `[order]
exec = "scripts/db-health.sh"
trigger = "cooldown"
interval = "10m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityFormulasDir},
			Rigs: map[string][]string{
				// rigFormulasDir extends city — it is the exclusive layer.
				"myrig": {cityFormulasDir, rigFormulasDir},
			},
		},
	}

	var stderr bytes.Buffer
	aa, err := scanAllOrders(cityPath, cfg, &stderr, "test")
	if err != nil {
		t.Fatalf("scanAllOrders: %v", err)
	}
	if len(aa) != 1 {
		t.Fatalf("got %d orders, want 1", len(aa))
	}
	if aa[0].Name != "rig-db-health" {
		t.Errorf("Name = %q, want %q", aa[0].Name, "rig-db-health")
	}
	if aa[0].Rig != "myrig" {
		t.Errorf("Rig = %q, want %q — rig-exclusive order must carry rig name", aa[0].Rig, "myrig")
	}
}

// TestOrderScanContractCityAndRigOrdersBothDiscovered verifies that city orders
// and rig-exclusive-layer orders are both returned with correct Rig stamps and
// no duplication.
func TestOrderScanContractCityAndRigOrdersBothDiscovered(t *testing.T) {
	cityPath, cityFormulasDir := contractCitySetup(t)
	writeContractOrder(t, filepath.Join(cityPath, "orders"), "city-heartbeat", `[order]
exec = "scripts/heartbeat.sh"
trigger = "cooldown"
interval = "5m"
`)

	rigDir := t.TempDir()
	rigFormulasDir := filepath.Join(rigDir, "formulas")
	if err := os.MkdirAll(rigFormulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeContractOrder(t, filepath.Join(rigDir, "orders"), "rig-db-sweep", `[order]
exec = "scripts/db-sweep.sh"
trigger = "cooldown"
interval = "10m"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityFormulasDir},
			Rigs: map[string][]string{
				"backend": {cityFormulasDir, rigFormulasDir},
			},
		},
	}

	var stderr bytes.Buffer
	aa, err := scanAllOrders(cityPath, cfg, &stderr, "test")
	if err != nil {
		t.Fatalf("scanAllOrders: %v", err)
	}
	if len(aa) != 2 {
		t.Fatalf("got %d orders, want 2 (one city + one rig)", len(aa))
	}

	var cityOrder, rigOrder *orders.Order
	for i := range aa {
		switch aa[i].Name {
		case "city-heartbeat":
			cityOrder = &aa[i]
		case "rig-db-sweep":
			rigOrder = &aa[i]
		}
	}
	if cityOrder == nil {
		t.Fatal("city-heartbeat not found")
	}
	if cityOrder.Rig != "" {
		t.Errorf("city-heartbeat Rig = %q, want empty", cityOrder.Rig)
	}
	if rigOrder == nil {
		t.Fatal("rig-db-sweep not found")
	}
	if rigOrder.Rig != "backend" {
		t.Errorf("rig-db-sweep Rig = %q, want %q", rigOrder.Rig, "backend")
	}
}

// TestOrderScanContractScanAllIncludesManualTrigger verifies that scanAllOrders
// returns manual-trigger orders so gc order list/check can display them.
// Manual orders are excluded only at the dispatcher layer, not at the scan layer.
func TestOrderScanContractScanAllIncludesManualTrigger(t *testing.T) {
	cityPath, cityFormulasDir := contractCitySetup(t)
	writeContractOrder(t, filepath.Join(cityPath, "orders"), "deploy", `[order]
formula = "mol-deploy"
trigger = "manual"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityFormulasDir},
		},
	}

	var stderr bytes.Buffer
	aa, err := scanAllOrders(cityPath, cfg, &stderr, "test")
	if err != nil {
		t.Fatalf("scanAllOrders: %v", err)
	}
	if len(aa) != 1 {
		t.Fatalf("got %d orders, want 1 — manual orders must be visible to list/check consumers", len(aa))
	}
	if aa[0].Trigger != "manual" {
		t.Errorf("Trigger = %q, want %q", aa[0].Trigger, "manual")
	}
}

// TestOrderScanContractDispatcherFiltersManualFromFilesystem verifies that
// buildOrderDispatcher returns nil when all discovered orders have manual
// triggers. Manual orders are never auto-dispatched by the controller.
func TestOrderScanContractDispatcherFiltersManualFromFilesystem(t *testing.T) {
	cityPath, cityFormulasDir := contractCitySetup(t)
	writeContractOrder(t, filepath.Join(cityPath, "orders"), "manual-deploy", `[order]
formula = "mol-deploy"
trigger = "manual"
`)

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityFormulasDir},
		},
	}

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(cityPath, cfg, events.Discard, &stderr)
	if ad != nil {
		t.Error("expected nil dispatcher — manual-trigger orders must be excluded from auto-dispatch")
	}
}

// TestOrderScanContractOverrideAppliedBeforeReturning verifies that
// loadAllOrders applies city.toml [orders.overrides] before returning.
// Consumers see overridden field values, not the original TOML values.
func TestOrderScanContractOverrideAppliedBeforeReturning(t *testing.T) {
	cityPath, cityFormulasDir := contractCitySetup(t)
	writeContractOrder(t, filepath.Join(cityPath, "orders"), "db-sync", `[order]
exec = "scripts/db-sync.sh"
trigger = "cooldown"
interval = "1h"
`)

	newInterval := "30m"
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityFormulasDir},
		},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{
				{Name: "db-sync", Interval: &newInterval},
			},
		},
	}

	var stderr bytes.Buffer
	aa, code := loadAllOrders(cityPath, cfg, &stderr, "test")
	if code != 0 {
		t.Fatalf("loadAllOrders code %d; stderr: %s", code, stderr.String())
	}
	if len(aa) != 1 {
		t.Fatalf("got %d orders, want 1", len(aa))
	}
	if aa[0].Interval != "30m" {
		t.Errorf("Interval = %q, want %q — override not applied", aa[0].Interval, "30m")
	}
}

// TestOrderScanContractOverrideEnabledFalseMarksOrderDisabled verifies that an
// order overridden with enabled=false has IsEnabled()=false after loadAllOrders.
// Consumers responsible for auto-dispatch must honor this flag.
func TestOrderScanContractOverrideEnabledFalseMarksOrderDisabled(t *testing.T) {
	cityPath, cityFormulasDir := contractCitySetup(t)
	writeContractOrder(t, filepath.Join(cityPath, "orders"), "nightly", `[order]
exec = "scripts/nightly.sh"
trigger = "cron"
schedule = "0 2 * * *"
`)

	disabled := false
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityFormulasDir},
		},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{
				{Name: "nightly", Enabled: &disabled},
			},
		},
	}

	var stderr bytes.Buffer
	aa, code := loadAllOrders(cityPath, cfg, &stderr, "test")
	if code != 0 {
		t.Fatalf("loadAllOrders code %d; stderr: %s", code, stderr.String())
	}
	if len(aa) != 1 {
		t.Fatalf("got %d orders, want 1 (override disables but does not remove from slice)", len(aa))
	}
	if aa[0].IsEnabled() {
		t.Error("order.IsEnabled() = true after enabled=false override; override was not applied")
	}
}

// TestOrderScanContractRigScopedOverrideTargetsCorrectOrder verifies that a
// rig-scoped override modifies only the named rig's order, leaving the
// same-named city-level order unchanged.
func TestOrderScanContractRigScopedOverrideTargetsCorrectOrder(t *testing.T) {
	cityPath, cityFormulasDir := contractCitySetup(t)
	writeContractOrder(t, filepath.Join(cityPath, "orders"), "health-check", `[order]
exec = "scripts/health.sh"
trigger = "cooldown"
interval = "5m"
`)

	rigDir := t.TempDir()
	rigFormulasDir := filepath.Join(rigDir, "formulas")
	if err := os.MkdirAll(rigFormulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeContractOrder(t, filepath.Join(rigDir, "orders"), "health-check", `[order]
exec = "scripts/health.sh"
trigger = "cooldown"
interval = "5m"
`)

	rigOverrideInterval := "10m"
	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{cityFormulasDir},
			Rigs: map[string][]string{
				"staging": {cityFormulasDir, rigFormulasDir},
			},
		},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{
				{Name: "health-check", Rig: "staging", Interval: &rigOverrideInterval},
			},
		},
	}

	var stderr bytes.Buffer
	aa, code := loadAllOrders(cityPath, cfg, &stderr, "test")
	if code != 0 {
		t.Fatalf("loadAllOrders code %d; stderr: %s", code, stderr.String())
	}
	if len(aa) != 2 {
		t.Fatalf("got %d orders, want 2 (city + rig health-check)", len(aa))
	}

	for _, a := range aa {
		switch a.Rig {
		case "":
			if a.Interval != "5m" {
				t.Errorf("city health-check Interval = %q, want %q — rig override must not affect city order", a.Interval, "5m")
			}
		case "staging":
			if a.Interval != "10m" {
				t.Errorf("staging health-check Interval = %q, want %q — rig override not applied", a.Interval, "10m")
			}
		}
	}
}
