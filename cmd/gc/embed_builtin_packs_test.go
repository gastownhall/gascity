package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

func TestMaterializeBuiltinPacks(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	// Verify bd pack.toml exists.
	bdToml := filepath.Join(dir, citylayout.SystemPacksRoot, "bd", "pack.toml")
	if _, err := os.Stat(bdToml); err != nil {
		t.Errorf("bd pack.toml missing: %v", err)
	}

	// Verify dolt pack.toml exists.
	doltToml := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "pack.toml")
	if _, err := os.Stat(doltToml); err != nil {
		t.Errorf("dolt pack.toml missing: %v", err)
	}

	// Verify doctor scripts are executable.
	for _, script := range []string{
		filepath.Join(dir, citylayout.SystemPacksRoot, "bd", "doctor", "check-bd", "run.sh"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "doctor", "check-dolt", "run.sh"),
	} {
		info, err := os.Stat(script)
		if err != nil {
			t.Errorf("script missing: %v", err)
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("script %s not executable: mode %v", filepath.Base(script), info.Mode())
		}
	}

	// Verify dolt commands have executable run.sh entrypoints.
	cmds := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "commands")
	entries, err := os.ReadDir(cmds)
	if err != nil {
		t.Fatalf("reading dolt commands dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("dolt commands dir is empty")
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		run := filepath.Join(cmds, e.Name(), "run.sh")
		info, err := os.Stat(run)
		if err != nil {
			t.Errorf("dolt command %s/run.sh missing: %v", e.Name(), err)
			continue
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("dolt command %s/run.sh not executable: mode %v", e.Name(), info.Mode())
		}
	}

	// Verify dolt assets/scripts/runtime.sh exists and is executable.
	runtimeSh := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "assets", "scripts", "runtime.sh")
	if info, err := os.Stat(runtimeSh); err != nil {
		t.Errorf("dolt assets/scripts/runtime.sh missing: %v", err)
	} else if info.Mode()&0o111 == 0 {
		t.Errorf("dolt assets/scripts/runtime.sh not executable: mode %v", info.Mode())
	}

	// Verify formulas exist.
	formulasDir := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "formulas")
	if _, err := os.Stat(formulasDir); err != nil {
		t.Errorf("dolt formulas dir missing: %v", err)
	}

	// Verify embedded order files are materialized alongside formulas.
	for _, order := range []string{
		filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "orders", "gate-sweep.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "orders", "mol-dog-jsonl.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "orders", "mol-dog-reaper.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "orders", "dolt-health.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "gastown", "orders", "digest-generate.toml"),
	} {
		if _, err := os.Stat(order); err != nil {
			t.Errorf("embedded order missing: %v", err)
		}
	}

	// Verify TOML files are not executable.
	info, err := os.Stat(bdToml)
	if err == nil && info.Mode()&0o111 != 0 {
		t.Errorf("pack.toml should not be executable: mode %v", info.Mode())
	}
}

func TestBuiltinDatabaseEnumeratorsSkipManagedProbeDatabase(t *testing.T) {
	dir := t.TempDir()
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	doltSystemNeedle := "information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe"
	maintenanceSystemNeedle := "^information_schema$\\|^mysql$\\|^dolt_cluster$\\|^performance_schema$\\|^sys$\\|^__gc_probe$"
	for _, tt := range []struct {
		pack     string
		rel      string
		needle   string
		minCount int
	}{
		{"maintenance", filepath.Join("assets", "scripts", "jsonl-export.sh"), maintenanceSystemNeedle, 1},
		{"maintenance", filepath.Join("assets", "scripts", "reaper.sh"), maintenanceSystemNeedle, 1},
		{"dolt", filepath.Join("commands", "list", "run.sh"), doltSystemNeedle, 1},
		{"dolt", filepath.Join("commands", "cleanup", "run.sh"), doltSystemNeedle, 1},
		{"dolt", filepath.Join("commands", "health", "run.sh"), doltSystemNeedle, 2},
		{"dolt", filepath.Join("commands", "sync", "run.sh"), doltSystemNeedle, 2},
		{"dolt", filepath.Join("formulas", "mol-dog-stale-db.toml"), "__gc_probe", 1},
		{"dolt", filepath.Join("formulas", "mol-dog-doctor.toml"), "__gc_probe", 1},
	} {
		path := filepath.Join(dir, citylayout.SystemPacksRoot, tt.pack, tt.rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%s/%s): %v", tt.pack, tt.rel, err)
		}
		if got := strings.Count(string(data), tt.needle); got < tt.minCount {
			t.Fatalf("%s/%s database enumeration must contain %q at least %d time(s), got %d", tt.pack, tt.rel, tt.needle, tt.minCount, got)
		}
	}
}

