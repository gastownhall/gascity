package sling

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// singleSessionAgent builds a config.Agent that does NOT support multiple
// sessions (max_active_sessions = 1). ResolveSlingEnv returns
// GC_SLING_TARGET for this shape.
func singleSessionAgent() config.Agent {
	one := 1
	return config.Agent{Name: "alice", MaxActiveSessions: &one}
}

// multiSessionAgent builds a pool agent that supports multiple sessions
// via a namepool. ResolveSlingEnv must NOT set GC_SLING_TARGET for this
// shape, but MUST still set GC_ARTIFACT_DIR when the bead is a molecule
// member.
func multiSessionAgent(name string) config.Agent {
	return config.Agent{
		Name:          name,
		NamepoolNames: []string{name + "-1", name + "-2"},
	}
}

func slingEnvTestDeps(t *testing.T, cityPath string) SlingDeps {
	t.Helper()
	return SlingDeps{
		CityName: "test-city",
		CityPath: cityPath,
		Cfg:      &config.City{Workspace: config.Workspace{Name: "test-city"}},
		Store:    beads.NewMemStore(),
		StoreRef: "city:test-city",
	}
}

func TestResolveSlingEnv_SingleSession_NoMolecule(t *testing.T) {
	// Free-standing bead (no gc.root_bead_id) should NOT get GC_ARTIFACT_DIR.
	deps := slingEnvTestDeps(t, t.TempDir())
	bead, err := deps.Store.Create(beads.Bead{Title: "standalone"})
	if err != nil {
		t.Fatal(err)
	}

	env := ResolveSlingEnv(singleSessionAgent(), deps, bead.ID)
	if _, ok := env["GC_SLING_TARGET"]; !ok {
		t.Errorf("GC_SLING_TARGET missing for single-session agent, got env=%v", env)
	}
	if _, ok := env["GC_ARTIFACT_DIR"]; ok {
		t.Errorf("GC_ARTIFACT_DIR should not be set for non-molecule bead, got %q", env["GC_ARTIFACT_DIR"])
	}
}

