package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/fsys"
)

// TestRigAddIncludeCanonicalizesBuiltinPackSource reproduces gascity#3137:
// `gc rig add <path> --include packs/gastown` writes the literal flag value
// (./packs/gastown) into city.toml instead of the canonical, resolvable pack
// import path. Builtin packs are materialized to .gc/system/packs/<name> (and
// are NOT registered in [packs]); the pack resolver joins the import source to
// the city root with no .gc/system/packs fallback (internal/config/pack.go ->
// resolveConfigPath), so ./packs/gastown resolves to <city>/packs/gastown,
// which does not exist — breaking pack expansion citywide.
//
// The --include flag's own --help promises it "writes canonical rig imports".
// This asserts that promise: a --include token naming a materialized builtin
// pack must be written as .gc/system/packs/<name>, not the literal token.
func TestRigAddIncludeCanonicalizesBuiltinPackSource(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	// Materialize a builtin pack at the canonical location only. The literal
	// ./packs/gastown location is intentionally absent.
	packDir := filepath.Join(cityPath, filepath.FromSlash(citylayout.SystemPacksRoot), "gastown")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"),
		[]byte("[pack]\nname = \"gastown\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

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
	// resolve (the pack lives under .gc/system/packs, not ./packs).
	if strings.Contains(cityToml, "./packs/gastown") {
		t.Errorf("city.toml persisted the literal --include value %q; pack expansion will fail citywide:\n%s",
			"./packs/gastown", cityToml)
	}
	// The import source must canonicalize to the materialized pack location.
	wantSource := citylayout.SystemPacksRoot + "/gastown" // .gc/system/packs/gastown
	if !strings.Contains(cityToml, wantSource) {
		t.Fatalf("city.toml import source did not canonicalize to %q (gascity#3137):\n%s", wantSource, cityToml)
	}

	// Belt and suspenders: the written source must resolve to the materialized
	// pack.toml when joined to the city root (mirrors resolveConfigPath).
	resolved := filepath.Join(cityPath, filepath.FromSlash(wantSource), "pack.toml")
	if _, statErr := os.Stat(resolved); statErr != nil {
		t.Fatalf("canonical pack.toml not resolvable at %s: %v", resolved, statErr)
	}
}

// TestRigAddIncludePrefersConfiguredPackOverBuiltin guards the collision case:
// a bare `--include gastown` where "gastown" is BOTH a registered [packs] key
// AND a materialized builtin at .gc/system/packs/gastown. Builtin canonicalization
// must not shadow the explicit [packs] reference — the written import source must
// be the configured [packs] source, not the system pack path. This makes the
// flag's "preserves [packs] references" guarantee true in all cases (gascity#3137).
func TestRigAddIncludePrefersConfiguredPackOverBuiltin(t *testing.T) {
	cityPath := t.TempDir()
	const configuredSource = "https://github.com/example/gastown"
	cityToml := "[workspace]\n\n[packs.gastown]\nsource = \"" + configuredSource + "\"\n"
	writeSchema2RigCity(t, cityPath, "test-city", cityToml, "")

	// Materialize a builtin pack at the canonical location too, so the token
	// resolves both ways and the [packs]-skip guard is what decides the source.
	packDir := filepath.Join(cityPath, filepath.FromSlash(citylayout.SystemPacksRoot), "gastown")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"),
		[]byte("[pack]\nname = \"gastown\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

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
	// The builtin system path must NOT be written as the import source for a
	// token that names a configured pack.
	systemSource := citylayout.SystemPacksRoot + "/gastown" // .gc/system/packs/gastown
	if strings.Contains(cityToml, systemSource) {
		t.Errorf("city.toml persisted the builtin path %q instead of honoring the configured [packs.gastown] reference:\n%s",
			systemSource, cityToml)
	}
}