func TestDoltSyncRejectsManagedProbeDatabaseFilter(t *testing.T) {
	for _, dbName := range []string{
		managedDoltProbeDatabase,
		strings.ToUpper(managedDoltProbeDatabase),
		" " + managedDoltProbeDatabase + " ",
		"information_schema",
		"mysql",
		"dolt_cluster",
		"performance_schema",
		"sys",
	} {
		t.Run(dbName, func(t *testing.T) {
			dir := t.TempDir()
			if err := MaterializeBuiltinPacks(dir); err != nil {
				t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
			}
			packDir := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt")
			script := filepath.Join(packDir, "commands", "sync", "run.sh")
			cmd := exec.Command(script, "--db", dbName)
			cmd.Env = sanitizedBaseEnv("GC_CITY_PATH="+dir, "GC_PACK_DIR="+packDir)
			out, err := cmd.CombinedOutput()
			if err == nil {
				t.Fatalf("gc dolt sync unexpectedly accepted %s:\n%s", dbName, out)
			}
			if !strings.Contains(string(out), "reserved Dolt database name: "+strings.TrimSpace(dbName)) {
				t.Fatalf("gc dolt sync output = %s, want reserved database error", out)
			}
		})
	}
}

func TestBuiltinDoltDoctorAllowsOlderVersionWhenProbeSucceeds(t *testing.T) {
	dir := t.TempDir()
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	binDir := t.TempDir()
	for _, tool := range []struct {
		name string
		body string
	}{
		{name: "dolt", body: "#!/bin/sh\nprintf 'dolt version 1.75.2\\n'\n"},
		{name: "flock", body: "#!/bin/sh\nexit 0\n"},
		{name: "lsof", body: "#!/bin/sh\nexit 0\n"},
	} {
		if err := os.WriteFile(filepath.Join(binDir, tool.name), []byte(tool.body), 0o755); err != nil {
			t.Fatalf("WriteFile(%s): %v", tool.name, err)
		}
	}

	script := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "doctor", "check-dolt", "run.sh")
	cmd := exec.Command(script)
	cmd.Env = append(sanitizedBaseEnv(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check-dolt unexpectedly rejected old Dolt probe: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "dolt available (dolt version 1.75.2)") {
		t.Fatalf("check-dolt output = %s, want successful version probe", out)
	}
}

func TestBuiltinDoltDoctorBoundsVersionProbe(t *testing.T) {
	dir := t.TempDir()
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	binDir := t.TempDir()
	capturePath := filepath.Join(t.TempDir(), "timeout-argv")
	for _, tool := range []struct {
		name string
		body string
	}{
		{
			name: "timeout",
			body: "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$TIMEOUT_CAPTURE\"\nif [ \"$1\" = \"--kill-after=2\" ]; then\n  shift\nfi\nshift\nexec \"$@\"\n",
		},
		{name: "dolt", body: "#!/bin/sh\nprintf 'dolt version 1.86.1\\n'\n"},
		{name: "flock", body: "#!/bin/sh\nexit 0\n"},
		{name: "lsof", body: "#!/bin/sh\nexit 0\n"},
	} {
		if err := os.WriteFile(filepath.Join(binDir, tool.name), []byte(tool.body), 0o755); err != nil {
			t.Fatalf("WriteFile(%s): %v", tool.name, err)
		}
	}

	script := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "doctor", "check-dolt", "run.sh")
	cmd := exec.Command(script)
	cmd.Env = append(
		sanitizedBaseEnv(),
		"PATH="+binDir+":"+os.Getenv("PATH"),
		"TIMEOUT_CAPTURE="+capturePath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check-dolt with fake timeout failed: %v\n%s", err, out)
	}

	capture, err := os.ReadFile(capturePath)
	if err != nil {
		t.Fatalf("ReadFile(timeout capture): %v", err)
	}
	if !strings.Contains(string(capture), "--kill-after=2 10 dolt version") {
		t.Fatalf("timeout argv = %q, want bounded dolt version probe", capture)
	}
}

