package main

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/materialize"
)

// TestInternalMaterializeSkillsMaterializesClaude exercises the happy
// path: a claude-provider agent in a city with a pack skill ends up
// with a symlink at <workdir>/.claude/skills/<name> pointing at the
// city skill directory.
func TestInternalMaterializeSkillsMaterializesClaude(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir()) // isolate bootstrap discovery
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	// Pack.toml enables PackSkillsDir discovery. Without it, the
	// materializer sees no shared city catalog and the sink stays empty.
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	writeBuiltinImportsFixture(t, cityDir, "core")
	writeSkillSource(t, filepath.Join(cityDir, "skills", "plan"))

	workdir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	// Symlink should exist at <workdir>/.claude/skills/plan -> <cityDir>/skills/plan
	link := filepath.Join(workdir, ".claude", "skills", "plan")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat(%s): %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
	tgt, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	wantTarget := filepath.Join(cityDir, "skills", "plan")
	if tgt != wantTarget {
		t.Fatalf("symlink target = %q, want %q", tgt, wantTarget)
	}

	absWorkdir, err := filepath.Abs(workdir)
	if err != nil {
		t.Fatalf("filepath.Abs(%s): %v", workdir, err)
	}
	sinkDir, ok := materialize.VendorSink("claude")
	if !ok {
		t.Fatal("materialize.VendorSink(claude) = not found")
	}
	wantStdout := fmt.Sprintf(
		"materialized 8 skill(s) into %s: core.gc-agents, core.gc-city, core.gc-dashboard, core.gc-dispatch, core.gc-mail, core.gc-rigs, core.gc-work, plan\n",
		filepath.Join(absWorkdir, sinkDir),
	)
	if stdout.String() != wantStdout {
		t.Errorf("stdout = %q, want %q", stdout.String(), wantStdout)
	}
}

func TestInternalMaterializeSkillsMaterializesImportedSharedSkills(t *testing.T) {
	clearGCEnv(t)
	rootDir := t.TempDir()
	cityDir := filepath.Join(rootDir, "city")
	packDir := filepath.Join(rootDir, "helper")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"city\"\nversion = \"0.1.0\"\nschema = 2\n\n[imports.helper]\nsource = \"../helper\"\n")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(helper): %v", err)
	}
	if err := os.WriteFile(filepath.Join(packDir, "pack.toml"), []byte("[pack]\nname = \"helper\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(helper/pack.toml): %v", err)
	}
	writeSkillSource(t, filepath.Join(packDir, "skills", "plan"))

	workdir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	link := filepath.Join(workdir, ".claude", "skills", "helper.plan")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat(%s): %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
	tgt, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("readlink: %v", err)
	}
	wantTarget := filepath.Join(packDir, "skills", "plan")
	if tgt != wantTarget {
		t.Fatalf("symlink target = %q, want %q", tgt, wantTarget)
	}
}

func TestInternalMaterializeSkillsCityScopedDirMatchingRigDoesNotMaterializeRigSharedSkills(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	helperDir := filepath.Join(cityDir, "assets", "helper")
	rigDir := filepath.Join(cityDir, "fe")
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	if err := os.MkdirAll(helperDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(helper): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[rigs]]
name = "fe"

[rigs.imports.helper]
source = "./assets/helper"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	writeMaterializeTestCityFile(t, filepath.Join(cityDir, ".gc"), "site.toml", fmt.Sprintf("[[rig]]\nname = \"fe\"\npath = %q\n", rigDir))
	writeMaterializeTestMayor(t, cityDir, "scope = \"city\"\ndir = \"fe\"\nprovider = \"claude\"\nstart_command = \"echo\"\n")
	writeMaterializeTestCityFile(t, helperDir, "pack.toml", "[pack]\nname = \"helper\"\nversion = \"0.1.0\"\nschema = 2\n")
	writeSkillSource(t, filepath.Join(helperDir, "skills", "plan"))

	workdir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if _, err := os.Lstat(filepath.Join(workdir, ".claude", "skills", "helper.plan")); !os.IsNotExist(err) {
		t.Fatalf("city-scoped agent should not receive rig-shared skill, lstat err=%v", err)
	}
}

