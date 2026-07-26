package main

import (
	"bytes"
	"fmt"
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
	wantSource, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	if !strings.Contains(cityToml, wantSource) {
		t.Fatalf("city.toml import source did not canonicalize to %q (gascity#3137):\n%s", wantSource, cityToml)
	}
	// The persisted rig import must carry the canonical bundled pin, not a
	// version-less import. Without the pin in city.toml, regenerating or
	// losing packs.lock resolves the import as an ordinary latest-version
	// remote import, and "gc import upgrade" treats it as unconstrained —
	// either path silently replaces the builtin the user asked for.
	wantVersion := fmt.Sprintf("version = %q", config.PublicGastownPackVersion)
	if !strings.Contains(cityToml, wantVersion) {
		t.Fatalf("city.toml rig import is not pinned at the canonical bundled version (want %s):\n%s", wantVersion, cityToml)
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

// TestRigAddDefaultRigImportsPersistPinnedBundledVersion guards the sibling
// path of the explicit --include pin hardening: `gc rig add` WITHOUT
// --include composes the rig's imports from root-pack defaults (plus legacy
// default_rig_includes), and a version-less bundled-source entry arriving on
// that path must persist the canonical pin into the city.toml rig import —
// exactly like the explicit path. Without it, regenerating or losing
// packs.lock resolves the import as latest and "gc import upgrade" treats it
// as unconstrained, silently replacing the builtin.
func TestRigAddDefaultRigImportsPersistPinnedBundledVersion(t *testing.T) {
	bundledSource, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}

	cityPath := t.TempDir()
	// A hand-authored version-less bundled default-rig import (gc init
	// writes pinned ones; the version-less form is the exposure).
	cityToml := fmt.Sprintf("[workspace]\n\n[defaults.rig.imports.gastown]\nsource = %q\n", bundledSource)
	writeSchema2RigCity(t, cityPath, "test-city", cityToml, "")

	rigPath := filepath.Join(t.TempDir(), "myproj")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS", "bd")

	var stdout, stderr bytes.Buffer
	// No --include: the rig inherits the default-rig imports.
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd returned %d, stderr: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	cityTomlAfter := string(data)
	if !strings.Contains(cityTomlAfter, bundledSource) {
		t.Fatalf("city.toml rig import did not inherit the default-rig bundled source %q:\n%s", bundledSource, cityTomlAfter)
	}
	wantVersion := fmt.Sprintf("version = %q", config.PublicGastownPackVersion)
	if !strings.Contains(cityTomlAfter, wantVersion) {
		t.Fatalf("city.toml default-rig bundled import is not pinned at the canonical bundled version (want %s):\n%s", wantVersion, cityTomlAfter)
	}

	lockData, err := os.ReadFile(filepath.Join(cityPath, "packs.lock"))
	if err != nil {
		t.Fatalf("packs.lock after rig add: %v", err)
	}
	if !strings.Contains(string(lockData), strings.TrimPrefix(config.PublicGastownPackVersion, "sha:")) {
		t.Fatalf("packs.lock missing public gastown pin after default-rig add:\n%s", lockData)
	}
}

// TestEnsureBundledRigImportsInstalledDoesNotMutateInput pins the helper's
// contract: pinning happens on the returned copy, never by side effect on
// the caller's slice. Both rig-add synthesis paths persist what the helper
// returns, so an in-place mutation would be an implicit dependency a future
// caller could miss.
func TestEnsureBundledRigImportsInstalledDoesNotMutateInput(t *testing.T) {
	bundledSource, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	input := []config.BoundImport{{Binding: "gastown", Import: config.Import{Source: bundledSource}}}
	pinned, _, err := ensureBundledRigImportsInstalled(cityPath, input)
	if err != nil {
		t.Fatalf("ensureBundledRigImportsInstalled: %v", err)
	}
	if got := input[0].Import.Version; got != "" {
		t.Errorf("input slice was mutated: version = %q, want \"\"", got)
	}
	if len(pinned) != 1 || pinned[0].Import.Version != config.PublicGastownPackVersion {
		t.Errorf("returned imports = %+v, want gastown pinned at %s", pinned, config.PublicGastownPackVersion)
	}
}

// TestEnsureBundledRigImportsInstalledDefersLockfileWrite pins the
// state-safety contract behind the deferred commit: ensureBundledRigImportsInstalled
// must resolve eagerly but never write packs.lock by itself, so a rig add
// that aborts before the city config write leaves packs.lock untouched. The
// returned commit performs the write, and only then does packs.lock gain the
// bundled pin. This is the regression guard for the "gc rig add advances
// packs.lock before the city.toml-last commit boundary" finding.
func TestEnsureBundledRigImportsInstalledDefersLockfileWrite(t *testing.T) {
	bundledSource, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	lockPath := filepath.Join(cityPath, "packs.lock")
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("packs.lock unexpectedly present before rig add: stat err = %v", err)
	}

	input := []config.BoundImport{{Binding: "gastown", Import: config.Import{Source: bundledSource}}}
	pinned, commit, err := ensureBundledRigImportsInstalled(cityPath, input)
	if err != nil {
		t.Fatalf("ensureBundledRigImportsInstalled: %v", err)
	}
	if commit == nil {
		t.Fatal("expected a non-nil commit for a bundled import")
	}
	if len(pinned) != 1 || pinned[0].Import.Version != config.PublicGastownPackVersion {
		t.Errorf("returned imports = %+v, want gastown pinned at %s", pinned, config.PublicGastownPackVersion)
	}

	// The resolve phase must not have written packs.lock: that is what keeps
	// an aborted rig add from leaving the lockfile advanced.
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("ensureBundledRigImportsInstalled wrote packs.lock before commit: stat err = %v", err)
	}

	if err := commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	lockData, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("packs.lock after commit: %v", err)
	}
	if !strings.Contains(string(lockData), strings.TrimPrefix(config.PublicGastownPackVersion, "sha:")) {
		t.Fatalf("packs.lock missing public gastown pin after commit:\n%s", lockData)
	}
}