func TestBuiltinDoltDoctorReportsTimedOutVersionProbe(t *testing.T) {
	dir := t.TempDir()
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	binDir := t.TempDir()
	for _, tool := range []struct {
		name string
		body string
	}{
		{name: "timeout", body: "#!/bin/sh\nexit 124\n"},
		{name: "dolt", body: "#!/bin/sh\nprintf 'dolt version 1.86.1\\n'\n"},
		{name: "flock", body: "#!/bin/sh\nexit 0\n"},
		{name: "lsof", body: "#!/bin/sh\nexit 0\n"},
	} {
		if err := os.WriteFile(filepath.Join(binDir, tool.name), []byte(tool.body), 0o755); err != nil {
			t.Fatalf("WriteFile(%s): %v", tool.name, err)
		}
	}

	script := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "doctor", "check-dolt", "run.sh")
	cmd := exec.Command(script)
	cmd.Env = append(sanitizedBaseEnv(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("check-dolt unexpectedly accepted timed out version probe:\n%s", out)
	}
	if !strings.Contains(string(out), "dolt version timed out after 10s") {
		t.Fatalf("check-dolt output = %s, want timeout warning", out)
	}
}

func TestBuiltinDoltDoctorFailsClosedWithoutBoundedRunner(t *testing.T) {
	dir := t.TempDir()
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	binDir := t.TempDir()
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		t.Fatalf("LookPath(bash): %v", err)
	}
	if err := os.Symlink(bashPath, filepath.Join(binDir, "bash")); err != nil {
		t.Fatalf("symlink bash: %v", err)
	}
	for _, tool := range []struct {
		name string
		body string
	}{
		{name: "dolt", body: "#!/bin/sh\nprintf 'dolt version 1.86.1\\n'\n"},
		{name: "flock", body: "#!/bin/sh\nexit 0\n"},
		{name: "lsof", body: "#!/bin/sh\nexit 0\n"},
	} {
		if err := os.WriteFile(filepath.Join(binDir, tool.name), []byte(tool.body), 0o755); err != nil {
			t.Fatalf("WriteFile(%s): %v", tool.name, err)
		}
	}

	script := filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "doctor", "check-dolt", "run.sh")
	cmd := exec.Command(script)
	cmd.Env = append(sanitizedBaseEnv(), "PATH="+binDir)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("check-dolt unexpectedly succeeded without bounded runner:\n%s", out)
	}
	if !strings.Contains(string(out), "dolt version timed out after 10s") {
		t.Fatalf("check-dolt output = %s, want timeout warning", out)
	}
}

func TestMaterializeBuiltinPacks_Idempotent(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}
	// Second call should succeed without error.
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("second call failed: %v", err)
	}

	// Files should still exist.
	if _, err := os.Stat(filepath.Join(dir, citylayout.SystemPacksRoot, "bd", "pack.toml")); err != nil {
		t.Error("bd pack.toml missing after second call")
	}
}

func TestMaterializeBuiltinPacksPiHookUsesCurrentExtensionAPI(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	data := readMaterializedPiHook(t, dir)
	for _, want := range []string{
		"module.exports = function gascityPiExtension(pi)",
		`pi.on("session_start"`,
		`pi.on("session_compact"`,
		`pi.on("before_agent_start"`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("materialized Pi hook missing current extension API marker %q:\n%s", want, data)
		}
	}
	if strings.Contains(data, "gc hook --inject") {
		t.Errorf("materialized Pi hook should not install no-op gc hook --inject:\n%s", data)
	}
	for _, legacy := range []string{
		"module.exports = {",
		`"session.created"`,
		`"session.compacted"`,
		`"session.deleted"`,
		`"experimental.chat.system.transform"`,
	} {
		if strings.Contains(data, legacy) {
			t.Errorf("materialized Pi hook still contains legacy API marker %q:\n%s", legacy, data)
		}
	}
}