func TestInternalMaterializeSkillsSharedCatalogFailurePrunesStaleSharedSymlink(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	skillsDir := filepath.Join(cityDir, "skills")
	writeSkillSource(t, filepath.Join(skillsDir, "plan"))

	workdir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("initial exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	link := filepath.Join(workdir, ".claude", "skills", "plan")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("initial shared symlink missing: %v", err)
	}

	if err := os.Chmod(skillsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skillsDir, 0o755) })
	if _, err := os.ReadDir(skillsDir); err == nil {
		t.Skip("environment ignores chmod 000 (likely running as root)")
	}

	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("second exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stderr.String(), "shared skill catalog unavailable") {
		t.Fatalf("stderr = %q, want shared catalog warning", stderr.String())
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("stale shared symlink should be pruned on catalog failure, lstat err=%v", err)
	}
}

func TestInternalMaterializeSkillsUsesSharedCatalogSnapshotEnvWhenLiveCatalogFails(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	skillsDir := filepath.Join(cityDir, "skills")
	writeSkillSource(t, filepath.Join(skillsDir, "plan"))
	sharedCat, err := materialize.LoadCityCatalog(skillsDir)
	if err != nil {
		t.Fatalf("LoadCityCatalog: %v", err)
	}
	snapshot, err := encodeSharedCatalogSnapshot(sharedCat)
	if err != nil {
		t.Fatalf("encodeSharedCatalogSnapshot: %v", err)
	}
	t.Setenv(sharedSkillCatalogSnapshotEnvVar, snapshot)

	if err := os.Chmod(skillsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skillsDir, 0o755) })
	if _, err := os.ReadDir(skillsDir); err == nil {
		t.Skip("environment ignores chmod 000 (likely running as root)")
	}

	workdir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	link := filepath.Join(workdir, ".claude", "skills", "plan")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat(%s): %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
	if strings.Contains(stderr.String(), "shared skill catalog unavailable") {
		t.Fatalf("snapshot-backed run should not reload the live catalog, stderr=%q", stderr.String())
	}
}

// TestInternalMaterializeSkillsUsesSharedCatalogSnapshotFile verifies the
// post-fix path: when the snapshot is provided via --shared-catalog-snapshot-file,
// the materializer reads the base64 blob from disk instead of the env or
// inline argv. This is the path used by stage-2 PreStart for catalogs
// large enough that env injection would overflow tmux's imsg buffer.
func TestInternalMaterializeSkillsUsesSharedCatalogSnapshotFile(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	skillsDir := filepath.Join(cityDir, "skills")
	writeSkillSource(t, filepath.Join(skillsDir, "plan"))
	sharedCat, err := materialize.LoadCityCatalog(skillsDir)
	if err != nil {
		t.Fatalf("LoadCityCatalog: %v", err)
	}
	snapshot, err := encodeSharedCatalogSnapshot(sharedCat)
	if err != nil {
		t.Fatalf("encodeSharedCatalogSnapshot: %v", err)
	}
	snapshotFile := filepath.Join(t.TempDir(), "snapshot.b64")
	if err := os.WriteFile(snapshotFile, []byte(snapshot), 0o600); err != nil {
		t.Fatalf("WriteFile(snapshot): %v", err)
	}

	// Make the live catalog unreadable so the test definitively proves the
	// snapshot-file path is what produced the materialization (not a
	// silent fallback to disk).
	if err := os.Chmod(skillsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skillsDir, 0o755) })
	if _, err := os.ReadDir(skillsDir); err == nil {
		t.Skip("environment ignores chmod 000 (likely running as root)")
	}

	workdir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
		"--shared-catalog-snapshot-file", snapshotFile,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	link := filepath.Join(workdir, ".claude", "skills", "plan")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat(%s): %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
	if strings.Contains(stderr.String(), "shared skill catalog unavailable") {
		t.Fatalf("snapshot-file run should not reload the live catalog, stderr=%q", stderr.String())
	}
}

