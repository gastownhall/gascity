package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
)

func TestFilterOrdersForBackendKeepsCoreJSONLExportForManagedDolt(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Beads: config.BeadsConfig{Provider: "bd", Backend: "dolt"}}

	got := filterOrdersForBackend(cityPath, cfg, backendFilterOrders())

	if !hasOrder(got, coreJSONLExportOrder) {
		t.Fatalf("filterOrdersForBackend removed %s for managed Dolt: %+v", coreJSONLExportOrder, got)
	}
}

func TestFilterOrdersForBackendSkipsCoreJSONLExportForPostgresPlugin(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{Beads: config.BeadsConfig{Provider: "plugin", Backend: "postgres"}}

	got := filterOrdersForBackend(cityPath, cfg, backendFilterOrders())

	if hasOrder(got, coreJSONLExportOrder) {
		t.Fatalf("filterOrdersForBackend kept %s for postgres plugin: %+v", coreJSONLExportOrder, got)
	}
	if !hasOrder(got, "beads-health") {
		t.Fatalf("filterOrdersForBackend removed unrelated order: %+v", got)
	}
}

func TestFilterOrdersForBackendKeepsCoreJSONLExportForPluginCapability(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[beads]
provider = "plugin"
backend = "archive"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, "assets", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "assets", "scripts", "setup-archive.sh"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "pack.toml"), []byte(`
[pack]
name = "city"
schema = 2

[[backend_plugins]]
backend = "archive"
setup_hook = "assets/scripts/setup-archive.sh"
capabilities = ["jsonl-export"]
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := loadCityConfig(cityPath, nil)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	got := filterOrdersForBackend(cityPath, cfg, backendFilterOrders())

	if !hasOrder(got, coreJSONLExportOrder) {
		t.Fatalf("filterOrdersForBackend removed %s despite plugin capability: %+v", coreJSONLExportOrder, got)
	}
}

func backendFilterOrders() []orders.Order {
	return []orders.Order{
		{Name: coreJSONLExportOrder, Trigger: "cooldown", Interval: "15m", Exec: "true"},
		{Name: "beads-health", Trigger: "cooldown", Interval: "30s", Exec: "true"},
	}
}

func hasOrder(aa []orders.Order, name string) bool {
	return slices.ContainsFunc(aa, func(a orders.Order) bool {
		return a.Name == name
	})
}
