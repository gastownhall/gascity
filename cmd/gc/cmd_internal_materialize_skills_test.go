package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	// Pack.toml enables PackSkillsDir discovery. Without it, the
	// materializer sees no shared city catalog and the sink stays empty.
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"city\"\nversion = \"0.1.0\"\nschema = 2\n\n[imports.helper]\nsource = \"../helper\"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
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
	cityToml := fmt.Sprintf(`[workspace]
name = "test-city"

[beads]
provider = "file"

[[rigs]]
name = "fe"
path = %q

[rigs.imports.helper]
source = "./assets/helper"

[[agent]]
name = "mayor"
scope = "city"
dir = "fe"
provider = "claude"
start_command = "echo"
`, rigDir)
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(helperDir, "pack.toml"), []byte("[pack]\nname = \"helper\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(helper/pack.toml): %v", err)
	}
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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "copilot"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	// Pack.toml enables PackSkillsDir discovery. Without it, the
	// materializer sees no shared city catalog and the sink stays empty.
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	// Pack.toml enables PackSkillsDir discovery. Without it, the
	// materializer sees no shared city catalog and the sink stays empty.
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}

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
	toml := `[workspace]
name = "test-city"

[beads]
provider = "file"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"

[[named_session]]
template = "mayor"
`
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
	// Pack.toml enables PackSkillsDir discovery. Without it, the
	// materializer sees no shared city catalog and the sink stays empty.
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte("[pack]\nname = \"test\"\nversion = \"0.1.0\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pack.toml): %v", err)
	}
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
