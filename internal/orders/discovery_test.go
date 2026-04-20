package orders

import (
	"bytes"
	"errors"
	"log"
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
	fs.Dirs["/pack/orders/health-check"] = true
	fs.Files["/pack/orders/health-check/order.toml"] = []byte(`
[order]
formula = "legacy-health-check"
trigger = "cron"
schedule = "0 * * * *"
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

func TestDiscoverRootFallsBackToSubdirectoryFormatWithWarning(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/pack/orders/health-check"] = true
	fs.Files["/pack/orders/health-check/order.toml"] = []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)

	logs := captureOrderLogs(t, func() {
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
		if orders[0].Source != "/pack/orders/health-check/order.toml" {
			t.Fatalf("Source = %q, want %q", orders[0].Source, "/pack/orders/health-check/order.toml")
		}
	})

	if !strings.Contains(logs, "rename to orders/health-check.toml") {
		t.Fatalf("logs = %q, want rename warning", logs)
	}
}

func TestDiscoverRootFallsBackToLegacyFormulaOrdersWithWarning(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/pack/formulas/orders/health-check"] = true
	fs.Files["/pack/formulas/orders/health-check/order.toml"] = []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)

	logs := captureOrderLogs(t, func() {
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
		if orders[0].Source != "/pack/formulas/orders/health-check/order.toml" {
			t.Fatalf("Source = %q, want %q", orders[0].Source, "/pack/formulas/orders/health-check/order.toml")
		}
	})

	if !strings.Contains(logs, "move to orders/health-check.toml") {
		t.Fatalf("logs = %q, want move warning", logs)
	}
}

func TestDiscoverRootSkipsUnreadableFlatFile(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/pack/orders/health-check.toml"] = []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)
	fs.Errors["/pack/orders/health-check.toml"] = errors.New("boom")

	orders, err := discoverRoot(fs, ScanRoot{
		Dir:          "/pack/orders",
		FormulaLayer: "/pack/formulas",
	})
	if err != nil {
		t.Fatalf("discoverRoot: %v", err)
	}
	if len(orders) != 0 {
		t.Fatalf("got %d orders, want 0", len(orders))
	}
}

func captureOrderLogs(t *testing.T, fn func()) string {
	t.Helper()

	var buf bytes.Buffer
	origWriter := log.Writer()
	origFlags := log.Flags()
	origPrefix := log.Prefix()
	log.SetOutput(&buf)
	log.SetFlags(0)
	log.SetPrefix("")
	defer func() {
		log.SetOutput(origWriter)
		log.SetFlags(origFlags)
		log.SetPrefix(origPrefix)
	}()

	fn()
	return buf.String()
}