func TestResolveSlingEnv_SingleSession_WithMolecule(t *testing.T) {
	cityPath := t.TempDir()
	deps := slingEnvTestDeps(t, cityPath)
	bead, err := deps.Store.Create(beads.Bead{
		Title:    "step",
		Metadata: map[string]string{"gc.root_bead_id": "root-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	env := ResolveSlingEnv(singleSessionAgent(), deps, bead.ID)

	if _, ok := env["GC_SLING_TARGET"]; !ok {
		t.Errorf("GC_SLING_TARGET missing for single-session agent, got env=%v", env)
	}
	wantDir := filepath.Join(cityPath, ".gc", "molecules", "root-1", "artifacts", bead.ID)
	if got := env["GC_ARTIFACT_DIR"]; got != wantDir {
		t.Errorf("GC_ARTIFACT_DIR = %q, want %q", got, wantDir)
	}
}

func TestResolveSlingEnv_MultiSession_WithMolecule(t *testing.T) {
	// A pool worker claiming a molecule-child bead must receive
	// GC_ARTIFACT_DIR even though it does not receive GC_SLING_TARGET.
	cityPath := t.TempDir()
	deps := slingEnvTestDeps(t, cityPath)
	bead, err := deps.Store.Create(beads.Bead{
		Title:    "design-council iteration 3",
		Metadata: map[string]string{"gc.root_bead_id": "mol-42"},
	})
	if err != nil {
		t.Fatal(err)
	}

	env := ResolveSlingEnv(multiSessionAgent("polecat"), deps, bead.ID)

	if _, ok := env["GC_SLING_TARGET"]; ok {
		t.Errorf("GC_SLING_TARGET must not be set for multi-session agent, got %q", env["GC_SLING_TARGET"])
	}
	wantDir := filepath.Join(cityPath, ".gc", "molecules", "mol-42", "artifacts", bead.ID)
	if got := env["GC_ARTIFACT_DIR"]; got != wantDir {
		t.Errorf("GC_ARTIFACT_DIR = %q, want %q", got, wantDir)
	}
}

func TestResolveSlingEnv_MultiSession_NoMolecule(t *testing.T) {
	// Polecat with a free-standing bead. No molecule context, no pool
	// session name needed. Expect nil env (or empty map).
	deps := slingEnvTestDeps(t, t.TempDir())
	bead, err := deps.Store.Create(beads.Bead{Title: "free"})
	if err != nil {
		t.Fatal(err)
	}

	env := ResolveSlingEnv(multiSessionAgent("polecat"), deps, bead.ID)
	if _, ok := env["GC_SLING_TARGET"]; ok {
		t.Errorf("GC_SLING_TARGET should not be set for multi-session agent, got %q", env["GC_SLING_TARGET"])
	}
	if _, ok := env["GC_ARTIFACT_DIR"]; ok {
		t.Errorf("GC_ARTIFACT_DIR should not be set without molecule context, got %q", env["GC_ARTIFACT_DIR"])
	}
}

func TestResolveSlingEnv_MoleculeCreatesDirectory(t *testing.T) {
	// EnsureArtifactDir should have been invoked as part of env
	// resolution so the path exists when the worker starts.
	cityPath := t.TempDir()
	deps := slingEnvTestDeps(t, cityPath)
	bead, err := deps.Store.Create(beads.Bead{
		Title:    "step",
		Metadata: map[string]string{"gc.root_bead_id": "root-1"},
	})
	if err != nil {
		t.Fatal(err)
	}

	env := ResolveSlingEnv(singleSessionAgent(), deps, bead.ID)
	dir := env["GC_ARTIFACT_DIR"]
	if dir == "" {
		t.Fatal("GC_ARTIFACT_DIR not set")
	}
	// The path should exist — the worker needs to write to it immediately.
	if !dirExists(t, dir) {
		t.Errorf("GC_ARTIFACT_DIR %q does not exist on disk after ResolveSlingEnv", dir)
	}
}

func TestResolveSlingEnv_NilStore(t *testing.T) {
	// Some dispatch paths may call ResolveSlingEnv with a nil store (tests,
	// bare dry-run paths). Must not panic; must still return session
	// target for single-session agents.
	deps := SlingDeps{
		CityName: "test-city",
		CityPath: t.TempDir(),
		Cfg:      &config.City{Workspace: config.Workspace{Name: "test-city"}},
	}

	env := ResolveSlingEnv(singleSessionAgent(), deps, "some-bead")
	if _, ok := env["GC_SLING_TARGET"]; !ok {
		t.Errorf("GC_SLING_TARGET must be set for single-session agent even with nil store, got env=%v", env)
	}
	if _, ok := env["GC_ARTIFACT_DIR"]; ok {
		t.Errorf("GC_ARTIFACT_DIR should not be set when store is nil, got %q", env["GC_ARTIFACT_DIR"])
	}
}

func TestResolveSlingEnv_EmptyBeadID(t *testing.T) {
	// Callers may pass "" for beadID (e.g. formula-only dispatch before
	// root bead is created). Don't attempt molecule lookup.
	deps := slingEnvTestDeps(t, t.TempDir())
	env := ResolveSlingEnv(singleSessionAgent(), deps, "")
	if _, ok := env["GC_ARTIFACT_DIR"]; ok {
		t.Errorf("GC_ARTIFACT_DIR should not be set with empty beadID, got %q", env["GC_ARTIFACT_DIR"])
	}
}

func TestResolveSlingEnv_BeadNotFound(t *testing.T) {
	// If the bead lookup fails (bead closed, deleted, or pre-creation
	// lookup), don't set GC_ARTIFACT_DIR. Don't return an error either —
	// ResolveSlingEnv's contract is best-effort env projection.
	deps := slingEnvTestDeps(t, t.TempDir())
	env := ResolveSlingEnv(singleSessionAgent(), deps, "nonexistent-bead")
	if _, ok := env["GC_ARTIFACT_DIR"]; ok {
		t.Errorf("GC_ARTIFACT_DIR should not be set when bead lookup fails, got %q", env["GC_ARTIFACT_DIR"])
	}
}

func TestResolveSlingEnv_RootBeadIsItself(t *testing.T) {
	// When the bead's gc.root_bead_id equals the bead's own ID (the root
	// of a molecule is itself a work item), the artifact path nests the
	// same ID twice — acceptable and unambiguous.
	cityPath := t.TempDir()
	deps := slingEnvTestDeps(t, cityPath)
	// Create first so we know the assigned ID, then rewrite metadata so
	// gc.root_bead_id matches the bead's own ID.
	bead, err := deps.Store.Create(beads.Bead{Title: "root itself"})
	if err != nil {
		t.Fatal(err)
	}
	if err := deps.Store.SetMetadata(bead.ID, "gc.root_bead_id", bead.ID); err != nil {
		t.Fatal(err)
	}

	env := ResolveSlingEnv(singleSessionAgent(), deps, bead.ID)
	want := filepath.Join(cityPath, ".gc", "molecules", bead.ID, "artifacts", bead.ID)
	if got := env["GC_ARTIFACT_DIR"]; got != want {
		t.Errorf("GC_ARTIFACT_DIR = %q, want %q", got, want)
	}
}

func TestResolveSlingEnv_UnsafeRootBeadID(t *testing.T) {
	// A malformed bead with a path-traversing gc.root_bead_id must not
	// produce a GC_ARTIFACT_DIR that escapes .gc/molecules/.
	deps := slingEnvTestDeps(t, t.TempDir())
	bead, err := deps.Store.Create(beads.Bead{
		Title:    "step",
		Metadata: map[string]string{"gc.root_bead_id": "../escape"},
	})
	if err != nil {
		t.Fatal(err)
	}

	env := ResolveSlingEnv(singleSessionAgent(), deps, bead.ID)
	if got, ok := env["GC_ARTIFACT_DIR"]; ok {
		if strings.Contains(got, "..") {
			t.Errorf("GC_ARTIFACT_DIR contains path traversal: %q", got)
		}
		t.Errorf("GC_ARTIFACT_DIR should be unset for unsafe root ID, got %q", got)
	}
}

// dirExists reports whether path is an existing directory on disk.
func dirExists(t *testing.T, path string) bool {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return info.IsDir()
}
