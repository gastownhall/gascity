//go:build gascity_native_beads

package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	_ "modernc.org/sqlite"
)

// writeMinimalDoltliteFixture creates just enough on disk for
// beads.NewDoltliteReadStore to succeed: a .beads/metadata.json naming the
// doltlite database, and an openable (if schema-empty) SQLite file at the
// path it resolves to. No bead rows or schema are needed: the fixture backs
// wrapping decisions, not the read store's query behavior, which
// internal/beads/doltlite_read_store_test.go already covers.
func writeMinimalDoltliteFixture(t *testing.T, dir string) {
	t.Helper()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	meta := []byte(`{"backend":"doltlite","database":"doltlite","dolt_database":"hq"}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), meta, 0o600); err != nil {
		t.Fatalf("write metadata.json: %v", err)
	}
	dbDir := filepath.Join(beadsDir, "doltlite")
	if err := os.MkdirAll(dbDir, 0o755); err != nil {
		t.Fatalf("mkdir doltlite dir: %v", err)
	}
	dbPath := filepath.Join(dbDir, "hq.db")
	db, err := sql.Open("sqlite", dbPath+"?_busy_timeout=10000")
	if err != nil {
		t.Fatalf("open doltlite fixture db: %v", err)
	}
	defer func() { _ = db.Close() }()
	// A no-op statement forces the driver to actually create the file on
	// disk; sql.Open alone is lazy and may not.
	if _, err := db.Exec("CREATE TABLE IF NOT EXISTS t(x)"); err != nil {
		t.Fatalf("materialize doltlite fixture db: %v", err)
	}
}

// TestOpenBdStoreAtScopedSkipsDoltliteOptimizationUnderNativeTransportOff
// proves "off" refuses the GC_NATIVE_DOLTLITE_BEADS read optimization even with
// the env var set and a real doltlite index file on disk: the optimization
// wraps BdStore with a direct-SQL reader that bypasses the bd subprocess, and
// "off" promises every read goes through bd. Both spellings of "off" are
// covered, the per-city value and the deprecated GC_BEADS_FORCE_FALLBACK alias.
// The "auto" leg proves the fixture really exercises the optimization; without
// it the refusals would prove nothing.
func TestOpenBdStoreAtScopedSkipsDoltliteOptimizationUnderNativeTransportOff(t *testing.T) {
	t.Setenv(nativeDoltliteBeadsEnv, "1")
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"t\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeMinimalDoltliteFixture(t, cityDir)

	off, err := openBdStoreAtScoped(cityDir, cityDir, &config.City{}, false, beads.NativeTransportOff)
	if err != nil {
		t.Fatalf("openBdStoreAtScoped(off): %v", err)
	}
	if _, isPlainBd := off.(*beads.BdStore); !isPlainBd {
		t.Fatalf("openBdStoreAtScoped(off) returned %T, want a plain *beads.BdStore (the doltlite optimization must be refused under off)", off)
	}

	auto, err := openBdStoreAtScoped(cityDir, cityDir, &config.City{}, false, beads.NativeTransportAuto)
	if err != nil {
		t.Fatalf("openBdStoreAtScoped(auto): %v", err)
	}
	if _, isOptimized := auto.(*beads.DoltliteReadStore); !isOptimized {
		t.Fatalf("openBdStoreAtScoped(auto) returned %T, want *beads.DoltliteReadStore: the fixture/env setup does not actually exercise the optimization, so the off-mode assertion above proves nothing", auto)
	}

	// The deprecated process-wide alias overrides the city's "auto".
	t.Setenv("GC_BEADS_FORCE_FALLBACK", "1")
	aliased, err := openBdStoreAtScoped(cityDir, cityDir, &config.City{}, false, beads.NativeTransportAuto)
	if err != nil {
		t.Fatalf("openBdStoreAtScoped(auto, GC_BEADS_FORCE_FALLBACK=1): %v", err)
	}
	if _, isPlainBd := aliased.(*beads.BdStore); !isPlainBd {
		t.Fatalf("openBdStoreAtScoped(auto) with GC_BEADS_FORCE_FALLBACK=1 returned %T, want a plain *beads.BdStore", aliased)
	}
}