// TestInternalMaterializeSkillsUsesDefaultSharedCatalogSnapshotFile verifies
// the upgrade-compatible path used by resolveTemplate: materialize-skills is
// invoked with its legacy flags, then discovers the staged snapshot via the
// deterministic workdir-local path.
func TestInternalMaterializeSkillsUsesDefaultSharedCatalogSnapshotFile(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	skillsDir := filepath.Join(cityDir, "skills")
	writeSkillSource(t, filepath.Join(skillsDir, "plan"))
	sharedCat, err := materialize.LoadCityCatalog(skillsDir)
	if err != nil {
		t.Fatalf("LoadCityCatalog: %v", err)
	}
	snapshot, err := encodeSharedCatalogSnapshot(sharedCat)
	if err != nil {
		t.Fatalf("encodeSharedCatalogSnapshot: %v", err)
	}

	workdir := t.TempDir()
	snapshotFile := skillSnapshotFilePath(workdir, "mayor")
	if err := os.MkdirAll(filepath.Dir(snapshotFile), 0o700); err != nil {
		t.Fatalf("MkdirAll(snapshot dir): %v", err)
	}
	if err := os.WriteFile(snapshotFile, []byte(snapshot), 0o600); err != nil {
		t.Fatalf("WriteFile(snapshot): %v", err)
	}

	if err := os.Chmod(skillsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skillsDir, 0o755) })
	if _, err := os.ReadDir(skillsDir); err == nil {
		t.Skip("environment ignores chmod 000 (likely running as root)")
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	link := filepath.Join(workdir, ".claude", "skills", "plan")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat(%s): %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
	if strings.Contains(stderr.String(), "shared skill catalog unavailable") {
		t.Fatalf("default snapshot-file run should not reload the live catalog, stderr=%q", stderr.String())
	}
}

func TestInternalMaterializeSkillsExplicitSnapshotFileOverridesInlineSnapshot(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	skillsDir := filepath.Join(cityDir, "skills")
	writeSkillSource(t, filepath.Join(skillsDir, "plan"))
	fileSnapshotCat, err := materialize.LoadCityCatalog(skillsDir)
	if err != nil {
		t.Fatalf("LoadCityCatalog: %v", err)
	}
	fileSnapshot, err := encodeSharedCatalogSnapshot(fileSnapshotCat)
	if err != nil {
		t.Fatalf("encodeSharedCatalogSnapshot(file): %v", err)
	}
	inlineSnapshot, err := encodeSharedCatalogSnapshot(materialize.CityCatalog{})
	if err != nil {
		t.Fatalf("encodeSharedCatalogSnapshot(inline): %v", err)
	}
	snapshotFile := filepath.Join(t.TempDir(), "snapshot.b64")
	if err := os.WriteFile(snapshotFile, []byte(fileSnapshot), 0o600); err != nil {
		t.Fatalf("WriteFile(snapshot): %v", err)
	}

	if err := os.Chmod(skillsDir, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(skillsDir, 0o755) })
	if _, err := os.ReadDir(skillsDir); err == nil {
		t.Skip("environment ignores chmod 000 (likely running as root)")
	}

	workdir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
		"--shared-catalog-snapshot", inlineSnapshot,
		"--shared-catalog-snapshot-file", snapshotFile,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	link := filepath.Join(workdir, ".claude", "skills", "plan")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("explicit file should win over inline snapshot, lstat(%s): %v", link, err)
	}
}

// TestInternalMaterializeSkillsSnapshotFileMissingFallsBackToLiveCatalog
// verifies graceful degradation when the snapshot file is missing — the
// command must not abort; it should warn and try the live catalog instead.
func TestInternalMaterializeSkillsSnapshotFileMissingFallsBackToLiveCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	skillsDir := filepath.Join(cityDir, "skills")
	writeSkillSource(t, filepath.Join(skillsDir, "plan"))

	workdir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
		"--shared-catalog-snapshot-file", "/nonexistent/path/that/does/not/exist.b64",
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stderr.String(), "shared-catalog-snapshot-file") {
		t.Errorf("expected warning about unreadable snapshot file, got stderr=%q", stderr.String())
	}
	// Live catalog fallback should still produce the materialization.
	link := filepath.Join(workdir, ".claude", "skills", "plan")
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("lstat(%s): %v (live catalog fallback should have materialized)", link, err)
	}
}