func TestMaterializeBuiltinPacksReplacesStaleMaterializedPiHook(t *testing.T) {
	dir := t.TempDir()
	hookPath := materializedPiHookPath(dir)
	if err := os.MkdirAll(filepath.Dir(hookPath), 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", filepath.Dir(hookPath), err)
	}
	stale := []byte(`// Gas City hooks for Pi Coding Agent.
module.exports = {
  name: "gascity",
  events: { "session.created": () => "" },
  hooks: { "experimental.chat.system.transform": (system) => system },
};
`)
	if err := os.WriteFile(hookPath, stale, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", hookPath, err)
	}

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	data := readMaterializedPiHook(t, dir)
	if data == string(stale) {
		t.Fatal("stale materialized Pi hook was preserved; expected core pack materialization to repair it")
	}
	if !strings.Contains(data, `pi.on("session_start"`) {
		t.Fatalf("repaired materialized Pi hook does not use current extension API:\n%s", data)
	}
}

func materializedPiHookPath(dir string) string {
	return filepath.Join(dir, citylayout.SystemPacksRoot, "core", "overlay", "per-provider", "pi", ".pi", "extensions", "gc-hooks.js")
}

func readMaterializedPiHook(t *testing.T, dir string) string {
	t.Helper()
	path := materializedPiHookPath(dir)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	return string(data)
}

func TestMaterializeBuiltinPacks_DoesNotRewriteUnchangedFiles(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	path := filepath.Join(dir, citylayout.SystemPacksRoot, "core", "skills", "gc-dashboard", "SKILL.md")
	past := time.Unix(123456789, 0)
	if err := os.Chtimes(path, past, past); err != nil {
		t.Fatalf("Chtimes(%s): %v", path, err)
	}

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() second call error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if !info.ModTime().Equal(past) {
		t.Fatalf("unchanged file was rewritten: modtime = %s, want %s", info.ModTime(), past)
	}
}

func TestMaterializeBuiltinPacks_RestoresModeWhenContentUnchanged(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	path := filepath.Join(dir, citylayout.SystemPacksRoot, "bd", "doctor", "check-bd", "run.sh")
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatalf("Chmod(%s): %v", path, err)
	}

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() second call error: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%s): %v", path, err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("script mode was not restored: %v", info.Mode().Perm())
	}
}

func TestMaterializeBuiltinPacks_ReplacesMatchingSymlink(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	path := filepath.Join(dir, citylayout.SystemPacksRoot, "core", "skills", "gc-dashboard", "SKILL.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s): %v", path, err)
	}
	target := filepath.Join(dir, "outside-skill.md")
	if err := os.WriteFile(target, data, 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", target, err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("Remove(%s): %v", path, err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skipf("Symlink: %v", err)
	}

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() second call error: %v", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat(%s): %v", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("matching symlink was preserved, want regular file")
	}
}

func TestMaterializedBuiltinPackOrdersScanWithoutWarnings(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{
				filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "formulas"),
				filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "formulas"),
				filepath.Join(dir, citylayout.SystemPacksRoot, "gastown", "formulas"),
			},
		},
	}

	var stderr bytes.Buffer
	aa, err := scanAllOrders(dir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v", err)
	}
	if strings.Contains(stderr.String(), "deprecated order path") {
		t.Fatalf("unexpected deprecation warning while scanning materialized builtin packs:\n%s", stderr.String())
	}

	names := make(map[string]bool, len(aa))
	for _, a := range aa {
		names[a.Name] = true
	}
	for _, want := range []string{"gate-sweep", "dolt-health", "digest-generate"} {
		if !names[want] {
			t.Fatalf("missing bundled order %q; got %v", want, names)
		}
	}
}

