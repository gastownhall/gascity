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
// path it resolves to. No bead rows or schema are needed — this proves
// WRAPPING decisions (Finding 9), not the read store's query behavior, which
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
// proves Finding 9: beads.native_transport="off" promises this city's stores
// always use BdStore, the bd CLI subprocess — so the GC_NATIVE_DOLTLITE_BEADS
// experiment (which wraps that same BdStore with a direct-SQL reader,
// bypassing the subprocess for reads) must not kick in under "off", even
// when the env var is set and a real doltlite index file is on disk. Under
// "auto"/unset, by contrast, the wrap is exactly what the env var promises
// and must still happen — the negative case alone would not prove the "off"
// skip is doing anything.
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
}