func TestInternalMaterializeSkillsInvalidSharedCatalogSnapshotFallsBackToLiveCatalog(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv(sharedSkillCatalogSnapshotEnvVar, "not-base64")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	writeSkillSource(t, filepath.Join(cityDir, "skills", "plan"))

	workdir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}

	link := filepath.Join(workdir, ".claude", "skills", "plan")
	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat(%s): %v", link, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is not a symlink", link)
	}
	if !strings.Contains(stderr.String(), "decoding shared catalog snapshot") {
		t.Fatalf("stderr = %q, want snapshot decode warning", stderr.String())
	}
	if strings.Contains(stderr.String(), "shared skill catalog unavailable") {
		t.Fatalf("invalid snapshot should fall back to live catalog, stderr=%q", stderr.String())
	}
}

// TestInternalMaterializeSkillsUnsupportedProvider confirms that an
// agent with no vendor sink (e.g. copilot in v0.15.1) exits 0 and logs
// a skip line to stdout — not an error.
func TestInternalMaterializeSkillsUnsupportedProvider(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"copilot\"\nstart_command = \"echo\"\n")
	// Pack.toml enables PackSkillsDir discovery. Without it, the
	// materializer sees no shared city catalog and the sink stays empty.
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")

	workdir := t.TempDir()
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), "has no skill sink") {
		t.Errorf("expected skip line, stdout=%q", stdout.String())
	}
	// No sink directory should have been created.
	for _, vendor := range []string{".copilot", ".claude"} {
		if _, err := os.Stat(filepath.Join(workdir, vendor, "skills")); err == nil {
			t.Errorf("unexpected sink dir created at %s", filepath.Join(workdir, vendor))
		}
	}
}

func TestInternalMaterializeSkillsUnknownAgent(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	// Pack.toml enables PackSkillsDir discovery. Without it, the
	// materializer sees no shared city catalog and the sink stays empty.
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")

	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "nonexistent",
		"--workdir", t.TempDir(),
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("expected non-zero exit for unknown agent; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "unknown agent") {
		t.Errorf("stderr missing 'unknown agent': %q", stderr.String())
	}
}

func TestInternalMaterializeSkillsMissingFlags(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())

	// No --agent
	var stdout, stderr bytes.Buffer
	code := run([]string{"internal", "materialize-skills", "--workdir", t.TempDir()}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit when --agent missing; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--agent is required") {
		t.Errorf("stderr missing '--agent is required': %q", stderr.String())
	}

	// No --workdir
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"internal", "materialize-skills", "--agent", "mayor"}, &stdout, &stderr)
	if code == 0 {
		t.Errorf("expected non-zero exit when --workdir missing; stderr=%q", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--workdir is required") {
		t.Errorf("stderr missing '--workdir is required': %q", stderr.String())
	}
}

// TestInternalMaterializeSkillsSecondRunIsIdempotent exercises the
// supervisor-tick use case: repeated materialization passes converge.
func TestInternalMaterializeSkillsSecondRunIsIdempotent(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	cityToml := `[workspace]
name = "test-city"

[beads]
provider = "file"
`
	writeMaterializeTestCityFile(t, cityDir, "city.toml", cityToml)
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	// Pack.toml enables PackSkillsDir discovery. Without it, the
	// materializer sees no shared city catalog and the sink stays empty.
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")
	writeSkillSource(t, filepath.Join(cityDir, "skills", "plan"))
	writeSkillSource(t, filepath.Join(cityDir, "skills", "code-review"))

	workdir := t.TempDir()

	// Pass 1.
	var stdout, stderr bytes.Buffer
	code := run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pass 1 exit %d: %s", code, stderr.String())
	}

	// Pass 2 — observes converged state, creates nothing new.
	stdout.Reset()
	stderr.Reset()
	code = run([]string{
		"internal", "materialize-skills",
		"--agent", "mayor",
		"--workdir", workdir,
	}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("pass 2 exit %d: %s", code, stderr.String())
	}
	// Both passes should report materialization of both skills (the
	// materializer records a "kept" match as materialized).
	for _, want := range []string{"plan", "code-review"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("pass 2 stdout missing %q: %q", want, stdout.String())
		}
	}
}

func writeSkillSource(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + filepath.Base(dir) + "\ndescription: test\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeMaterializeTestCityFile(t *testing.T, rootDir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(rootDir, name), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", name, err)
	}
}

