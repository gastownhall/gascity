package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestLoadActiveOrdersOverrideDisablesOrder(t *testing.T) {
	cityDir, cfg := newOrderEnabledFilterCity(t)

	var stderr bytes.Buffer
	aa, code := loadActiveOrdersForCity(cityDir, cfg, &stderr, "gc order list")
	if code != 0 {
		t.Fatalf("loadActiveOrdersForCity returned code %d; stderr: %s", code, stderr.String())
	}
	requireOrderNames(t, aa, "keep")
}

func TestLoadAllOrdersPreservesOverrideDisabledOrder(t *testing.T) {
	cityDir, cfg := newOrderEnabledFilterCity(t)

	var stderr bytes.Buffer
	aa, code := loadAllOrders(cityDir, cfg, &stderr, "gc order show")
	if code != 0 {
		t.Fatalf("loadAllOrders returned code %d; stderr: %s", code, stderr.String())
	}
	requireOrderEnabledState(t, aa, "keep", true)
	requireOrderEnabledState(t, aa, "drop", false)
}

func TestBuildOrderDispatcherOverrideDisablesOrder(t *testing.T) {
	cityDir, cfg := newOrderEnabledFilterCity(t)

	var stderr bytes.Buffer
	ad := buildOrderDispatcher(cityDir, cfg, events.Discard, &stderr)
	if ad == nil {
		t.Fatalf("buildOrderDispatcher returned nil; stderr: %s", stderr.String())
	}
	mad := ad.(*memoryOrderDispatcher)
	requireOrderNames(t, mad.aa, "keep")
}

func TestControllerStateOrdersOverrideDisablesOrder(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityDir, cfg := newOrderEnabledFilterCity(t)

	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "test-city", cityDir)
	requireOrderNames(t, cs.Orders(), "keep")
}

func TestControllerStateOrdersDisableEnableRoundTrip(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityDir, cfg := newPersistedOrderEnabledFilterCity(t)

	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "test-city", cityDir)
	if err := cs.DisableOrder("keep", ""); err != nil {
		t.Fatalf("DisableOrder: %v", err)
	}
	requireOrderAbsent(t, cs.Orders(), "keep")
	requireOrderEnabledState(t, cs.OrdersAll(), "keep", false)

	if err := cs.EnableOrder("keep", ""); err != nil {
		t.Fatalf("EnableOrder: %v", err)
	}
	requireOrderEnabledState(t, cs.Orders(), "keep", true)
	requireOrderEnabledState(t, cs.OrdersAll(), "keep", true)
}

func TestControllerStateOrdersAllLogsOverrideErrors(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityDir, cfg := newOrderEnabledFilterCity(t)
	cfg.Orders.Overrides = []config.OrderOverride{{Name: "missing"}}

	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "test-city", cityDir)
	logs := captureCmdOrderLogs(t, func() {
		_ = cs.OrdersAll()
	})
	if !strings.Contains(logs, "applying order overrides") || !strings.Contains(logs, "missing") {
		t.Fatalf("logs = %q, want order override failure with missing order name", logs)
	}
}

func TestCmdOrderShowIncludesOverrideDisabledOrder(t *testing.T) {
	clearCityRigFlags(t)
	cityDir, _ := newPersistedOrderEnabledFilterCity(t)
	t.Chdir(cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdOrderShow("drop", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderShow = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "drop") {
		t.Fatalf("stdout missing disabled order name:\n%s", stdout.String())
	}
}

func TestCmdOrderHistoryIncludesOverrideDisabledOrder(t *testing.T) {
	clearCityRigFlags(t)
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")
	cityDir, _ := newPersistedOrderEnabledFilterCity(t)
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}
	store, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatal(err)
	}
	run, err := store.Create(beads.Bead{Title: "drop run", Type: "task", Labels: []string{"order-run:drop"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(cityDir)

	var stdout, stderr bytes.Buffer
	code := cmdOrderHistory("drop", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdOrderHistory = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), run.ID) {
		t.Fatalf("stdout missing disabled order run %q:\n%s", run.ID, stdout.String())
	}
}

func newOrderEnabledFilterCity(t *testing.T) (string, *config.City) {
	t.Helper()

	cityDir := t.TempDir()
	formulasDir := filepath.Join(cityDir, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatalf("mkdir formulas: %v", err)
	}
	writeOrderEnabledFilterFixture(t, cityDir, "keep")
	writeOrderEnabledFilterFixture(t, cityDir, "drop")

	enabled := false
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{
			City: []string{formulasDir},
		},
		Orders: config.OrdersConfig{
			Overrides: []config.OrderOverride{
				{Name: "drop", Enabled: &enabled},
			},
		},
	}
	return cityDir, cfg
}

func newPersistedOrderEnabledFilterCity(t *testing.T) (string, *config.City) {
	t.Helper()

	cityDir, cfg := newOrderEnabledFilterCity(t)
	writeProviderAwareTestCity(t, cityDir, `[workspace]
name = "test-city"

[[orders.overrides]]
name = "drop"
enabled = false
`)
	return cityDir, cfg
}

func writeOrderEnabledFilterFixture(t *testing.T, cityDir, name string) {
	t.Helper()

	ordersDir := filepath.Join(cityDir, "orders")
	if err := os.MkdirAll(ordersDir, 0o755); err != nil {
		t.Fatalf("mkdir orders: %v", err)
	}
	content := `[order]
exec = "scripts/` + name + `.sh"
trigger = "cooldown"
interval = "1m"
`
	if err := os.WriteFile(filepath.Join(ordersDir, name+".toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write order %q: %v", name, err)
	}
}

func requireOrderEnabledState(t *testing.T, aa []orders.Order, name string, want bool) {
	t.Helper()

	for _, a := range aa {
		if a.Name == name {
			if got := a.IsEnabled(); got != want {
				t.Fatalf("order %q enabled = %v, want %v; all orders: %#v", name, got, want, aa)
			}
			return
		}
	}
	t.Fatalf("order %q not found in %#v", name, aa)
}

func requireOrderAbsent(t *testing.T, aa []orders.Order, name string) {
	t.Helper()

	for _, a := range aa {
		if a.Name == name {
			t.Fatalf("order %q unexpectedly present in %#v", name, aa)
		}
	}
}

func requireOrderNames(t *testing.T, aa []orders.Order, want ...string) {
	t.Helper()

	if len(aa) != len(want) {
		t.Fatalf("orders = %#v, want names %v", aa, want)
	}
	for i, name := range want {
		if aa[i].Name != name {
			t.Fatalf("orders[%d].Name = %q, want %q; all orders: %#v", i, aa[i].Name, name, aa)
		}
	}
}

func clearCityRigFlags(t *testing.T) {
	t.Helper()

	prevCityFlag, prevRigFlag := cityFlag, rigFlag
	cityFlag, rigFlag = "", ""
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		rigFlag = prevRigFlag
	})
}
