package orders

import (
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestDiscoverRootPrefersFlatFiles(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/pack/orders/health-check.toml"] = []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)

	orders, err := discoverRoot(fs, ScanRoot{
		Dir:          "/pack/orders",
		FormulaLayer: "/pack/formulas",
	})
	if err != nil {
		t.Fatalf("discoverRoot: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Name != "health-check" {
		t.Fatalf("Name = %q, want %q", orders[0].Name, "health-check")
	}
	if orders[0].Formula != "health-check" {
		t.Fatalf("Formula = %q, want %q", orders[0].Formula, "health-check")
	}
	if orders[0].Source != "/pack/orders/health-check.toml" {
		t.Fatalf("Source = %q, want %q", orders[0].Source, "/pack/orders/health-check.toml")
	}
}

func TestDiscoverRootRejectsSubdirectoryFormat(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/pack/orders/health-check"] = true
	fs.Files["/pack/orders/health-check/order.toml"] = []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)

	_, err := discoverRoot(fs, ScanRoot{
		Dir:          "/pack/orders",
		FormulaLayer: "/pack/formulas",
	})
	if err == nil {
		t.Fatal("discoverRoot succeeded, want hard error for legacy subdirectory layout")
	}
	if !strings.Contains(err.Error(), "rename to orders/health-check.toml") {
		t.Fatalf("error = %v, want rename guidance", err)
	}
}

func TestScanRootsRejectsSubdirectoryFormat(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/pack/orders/health-check"] = true
	fs.Files["/pack/orders/health-check/order.toml"] = []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)

	_, err := ScanRoots(fs, []ScanRoot{{
		Dir:          "/pack/orders",
		FormulaLayer: "/pack/formulas",
	}}, nil)
	if err == nil {
		t.Fatal("ScanRoots succeeded, want hard error for legacy subdirectory layout")
	}
	if !strings.Contains(err.Error(), "unsupported PackV1 order path /pack/orders/health-check/order.toml") {
		t.Fatalf("error = %v, want PackV1 path rejection", err)
	}
	if !strings.Contains(err.Error(), "rename to orders/health-check.toml") {
		t.Fatalf("error = %v, want flat-file migration guidance", err)
	}
}

func TestScanRootsAggregatesLegacyOrderLayoutGuidance(t *testing.T) {
	fs := fsys.NewFake()
	for _, dir := range []string{
		"/base/orders/alpha",
		"/base/formulas/orders/beta",
		"/pack/orders/gamma",
	} {
		fs.Dirs[dir] = true
		fs.Files[dir+"/order.toml"] = []byte(`
[order]
formula = "noop"
trigger = "manual"
`)
	}

	_, err := ScanRoots(fs, []ScanRoot{
		{Dir: "/base/orders", FormulaLayer: "/base/formulas"},
		{Dir: "/pack/orders", FormulaLayer: "/pack/formulas"},
	}, nil)
	if err == nil {
		t.Fatal("ScanRoots succeeded, want hard error for legacy subdirectory layouts")
	}
	for _, want := range []string{
		"unsupported PackV1 order paths",
		"/base/orders/alpha/order.toml",
		"rename to orders/alpha.toml",
		"/base/formulas/orders/beta/order.toml",
		"move to orders/beta.toml",
		"/pack/orders/gamma/order.toml",
		"rename to orders/gamma.toml",
		"applies to all pack schemas",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %v, want substring %q", err, want)
		}
	}
}

func TestDiscoverRootRejectsLegacyFormulaOrders(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/pack/formulas/orders/health-check"] = true
	fs.Files["/pack/formulas/orders/health-check/order.toml"] = []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)

	_, err := discoverRoot(fs, ScanRoot{
		Dir:          "/pack/orders",
		FormulaLayer: "/pack/formulas",
	})
	if err == nil {
		t.Fatal("discoverRoot succeeded, want hard error for legacy formulas/orders path")
	}
	if !strings.Contains(err.Error(), "move to orders/health-check.toml") {
		t.Fatalf("error = %v, want move guidance", err)
	}
}

func TestDiscoverRootAcceptsInfixedFlatOrderFilename(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/pack/orders/health-check.order.toml"] = []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)

	orders, err := discoverRoot(fs, ScanRoot{
		Dir:          "/pack/orders",
		FormulaLayer: "/pack/formulas",
	})
	if err != nil {
		t.Fatalf("discoverRoot: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Name != "health-check" {
		t.Fatalf("Name = %q, want health-check", orders[0].Name)
	}
	if orders[0].Source != "/pack/orders/health-check.order.toml" {
		t.Fatalf("Source = %q, want infixed flat source", orders[0].Source)
	}
}

func TestDiscoverRootPlainFlatOrderBeatsInfixedSibling(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/pack/orders/health-check.order.toml"] = []byte(`
[order]
formula = "infixed"
trigger = "manual"
`)
	fs.Files["/pack/orders/health-check.toml"] = []byte(`
[order]
formula = "plain"
trigger = "manual"
`)

	orders, err := discoverRoot(fs, ScanRoot{
		Dir:          "/pack/orders",
		FormulaLayer: "/pack/formulas",
	})
	if err != nil {
		t.Fatalf("discoverRoot: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Name != "health-check" {
		t.Fatalf("Name = %q, want health-check", orders[0].Name)
	}
	if orders[0].Formula != "plain" {
		t.Fatalf("Formula = %q, want plain spelling to win", orders[0].Formula)
	}
	if orders[0].Source != "/pack/orders/health-check.toml" {
		t.Fatalf("Source = %q, want plain flat source", orders[0].Source)
	}
}

func TestDiscoverRootPlainFlatOrderIgnoresMalformedInfixedSibling(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/pack/orders/health-check.order.toml"] = []byte("[order\n")
	fs.Files["/pack/orders/health-check.toml"] = []byte(`
[order]
formula = "plain"
trigger = "manual"
`)

	orders, err := discoverRoot(fs, ScanRoot{
		Dir:          "/pack/orders",
		FormulaLayer: "/pack/formulas",
	})
	if err != nil {
		t.Fatalf("discoverRoot: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("got %d orders, want 1", len(orders))
	}
	if orders[0].Formula != "plain" {
		t.Fatalf("Formula = %q, want plain spelling to win", orders[0].Formula)
	}
	if orders[0].Source != "/pack/orders/health-check.toml" {
		t.Fatalf("Source = %q, want plain flat source", orders[0].Source)
	}
}

func TestDiscoverRootReturnsUnreadableFlatFileError(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/pack/orders/health-check.toml"] = []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)
	fs.Errors["/pack/orders/health-check.toml"] = errors.New("boom")

	_, err := discoverRoot(fs, ScanRoot{
		Dir:          "/pack/orders",
		FormulaLayer: "/pack/formulas",
	})
	if err == nil {
		t.Fatal("discoverRoot returned nil error for unreadable flat order file")
	}
	if !strings.Contains(err.Error(), "reading order /pack/orders/health-check.toml") {
		t.Fatalf("error = %v, want flat order path context", err)
	}
}

func TestDiscoverRootReturnsUnreadableRootError(t *testing.T) {
	fs := fsys.NewFake()
	fs.Errors["/pack/orders"] = errors.New("permission denied")

	_, err := discoverRoot(fs, ScanRoot{
		Dir:          "/pack/orders",
		FormulaLayer: "/pack/formulas",
	})
	if err == nil {
		t.Fatal("discoverRoot returned nil error for unreadable root")
	}
	if !strings.Contains(err.Error(), "reading order root") {
		t.Fatalf("error = %v, want readable root context", err)
	}
}