func writeMaterializeTestMayor(t *testing.T, cityDir, body string) {
	t.Helper()
	agentDir := filepath.Join(cityDir, "agents", "mayor")
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", agentDir, err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "agent.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", filepath.Join("agents", "mayor", "agent.toml"), err)
	}
}

// TestInternalMaterializeSkillsBestEffortSkipsUnknownAgent guards the pre_start
// robustness fix: a session identity that can't be resolved to a config template
// (named/wisp expansions like "rig/executor-vc-eswi4") must be FATAL for a direct
// call but a warn-and-exit-0 skip under --best-effort, so the materialize-skills
// pre_start command can't make session start fatal.
func TestInternalMaterializeSkillsBestEffortSkipsUnknownAgent(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	writeMaterializeTestCityFile(t, cityDir, "city.toml", "[workspace]\nname = \"test-city\"\n\n[beads]\nprovider = \"file\"\n")
	writeMaterializeTestMayor(t, cityDir, "provider = \"claude\"\nstart_command = \"echo\"\n")
	writeMaterializeTestCityFile(t, cityDir, "pack.toml", "[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n")

	workdir := t.TempDir()
	const unresolvable = "rig/executor-vc-eswi4"

	// Direct call (no --best-effort): unresolvable agent is fatal.
	var so, se bytes.Buffer
	if code := run([]string{"internal", "materialize-skills", "--agent", unresolvable, "--workdir", workdir}, &so, &se); code == 0 {
		t.Fatalf("unknown agent without --best-effort: got exit 0, want non-zero; stderr=%q", se.String())
	}

	// --best-effort: warn and exit 0 so pre_start (session start) is not fatal.
	so.Reset()
	se.Reset()
	if code := run([]string{"internal", "materialize-skills", "--best-effort", "--agent", unresolvable, "--workdir", workdir}, &so, &se); code != 0 {
		t.Fatalf("unknown agent with --best-effort: exit %d, want 0; stderr=%q", code, se.String())
	}
	if !strings.Contains(se.String(), "best-effort") {
		t.Fatalf("expected a best-effort skip warning on stderr, got %q", se.String())
	}
}

// TestInternalMaterializeSkillsBestEffortExitsZeroOnResolveCityError guards
// the pre_start robustness fix: when the city itself can't be resolved (e.g.
// GC_CITY unset, cwd outside any city, city directory missing), --best-effort
// must warn and exit 0 so session start is not fatal.
func TestInternalMaterializeSkillsBestEffortExitsZeroOnResolveCityError(t *testing.T) {
	clearGCEnv(t)
	// Change cwd to an isolated tempdir so all city-discovery steps fail:
	// no GC_CITY, no GC_DIR, no registered cities (GC_HOME is also a fresh
	// tempdir set by clearGCEnv), and the walk-up from cwd finds nothing.
	t.Chdir(t.TempDir())

	workdir := t.TempDir()
	const agent = "some-agent"

	// Direct call (no --best-effort): unresolvable city is fatal.
	var so, se bytes.Buffer
	if code := run([]string{"internal", "materialize-skills", "--agent", agent, "--workdir", workdir}, &so, &se); code == 0 {
		t.Fatalf("city not found without --best-effort: got exit 0, want non-zero; stderr=%q", se.String())
	}

	// --best-effort: warn and exit 0 so pre_start is not fatal.
	so.Reset()
	se.Reset()
	if code := run([]string{"internal", "materialize-skills", "--best-effort", "--agent", agent, "--workdir", workdir}, &so, &se); code != 0 {
		t.Fatalf("city not found with --best-effort: exit %d, want 0; stderr=%q", code, se.String())
	}
	if !strings.Contains(se.String(), "best-effort") {
		t.Fatalf("expected a best-effort skip warning on stderr, got %q", se.String())
	}
}