// TestSnapshotRigAddTopologyFilesCoversPacksLock proves packs.lock is part of
// the rig-add rollback snapshot, so the deferred bundled-import commit is
// atomic with the rest of rig add: a packs.lock created (or advanced) during
// the add is restored to its pre-add state when a later step rolls back.
func TestSnapshotRigAddTopologyFilesCoversPacksLock(t *testing.T) {
	cityPath := t.TempDir()
	writeSchema2RigCity(t, cityPath, "test-city", "[workspace]\n", "")

	lockPath := filepath.Join(cityPath, "packs.lock")
	cfg, err := loadCityConfigForEditFS(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("loadCityConfigForEditFS: %v", err)
	}

	// Case 1: packs.lock absent before the add — rollback must remove a
	// lockfile the commit created.
	snapshots, err := snapshotRigAddTopologyFiles(fsys.OSFS{}, cityPath, cfg)
	if err != nil {
		t.Fatalf("snapshotRigAddTopologyFiles: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("schema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreSnapshots(fsys.OSFS{}, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("rollback did not remove the created packs.lock: stat err = %v", err)
	}

	// Case 2: packs.lock present before the add — rollback must restore the
	// original contents, not the advanced ones.
	const original = "schema = 1\n# original\n"
	if err := os.WriteFile(lockPath, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	snapshots, err = snapshotRigAddTopologyFiles(fsys.OSFS{}, cityPath, cfg)
	if err != nil {
		t.Fatalf("snapshotRigAddTopologyFiles: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("schema = 1\n# advanced\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := restoreSnapshots(fsys.OSFS{}, snapshots); err != nil {
		t.Fatalf("restoreSnapshots: %v", err)
	}
	got, err := os.ReadFile(lockPath)
	if err != nil {
		t.Fatalf("read packs.lock after rollback: %v", err)
	}
	if string(got) != original {
		t.Fatalf("rollback did not restore original packs.lock:\ngot:  %q\nwant: %q", got, original)
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
	bundledSource, ok := builtinpacks.CanonicalImportSource("gastown")
	if !ok {
		t.Fatal("bundled gastown pack not registered")
	}
	if strings.Contains(cityToml, bundledSource) {
		t.Errorf("city.toml persisted the bundled source %q instead of honoring the configured [packs.gastown] reference:\n%s",
			bundledSource, cityToml)
	}
}
