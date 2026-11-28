package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
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

// TestBuiltInSlingNamepoolMemberTargetNormalizesRoutedTo is the namepool twin
// of TestBuiltInSlingSlotSuffixedTargetNormalizesRoutedTo: a binding-qualified
// namepool member target resolves to the member identity, and slinging it
// records the base pool in gc.routed_to.
func TestBuiltInSlingNamepoolMemberTargetNormalizesRoutedTo(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "gascity", Path: "/tmp/gascity", Prefix: "gc"}},
		Agents:    []config.Agent{namepoolAgentForNudgeTest()},
	}
	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	store := newSlingTestStore()
	deps = slingDepsWithStore(deps, store)
	deps.StoreRef = "rig:gascity"

	created, err := store.Create(beads.Bead{Title: "namepool-routed work", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	target, ok := resolveSlingTarget(cfg, "gascity/gastown.rictus", "")
	if !ok {
		t.Fatal("resolveSlingTarget(gascity/gastown.rictus) failed")
	}
	if got := target.QualifiedName(); got != "gascity/gastown.rictus" {
		t.Fatalf("resolved target QualifiedName = %q, want gascity/gastown.rictus", got)
	}
	if got := agentutil.RoutedToIdentity(&target); got != "gascity/gastown.polecat" {
		t.Fatalf("RoutedToIdentity = %q, want gascity/gastown.polecat", got)
	}

	code := doSling(testOpts(target, created.ID), deps, &fakeQuerier{bead: created}, stdout, stderr)
	if code != 0 {
		t.Fatalf("doSling returned %d; stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	routed, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get routed bead: %v", err)
	}
	if got := routed.Metadata["gc.routed_to"]; got != "gascity/gastown.polecat" {
		t.Fatalf("gc.routed_to = %q, want gascity/gastown.polecat (member name should normalize to the pool)", got)
	}
}

// TestCmdSlingNamepoolTargetOutsidePoolFailsResolve guards against
// over-acceptance: a namepool name past the pool's bound and a name not in
// the namepool are not pool members, so gc sling refuses them and creates
// nothing.
func TestCmdSlingNamepoolTargetOutsidePoolFailsResolve(t *testing.T) {
	for _, target := range []string{"frontend/nux", "frontend/ghost"} {
		t.Run(target, func(t *testing.T) {
			cityDir, rigDir := namepoolSlingCity(t)

			var stdout, stderr bytes.Buffer
			code := cmdSlingWithJSON([]string{target, "ship feature"}, false, false, true, "", nil, "", true, false, false, "", false, false, false, "", "", true, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("cmdSlingWithJSON(%s) = %d, want 1; stdout=%s stderr=%s", target, code, stdout.String(), stderr.String())
			}
			var got cliJSONErrorOutput
			if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
				t.Fatalf("stdout is not a JSON error: %v (%q)", err, stdout.String())
			}
			if got.Error.Code != "target_resolve_failed" {
				t.Errorf("error code = %q, want target_resolve_failed (message %q)", got.Error.Code, got.Error.Message)
			}
			if rigBeads := listStoreBeads(t, rigDir, cityDir); len(rigBeads) != 0 {
				t.Errorf("rig store bead count = %d, want 0: %#v", len(rigBeads), rigBeads)
			}
			if cityBeads := listStoreBeads(t, cityDir, cityDir); len(cityBeads) != 0 {
				t.Errorf("city store bead count = %d, want 0: %#v", len(cityBeads), cityBeads)
			}
		})
	}
}