// TestInternalMaterializeSkillsBestEffortExitsZeroOnLoadCityConfigError guards
// the case where the city directory exists (has .gc/) but city.toml is absent
// or unreadable. Under --best-effort this must warn and exit 0.
func TestInternalMaterializeSkillsBestEffortExitsZeroOnLoadCityConfigError(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	// .gc/ present → passes validateCityPath; no city.toml → loadCityConfig fails.
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	workdir := t.TempDir()
	var so, se bytes.Buffer
	if code := run([]string{"internal", "materialize-skills", "--best-effort", "--agent", "some-agent", "--workdir", workdir}, &so, &se); code != 0 {
		t.Fatalf("missing city.toml with --best-effort: exit %d, want 0; stderr=%q", code, se.String())
	}
	if !strings.Contains(se.String(), "best-effort") {
		t.Fatalf("expected a best-effort skip warning on stderr, got %q", se.String())
	}
}

// TestInternalMaterializeSkillsNonBestEffortFailsOnLoadCityConfigError guards
// that the hard-fail path (no --best-effort) still returns non-zero when
// city.toml is missing, so direct invocations get a clear error.
func TestInternalMaterializeSkillsNonBestEffortFailsOnLoadCityConfigError(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	t.Setenv("GC_CITY", cityDir)

	var so, se bytes.Buffer
	if code := run([]string{"internal", "materialize-skills", "--agent", "some-agent", "--workdir", t.TempDir()}, &so, &se); code == 0 {
		t.Fatalf("missing city.toml without --best-effort: got exit 0, want non-zero; stderr=%q", se.String())
	}
}

// TestSharedCatalogSnapshotFrontmatterNameCompatibility pins that the
// shared-catalog snapshot carries FrontmatterName and that a snapshot
// written before the field existed still decodes to the same skill
// identity, with an empty FrontmatterName.
func TestSharedCatalogSnapshotFrontmatterNameCompatibility(t *testing.T) {
	t.Parallel()
	current := materialize.CityCatalog{
		Entries: []materialize.SkillEntry{{
			Name:            "core.gc-work",
			Source:          "/packs/core/skills/gc-work",
			Origin:          "core",
			Description:     "Working with beads",
			FrontmatterName: "gc-work",
		}},
		OwnedRoots: []string{"/packs/core/skills"},
	}
	encoded, err := encodeSharedCatalogSnapshot(current)
	if err != nil {
		t.Fatalf("encodeSharedCatalogSnapshot: %v", err)
	}
	roundTrip, err := decodeSharedCatalogSnapshot(encoded)
	if err != nil {
		t.Fatalf("decodeSharedCatalogSnapshot: %v", err)
	}
	if !reflect.DeepEqual(roundTrip, current) {
		t.Fatalf("round trip = %+v, want %+v", roundTrip, current)
	}

	old := `{"Entries":[{"Name":"core.gc-work","Source":"/packs/core/skills/gc-work","Origin":"core","Description":"Working with beads"}],"OwnedRoots":["/packs/core/skills"],"Shadowed":null}`
	decoded, err := decodeSharedCatalogSnapshot(base64.StdEncoding.EncodeToString([]byte(old)))
	if err != nil {
		t.Fatalf("decoding pre-FrontmatterName snapshot: %v", err)
	}
	want := current
	want.Entries = []materialize.SkillEntry{current.Entries[0]}
	want.Entries[0].FrontmatterName = ""
	if !reflect.DeepEqual(decoded, want) {
		t.Fatalf("pre-FrontmatterName snapshot decoded = %+v, want %+v", decoded, want)
	}
}

// sinkLinkTargets maps each symlink in sink to its os.Readlink target.
func sinkLinkTargets(t *testing.T, sink string) map[string]string {
	t.Helper()
	entries, err := os.ReadDir(sink)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", sink, err)
	}
	out := make(map[string]string)
	for _, e := range entries {
		if e.Type()&os.ModeSymlink == 0 {
			continue
		}
		target, err := os.Readlink(filepath.Join(sink, e.Name()))
		if err != nil {
			t.Fatalf("Readlink(%s): %v", e.Name(), err)
		}
		out[e.Name()] = target
	}
	return out
}

func requireLinkTarget(t *testing.T, link, want string) {
	t.Helper()
	got, err := os.Readlink(link)
	if err != nil {
		t.Errorf("Readlink(%s): %v", link, err)
		return
	}
	if got != want {
		t.Errorf("%s -> %s, want %s", link, got, want)
	}
}

