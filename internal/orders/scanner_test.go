package orders

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestScan(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/layer1/orders/digest"] = true
	fs.Files["/layer1/orders/digest/order.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
pool = "dog"
`)
	fs.Dirs["/layer1/orders/cleanup"] = true
	fs.Files["/layer1/orders/cleanup/order.toml"] = []byte(`
[order]
formula = "mol-cleanup"
trigger = "cron"
schedule = "0 3 * * *"
`)

	orders, err := Scan(fs, []string{"/layer1"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("got %d orders, want 2", len(orders))
	}
	// Names should be set from directory names.
	names := map[string]bool{}
	for _, a := range orders {
		names[a.Name] = true
	}
	if !names["digest"] || !names["cleanup"] {
		t.Errorf("expected digest and cleanup, got %v", names)
	}
}

func TestScanEmpty(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/layer1"] = true

	orders, err := Scan(fs, []string{"/layer1"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 0 {
		t.Fatalf("got %d orders, want 0", len(orders))
	}
}

func TestScanLayerOverride(t *testing.T) {
	fs := fsys.NewFake()
	// Layer 1 (lower priority): digest with 24h.
	fs.Dirs["/layer1/orders/digest"] = true
	fs.Files["/layer1/orders/digest/order.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
pool = "dog"
`)
	// Layer 2 (higher priority): digest with 8h.
	fs.Dirs["/layer2/orders/digest"] = true
	fs.Files["/layer2/orders/digest/order.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "8h"
pool = "dog"
`)

	orders, err := Scan(fs, []string{"/layer1", "/layer2"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Interval != "8h" {
		t.Errorf("Interval = %q, want %q (layer 2 overrides)", orders[0].Interval, "8h")
	}
}

func TestScanSkip(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/layer1/orders/digest"] = true
	fs.Files["/layer1/orders/digest/order.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
`)
	fs.Dirs["/layer1/orders/cleanup"] = true
	fs.Files["/layer1/orders/cleanup/order.toml"] = []byte(`
[order]
formula = "mol-cleanup"
trigger = "manual"
`)

	orders, err := Scan(fs, []string{"/layer1"}, []string{"digest"})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Name != "cleanup" {
		t.Errorf("Name = %q, want %q", orders[0].Name, "cleanup")
	}
}

func TestScanDisabled(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/layer1/orders/digest"] = true
	fs.Files["/layer1/orders/digest/order.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "cooldown"
interval = "24h"
enabled = false
`)

	orders, err := Scan(fs, []string{"/layer1"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 0 {
		t.Fatalf("got %d orders, want 0 (disabled)", len(orders))
	}
}

func TestScanFormulaLayer(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/pack/formulas/orders/health"] = true
	fs.Files["/pack/formulas/orders/health/order.toml"] = []byte(`
[order]
exec = "$PACK_DIR/scripts/health.sh"
trigger = "cooldown"
interval = "1m"
`)

	orders, err := Scan(fs, []string{"/pack/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].FormulaLayer != "/pack/formulas" {
		t.Errorf("FormulaLayer = %q, want %q", orders[0].FormulaLayer, "/pack/formulas")
	}
}

func TestScanFormulaLayerOverride(t *testing.T) {
	fs := fsys.NewFake()
	// Layer 1: lower priority.
	fs.Dirs["/base/formulas/orders/health"] = true
	fs.Files["/base/formulas/orders/health/order.toml"] = []byte(`
[order]
exec = "$PACK_DIR/scripts/health.sh"
trigger = "cooldown"
interval = "1h"
`)
	// Layer 2: higher priority overrides.
	fs.Dirs["/pack/formulas/orders/health"] = true
	fs.Files["/pack/formulas/orders/health/order.toml"] = []byte(`
[order]
exec = "$PACK_DIR/scripts/health.sh"
trigger = "cooldown"
interval = "5m"
`)

	orders, err := Scan(fs, []string{"/base/formulas", "/pack/formulas"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	// FormulaLayer should come from the winning (higher-priority) layer.
	if orders[0].FormulaLayer != "/pack/formulas" {
		t.Errorf("FormulaLayer = %q, want %q", orders[0].FormulaLayer, "/pack/formulas")
	}
}

func TestScanSourcePath(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/layer1/orders/digest"] = true
	fs.Files["/layer1/orders/digest/order.toml"] = []byte(`
[order]
formula = "mol-digest"
trigger = "manual"
`)

	orders, err := Scan(fs, []string{"/layer1"}, nil)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Source != "/layer1/orders/digest/order.toml" {
		t.Errorf("Source = %q, want %q", orders[0].Source, "/layer1/orders/digest/order.toml")
	}
}
