package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestRigAddIncludeCanonicalizesBuiltinPackSource reproduces gascity#3137:
// `gc rig add <path> --include packs/gastown` writes the literal flag value
// (./packs/gastown) into city.toml instead of a resolvable pack import.
// Builtin packs compose from the user-global repo cache via their bundled
// remote source; the pack resolver joins local import sources to the city
// root (internal/config/pack.go -> resolveConfigPath), so ./packs/gastown
// resolves to <city>/packs/gastown, which does not exist — breaking pack
// expansion citywide.
//
// The --include flag's own --help promises it "writes canonical rig imports".
// This asserts that promise: a --include token naming a bundled builtin pack
// must canonicalize to the pack's bundled remote source (with a lock entry
// so it resolves offline), not the literal token.
func TestRigAddIncludeCanonicalizesBuiltinPackSource(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	rigPath := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	// Exactly the form documented in `gc rig add --help`.
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"packs/gastown"}, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cityToml := string(data)

	// The literal flag value must NOT be persisted verbatim — it does not
	// resolve (no <city>/packs/gastown exists).
	if strings.Contains(cityToml, "./packs/gastown") {
		t.Errorf("city.toml persisted the literal --include value %q; pack expansion will fail citywide:\n%s",
			"./packs/gastown", cityToml)
	}
	// The import source must canonicalize to the bundled remote source.
	wantSource, ok := builtinpacks.Source("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	if !strings.Contains(cityToml, wantSource) {
		t.Fatalf("city.toml import source did not canonicalize to %q (gascity#3137):\n%s", wantSource, cityToml)
	}

	// Belt and suspenders: the canonical source must be pinned in packs.lock
	// at the public registry version so it resolves offline from the
	// bundled cache.
	lockData, err := os.ReadFile(filepath.Join(cityPath, "packs.lock"))
	if err != nil {
		t.Fatalf("packs.lock after rig add: %v", err)
	}
	if !strings.Contains(string(lockData), strings.TrimPrefix(config.PublicGastownPackVersion, "sha:")) {
		t.Fatalf("packs.lock missing public gastown pin after rig add:\n%s", lockData)
	}
}

// TestRigAddIncludePrefersConfiguredPackOverBuiltin guards the collision case:
// a bare `--include gastown` where "gastown" is BOTH a registered [packs] key
// AND a bundled builtin pack. Builtin canonicalization must not shadow the
// explicit [packs] reference — the written import source must be the
// configured [packs] source, not the bundled remote source. This makes the
// flag's "preserves [packs] references" guarantee true in all cases
// (gascity#3137).
func TestRigAddIncludePrefersConfiguredPackOverBuiltin(t *testing.T) {
	cityPath := t.TempDir()
	const configuredSource = "https://github.com/example/gastown"
	cityToml := "[workspace]\n\n[packs.gastown]\nsource = \"" + configuredSource + "\"\n"
	writeSchema2RigCity(t, cityPath, "test-city", cityToml, "")

	rigPath := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, []string{"gastown"}, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cityToml = string(data)

	// The configured [packs] source must win — the import must reference it.
	if !strings.Contains(cityToml, configuredSource) {
		t.Errorf("city.toml dropped the configured [packs.gastown] source %q; builtin canonicalization shadowed the explicit reference:\n%s",
			configuredSource, cityToml)
	}
	// The bundled remote source must NOT be written as the import source for
	// a token that names a configured pack.
	bundledSource, ok := builtinpacks.Source("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	if strings.Contains(cityToml, bundledSource) {
		t.Errorf("city.toml persisted the bundled source %q instead of honoring the configured [packs.gastown] reference:\n%s",
			bundledSource, cityToml)
	}
}