func TestMaterializeBuiltinPacks_PrunesLegacyOrderDirs(t *testing.T) {
	dir := t.TempDir()

	legacyPaths := []string{
		filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "formulas", "orders", "gate-sweep", "order.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "formulas", "orders", "dolt-health", "order.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "gastown", "formulas", "orders", "digest-generate", "order.toml"),
	}
	for _, path := range legacyPaths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("mkdir legacy path: %v", err)
		}
		if err := os.WriteFile(path, []byte("legacy"), 0o644); err != nil {
			t.Fatalf("write legacy path: %v", err)
		}
	}

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatalf("MaterializeBuiltinPacks() error: %v", err)
	}

	for _, path := range legacyPaths {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("legacy order path still exists: %s", path)
		}
	}

	for _, path := range []string{
		filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance", "orders", "gate-sweep.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "dolt", "orders", "dolt-health.toml"),
		filepath.Join(dir, citylayout.SystemPacksRoot, "gastown", "orders", "digest-generate.toml"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("flat order missing after materialization: %v", err)
		}
	}
}

func TestBuiltinPackIncludes_DefaultProvider(t *testing.T) {
	dir := t.TempDir()

	// Materialize packs first.
	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// Default provider (empty) → should include core, maintenance, and bd.
	t.Setenv("GC_BEADS", "")
	includes := builtinPackIncludes(dir)

	if len(includes) != 3 {
		t.Fatalf("builtinPackIncludes() = %v, want 3 entries", includes)
	}

	systemRoot := filepath.Join(dir, citylayout.SystemPacksRoot)
	wantCore := filepath.Join(systemRoot, "core")
	wantMaintenance := filepath.Join(systemRoot, "maintenance")
	wantBd := filepath.Join(systemRoot, "bd")

	if includes[0] != wantCore {
		t.Errorf("includes[0] = %q, want %q", includes[0], wantCore)
	}
	if includes[1] != wantMaintenance {
		t.Errorf("includes[1] = %q, want %q", includes[1], wantMaintenance)
	}
	if includes[2] != wantBd {
		t.Errorf("includes[2] = %q, want %q", includes[2], wantBd)
	}
}

func TestBuiltinPackIncludes_ExplicitBd(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// Write a city.toml with provider = "bd".
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "")
	includes := builtinPackIncludes(dir)

	if len(includes) != 3 {
		t.Fatalf("builtinPackIncludes() = %v, want 3 entries (core + maintenance + bd)", includes)
	}

	if got := filepath.Base(includes[0]); got != "core" {
		t.Errorf("includes[0] base = %q, want core", got)
	}
	if got := filepath.Base(includes[1]); got != "maintenance" {
		t.Errorf("includes[1] base = %q, want maintenance", got)
	}
	if got := filepath.Base(includes[2]); got != "bd" {
		t.Errorf("includes[2] base = %q, want bd", got)
	}
}

func TestBuiltinPackIncludes_NonBdProvider(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// Write a city.toml with a non-bd provider.
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "")
	includes := builtinPackIncludes(dir)

	// Core and maintenance are always auto-included; bd/dolt are gated
	// on a bd-compatible provider.
	if len(includes) != 2 {
		t.Fatalf("builtinPackIncludes() = %v, want 2 entries (core + maintenance)", includes)
	}

	if got := filepath.Base(includes[0]); got != "core" {
		t.Errorf("includes[0] base = %q, want core", got)
	}
	if got := filepath.Base(includes[1]); got != "maintenance" {
		t.Errorf("includes[1] base = %q, want maintenance", got)
	}
}