// stage2Result runs agent name's per-session pass into workdir and
// returns what the pass did to the sink.
func (c workDirWriterCity) stage2Result(t *testing.T, name, workdir string) (materialize.Result, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	res, err := materializeSkillSinkForSession(c.cfg, c.agent(t, name), c.cityPath, workdir, nil, &stdout, &stderr)
	if err != nil {
		t.Fatalf("stage 2 for %s into %s: %v; stderr=%q", name, workdir, err, stderr.String())
	}
	return res, stderr.String()
}

func (c workDirWriterCity) stage1(t *testing.T) {
	t.Helper()
	var stderr bytes.Buffer
	if err := runStage1SkillMaterialization(c.cityPath, c.cfg, &stderr); err != nil {
		t.Fatalf("stage 1: %v; stderr=%q", err, stderr.String())
	}
}

// TestMaterializeSkillsIntoWorkdirKeepsScopeRootAgentsSkills: r's session
// writes the city root's sink, which stage 1 already reconciled for m and
// r. r's stage-2 pass folds every writer of that sink, so m's agent-local
// notes stays linked to m's source.
func TestMaterializeSkillsIntoWorkdirKeepsScopeRootAgentsSkills(t *testing.T) {
	c := newWorkDirWriterCity(t)
	c.stage1(t)
	requireSinkLinks(t, c.citySink(), "after stage 1", []string{"notes", "rig-notes", "rp.rig-plan", "plan"}, nil)

	c.stage2(t, "r", c.cityPath)
	requireSinkLinks(t, c.citySink(), "after r's stage 2", []string{"notes", "rig-notes", "rp.rig-plan", "plan"}, nil)
	requireLinkTarget(t, filepath.Join(c.citySink(), "notes"), c.mNotes)
	requireLinkTarget(t, filepath.Join(c.citySink(), "rig-notes"), c.rNotes)
}

// TestMaterializeSkillsIntoWorkdirSharedWorkDirKeepsCoWriterSkills: agents
// a and b both run sessions in {{.CityRoot}}/shared, a sink no stage-1
// pass owns. Whichever session starts first, each pass keeps the other
// agent's agent-local link.
func TestMaterializeSkillsIntoWorkdirSharedWorkDirKeepsCoWriterSkills(t *testing.T) {
	for _, order := range [][]string{{"a", "b"}, {"b", "a"}} {
		t.Run(strings.Join(order, "-then-"), func(t *testing.T) {
			clearGCEnv(t)
			cityPath := t.TempDir()
			t.Setenv("GC_HOME", t.TempDir())
			shared := filepath.Join(cityPath, "shared")
			if err := os.MkdirAll(shared, 0o755); err != nil {
				t.Fatal(err)
			}
			workDir := "{{.CityRoot}}/shared"
			c := workDirWriterCity{cityPath: cityPath, cfg: &config.City{
				Workspace: config.Workspace{Name: "test-city"},
				Session:   config.SessionConfig{Provider: "tmux"},
			}}
			sources := map[string]string{}
			for _, name := range []string{"a", "b"} {
				skills := filepath.Join(cityPath, "agents", name, "skills")
				sources[name+"-notes"] = filepath.Join(skills, name+"-notes")
				writeSkillSource(t, sources[name+"-notes"])
				c.cfg.Agents = append(c.cfg.Agents, config.Agent{Name: name, Scope: "city", Provider: "claude", SkillsDir: skills, WorkDir: workDir})
			}
			sink := filepath.Join(shared, ".claude", "skills")
			for _, name := range order {
				c.stage2(t, name, shared)
				requireSinkLinks(t, sink, "after "+name+"'s stage 2", []string{"a-notes", "b-notes"}, nil)
				for skill, src := range sources {
					requireLinkTarget(t, filepath.Join(sink, skill), src)
				}
			}
		})
	}
}

// clashingWorkDirWriterCity is newWorkDirWriterCity converged by stage 1,
// after which r's skills dir also provides an agent-local notes, so m and
// r want notes from different sources in the city root's sink.
func clashingWorkDirWriterCity(t *testing.T) (workDirWriterCity, string, map[string]string) {
	t.Helper()
	c := newWorkDirWriterCity(t)
	c.stage1(t)
	before := sinkLinkTargets(t, c.citySink())
	rClash := filepath.Join(filepath.Dir(c.rNotes), "notes")
	writeSkillSource(t, rClash)
	return c, rClash, before
}

