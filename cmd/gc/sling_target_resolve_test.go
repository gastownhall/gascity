package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// namepoolSlingCity writes a city with one rig pool, frontend/polecat
// (max_active_sessions = 2), whose members are named from a namepool file
// listing furiosa, rictus and nux. It returns the city and rig directories
// with file-backed stores scoped to each.
func namepoolSlingCity(t *testing.T) (cityDir, rigDir string) {
	t.Helper()
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")

	cityDir = t.TempDir()
	rigDir = filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatalf("ensureScopedFileStoreLayout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(city): %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(rigDir); err != nil {
		t.Fatalf("ensurePersistedScopeLocalFileStore(rig): %v", err)
	}
	// Agent.NamepoolNames is loaded from this file at config load.
	if err := os.WriteFile(filepath.Join(cityDir, "polecat-names.txt"), []byte("furiosa\nrictus\nnux\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(namepool): %v", err)
	}
	cityToml := `[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "FE"

[[agent]]
name = "polecat"
dir = "frontend"
min_active_sessions = 0
max_active_sessions = 2
namepool = "polecat-names.txt"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	t.Chdir(cityDir)
	t.Setenv("GC_CITY_PATH", cityDir)
	return cityDir, rigDir
}

// listStoreBeads returns every bead in the file store scoped to dir.
func listStoreBeads(t *testing.T, dir, cityDir string) []beads.Bead {
	t.Helper()
	store, err := openStoreAtForCity(dir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(%s): %v", dir, err)
	}
	all, err := store.List(beads.ListQuery{AllowScan: true, Sort: beads.SortCreatedAsc})
	if err != nil {
		t.Fatalf("List(%s): %v", dir, err)
	}
	return all
}

// TestCmdSlingNamepoolMemberTargetRoutesToPool covers #5724: gc status names
// namepool pool members by their themed names (frontend/furiosa), so gc sling
// must accept that name and route the work to the base pool.
func TestCmdSlingNamepoolMemberTargetRoutesToPool(t *testing.T) {
	cityDir, rigDir := namepoolSlingCity(t)

	var stdout, stderr bytes.Buffer
	code := cmdSling([]string{"frontend/furiosa", "ship feature"}, false, false, true, "", nil, "", true, false, false, "", false, false, false, "", "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdSling(frontend/furiosa) = %d, want 0; stderr: %s", code, stderr.String())
	}

	rigBeads := listStoreBeads(t, rigDir, cityDir)
	if len(rigBeads) != 1 {
		t.Fatalf("rig store bead count = %d, want 1: %#v", len(rigBeads), rigBeads)
	}
	if got := rigBeads[0].Title; got != "ship feature" {
		t.Errorf("rig bead title = %q, want %q", got, "ship feature")
	}
	if got := rigBeads[0].Metadata["gc.routed_to"]; got != "frontend/polecat" {
		t.Errorf("rig bead gc.routed_to = %q, want frontend/polecat", got)
	}
	if cityBeads := listStoreBeads(t, cityDir, cityDir); len(cityBeads) != 0 {
		t.Errorf("city store bead count = %d, want 0: %#v", len(cityBeads), cityBeads)
	}
}
