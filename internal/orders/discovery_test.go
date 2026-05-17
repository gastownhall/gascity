package orders

import (
	"bytes"
	"errors"
	"log"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/logutil"
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

func TestDiscoverRootAcceptsLegacyFlatOrderFilename(t *testing.T) {
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
		t.Fatalf("Source = %q, want legacy flat source", orders[0].Source)
	}
}

func TestDiscoverRootDedupsDeprecatedPathWarnings(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/pack/orders/health-check"] = true
	fs.Files["/pack/orders/health-check/order.toml"] = []byte(`
	[order]
	formula = "health-check"
	trigger = "cron"
	schedule = "*/5 * * * *"
	`)

	var logs bytes.Buffer
	opts := ScanOptions{
		DeprecatedPathWarningDedup:  logutil.NewDedup(10),
		DeprecatedPathWarningWriter: &logs,
	}
	for i := 0; i < 2; i++ {
		if _, err := discoverRootWithOptions(fs, ScanRoot{
			Dir:          "/pack/orders",
			FormulaLayer: "/pack/formulas",
		}, opts); err != nil {
			t.Fatalf("discoverRootWithOptions: %v", err)
		}
	}

	if got := strings.Count(logs.String(), "deprecated order path"); got != 1 {
		t.Fatalf("deprecated warning count = %d, want 1; logs=%q", got, logs.String())
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

	logs := captureOrderLogs(t, func() {
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
	})
	if !strings.Contains(logs, "unreadable order path") {
		t.Fatalf("logs = %q, want unreadable order path warning", logs)
	}
}

func TestDiscoverRootLogsUnreadablePathWhenDeprecatedWarningsSuppressed(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/pack/orders/health-check.toml"] = []byte(`
[order]
formula = "health-check"
trigger = "cron"
schedule = "*/5 * * * *"
`)
	fs.Errors["/pack/orders/health-check.toml"] = errors.New("boom")

	logs := captureOrderLogs(t, func() {
		orders, err := discoverRootWithOptions(fs, ScanRoot{
			Dir:          "/pack/orders",
			FormulaLayer: "/pack/formulas",
		}, ScanOptions{SuppressDeprecatedPathWarnings: true})
		if err != nil {
			t.Fatalf("discoverRootWithOptions: %v", err)
		}
		if len(orders) != 0 {
			t.Fatalf("got %d orders, want 0", len(orders))
		}
	})
	if !strings.Contains(logs, "unreadable order path") {
		t.Fatalf("logs = %q, want unreadable order path warning", logs)
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