func requireClashReported(t *testing.T, stderr, rClash string, c workDirWriterCity) {
	t.Helper()
	for _, want := range []string{`"notes"`, `"m"`, `"fe/r"`, c.mNotes, rClash} {
		if !strings.Contains(stderr, want) {
			t.Errorf("clash report does not name %s: %q", want, stderr)
		}
	}
}

// TestStage1SharedSinkClashHoldsSink: m and r both write the city root's
// sink and both provide agent-local notes from different sources. The
// stage-1 pass reports the clash and holds the sink unchanged.
func TestStage1SharedSinkClashHoldsSink(t *testing.T) {
	c, rClash, before := clashingWorkDirWriterCity(t)
	var stderr bytes.Buffer
	err := runStage1SkillMaterialization(c.cityPath, c.cfg, &stderr)
	if err == nil {
		t.Errorf("stage 1 reconciled a sink with a clash; stderr=%q", stderr.String())
	}
	requireClashReported(t, stderr.String(), rClash, c)
	if after := sinkLinkTargets(t, c.citySink()); !reflect.DeepEqual(after, before) {
		t.Errorf("held sink changed:\nbefore %v\nafter  %v", before, after)
	}
}

// TestMaterializeSkillsSharedSinkClashExitsZero: r's stage 2 into the
// city root meets the same clash; it reports it, holds the sink, and the
// command exits 0 so the session still starts.
func TestMaterializeSkillsSharedSinkClashExitsZero(t *testing.T) {
	c, rClash, before := clashingWorkDirWriterCity(t)
	res, stderr := c.stage2Result(t, "r", c.cityPath)
	requireClashReported(t, stderr, rClash, c)
	if len(res.Created)+len(res.Removed)+len(res.Relinked) != 0 {
		t.Errorf("held sink pass changed links: %+v", res)
	}
	if after := sinkLinkTargets(t, c.citySink()); !reflect.DeepEqual(after, before) {
		t.Errorf("held sink changed:\nbefore %v\nafter  %v", before, after)
	}
}

// TestSharedSinkConvergesAcrossStages: the city root's sink has three
// writers (m through stage 1, rig agents r and s through stage 2). Once
// stage 1 has converged it, every order of the three passes leaves the
// sink's links and targets unchanged and no pass removes or relinks.
func TestSharedSinkConvergesAcrossStages(t *testing.T) {
	ops := []string{"stage1", "r", "s"}
	perms := [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}}
	for _, perm := range perms {
		names := make([]string, len(perm))
		for i, p := range perm {
			names[i] = ops[p]
		}
		t.Run(strings.Join(names, "-"), func(t *testing.T) {
			c := newWorkDirWriterCity(t)
			sSkills := filepath.Join(c.cityPath, "agents", "s", "skills")
			writeSkillSource(t, filepath.Join(sSkills, "s-notes"))
			c.cfg.Agents = append(c.cfg.Agents, config.Agent{Name: "s", Scope: "rig", Dir: "fe", Provider: "claude", SkillsDir: sSkills, WorkDir: "{{.CityRoot}}"})
			c.stage1(t)
			want := sinkLinkTargets(t, c.citySink())
			for _, name := range []string{"notes", "rig-notes", "s-notes", "rp.rig-plan", "plan"} {
				if _, ok := want[name]; !ok {
					t.Fatalf("stage 1 did not converge %s into the sink: %v", name, want)
				}
			}
			for _, name := range names {
				if name == "stage1" {
					c.stage1(t)
				} else {
					res, _ := c.stage2Result(t, name, c.cityPath)
					if len(res.Removed) != 0 || len(res.Relinked) != 0 {
						t.Errorf("%s's stage 2 removed %v, relinked %v", name, res.Removed, res.Relinked)
					}
				}
				if got := sinkLinkTargets(t, c.citySink()); !reflect.DeepEqual(got, want) {
					t.Errorf("after %s:\ngot  %v\nwant %v", name, got, want)
				}
			}
		})
	}
}