func TestBuiltinPackIncludes_ExecGcBeadsBdOverrideIncludesBdAndDolt(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:/tmp/gc-beads-bd")
	includes := builtinPackIncludes(dir)
	// core + maintenance + bd + dolt = 4 entries. Core and maintenance are
	// always auto-included; bd and dolt arrive via the exec-override path.
	if len(includes) != 4 {
		t.Fatalf("builtinPackIncludes() = %v, want 4 entries when GC_BEADS=exec:gc-beads-bd", includes)
	}
	if got := filepath.Base(includes[0]); got != "core" {
		t.Fatalf("includes[0] base = %q, want core", got)
	}
	if got := filepath.Base(includes[2]); got != "bd" {
		t.Fatalf("includes[2] base = %q, want bd", got)
	}
	if got := filepath.Base(includes[3]); got != "dolt" {
		t.Fatalf("includes[3] base = %q, want dolt", got)
	}
}

func TestBuiltinPackIncludes_EnvOverride(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// GC_BEADS env var overrides city.toml provider.
	t.Setenv("GC_BEADS", "file")
	includes := builtinPackIncludes(dir)

	// Core and maintenance are always auto-included; bd/dolt are gated on
	// a bd-compatible provider.
	if len(includes) != 2 {
		t.Fatalf("builtinPackIncludes() = %v, want 2 entries when GC_BEADS=file", includes)
	}

	if got := filepath.Base(includes[0]); got != "core" {
		t.Errorf("includes[0] base = %q, want core", got)
	}
	if got := filepath.Base(includes[1]); got != "maintenance" {
		t.Errorf("includes[1] base = %q, want maintenance", got)
	}
}

func TestBuiltinPackIncludes_ManagedExecEnvStillIncludesBd(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(dir))
	includes := builtinPackIncludes(dir)

	if len(includes) != 3 {
		t.Fatalf("builtinPackIncludes() = %v, want core + maintenance + bd", includes)
	}
	if got := filepath.Base(includes[0]); got != "core" {
		t.Errorf("includes[0] base = %q, want core", got)
	}
	if got := filepath.Base(includes[1]); got != "maintenance" {
		t.Errorf("includes[1] base = %q, want maintenance", got)
	}
	if got := filepath.Base(includes[2]); got != "bd" {
		t.Errorf("includes[2] base = %q, want bd", got)
	}
}

func TestBuiltinPackIncludes_NotMaterialized(t *testing.T) {
	dir := t.TempDir()

	// Don't materialize — should return empty.
	t.Setenv("GC_BEADS", "")
	includes := builtinPackIncludes(dir)

	if len(includes) != 0 {
		t.Errorf("builtinPackIncludes() = %v, want empty when packs not materialized", includes)
	}
}

func TestBuiltinPackIncludes_PathsPointToSystemPacks(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_BEADS", "")
	includes := builtinPackIncludes(dir)

	systemRoot := filepath.Join(dir, citylayout.SystemPacksRoot)
	for _, inc := range includes {
		// Every include path must be under .gc/system/packs/.
		rel, err := filepath.Rel(systemRoot, inc)
		if err != nil {
			t.Errorf("path %q not relative to system root: %v", inc, err)
			continue
		}
		if rel == ".." || len(rel) > 0 && rel[0] == '.' {
			t.Errorf("path %q escapes system packs root (rel=%q)", inc, rel)
		}
		// Each include path should be a directory with pack.toml inside.
		if _, err := os.Stat(filepath.Join(inc, "pack.toml")); err != nil {
			t.Errorf("pack.toml missing in %q: %v", inc, err)
		}
	}
}

func TestBuiltinPackIncludes_AlwaysIncludesMaintenance(t *testing.T) {
	dir := t.TempDir()

	if err := MaterializeBuiltinPacks(dir); err != nil {
		t.Fatal(err)
	}

	// Even with non-bd provider, maintenance must be present.
	t.Setenv("GC_BEADS", "file")
	includes := builtinPackIncludes(dir)

	found := false
	for _, inc := range includes {
		if filepath.Base(inc) == "maintenance" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("maintenance pack not found in includes: %v", includes)
	}

	// Also with bd provider.
	t.Setenv("GC_BEADS", "bd")
	includes = builtinPackIncludes(dir)

	found = false
	for _, inc := range includes {
		if filepath.Base(inc) == "maintenance" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("maintenance pack not found in bd includes: %v", includes)
	}
}
