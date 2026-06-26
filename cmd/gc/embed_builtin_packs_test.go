package main

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

// TestPeekEventsProvider asserts the fast city.toml read path used by
// `gc event emit` (gastownhall/gascity#2099) — it must return the
// configured provider without doing any pack-include resolution.
func TestPeekEventsProvider(t *testing.T) {
	t.Run("set_in_city_toml", func(t *testing.T) {
		dir := t.TempDir()
		tomlPath := filepath.Join(dir, "city.toml")
		if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"city\"\n\n[events]\nprovider = \"exec:/usr/local/bin/my-handler\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := peekEventsProvider(tomlPath); got != "exec:/usr/local/bin/my-handler" {
			t.Fatalf("peekEventsProvider = %q, want %q", got, "exec:/usr/local/bin/my-handler")
		}
	})

	t.Run("section_absent", func(t *testing.T) {
		dir := t.TempDir()
		tomlPath := filepath.Join(dir, "city.toml")
		if err := os.WriteFile(tomlPath, []byte("[workspace]\nname = \"city\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := peekEventsProvider(tomlPath); got != "" {
			t.Fatalf("peekEventsProvider = %q, want empty", got)
		}
	})

	t.Run("file_missing", func(t *testing.T) {
		if got := peekEventsProvider(filepath.Join(t.TempDir(), "nope.toml")); got != "" {
			t.Fatalf("peekEventsProvider missing-file = %q, want empty", got)
		}
	})

	// The whole point of this helper is to skip pack resolution. A
	// well-formed [imports] block referencing a remote pack with no
	// matching packs.lock entry MUST NOT cause peekEventsProvider to
	// error or shell out — it should still return the [events] value.
	t.Run("ignores_unresolved_imports", func(t *testing.T) {
		dir := t.TempDir()
		tomlPath := filepath.Join(dir, "city.toml")
		body := "[workspace]\nname = \"city\"\nincludes = [\"git://example.invalid/foo//bar\"]\n\n[events]\nprovider = \"exec:./my-handler\"\n"
		if err := os.WriteFile(tomlPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if got := peekEventsProvider(tomlPath); got != "exec:./my-handler" {
			t.Fatalf("peekEventsProvider = %q, want %q (unresolved imports must not block the peek)", got, "exec:./my-handler")
		}
	})
}

// TestBuiltinPacksUseCanonicalRegistry pins the canonical registry surface
// cmd/gc depends on: the bundled pack set, and a registered source plus
// embedded FS for every name requiredBuiltinPackNames can return.
func TestBuiltinPacksUseCanonicalRegistry(t *testing.T) {
	want := []string{"core", "bd", "dolt", "gastown", "gascity"}
	registry := builtinpacks.All()
	got := make([]string, 0, len(registry))
	for _, pack := range registry {
		got = append(got, pack.Name)
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("builtinpacks.All names = %v, want %v", got, want)
	}
	for _, name := range want {
		if _, ok := builtinpacks.Source(name); !ok {
			t.Errorf("builtinpacks.Source(%q) not registered", name)
		}
		pack, ok := builtinpacks.ByName(name)
		if !ok {
			t.Errorf("builtinpacks.ByName(%q) not found", name)
			continue
		}
		if pack.FS == nil {
			t.Errorf("builtinpacks.ByName(%q).FS is nil", name)
		}
	}
}

// readBundledPackFileForTest reads a file from a bundled pack's embedded FS.
// rel must use forward slashes.
func readBundledPackFileForTest(t *testing.T, packName, rel string) string {
	t.Helper()
	pack, ok := builtinpacks.ByName(packName)
	if !ok {
		t.Fatalf("bundled %s pack is not registered", packName)
	}
	data, err := fs.ReadFile(pack.FS, rel)
	if err != nil {
		t.Fatalf("reading bundled %s pack file %s: %v", packName, rel, err)
	}
	return string(data)
}

// bundledPackDirForTest hydrates the user-global cache for a bundled pack's
// source (the same packman path EnsureBuiltinRuntimeAssets takes) and
// returns the on-disk pack directory inside the cache. Use it for tests
// that exec bundled scripts; content-only assertions should read the
// embedded FS via readBundledPackFileForTest instead.
func bundledPackDirForTest(t testing.TB, packName string) string {
	t.Helper()
	source, ok := builtinpacks.Source(packName)
	if !ok {
		t.Fatalf("bundled %s pack is not registered", packName)
	}
	cachePath, err := packman.EnsureRepoInCache(source, bundledPackImportCommit())
	if err != nil {
		t.Fatalf("EnsureRepoInCache(%s): %v", packName, err)
	}
	pack, _ := builtinpacks.ByName(packName)
	return filepath.Join(cachePath, filepath.FromSlash(pack.Subpath))
}

func TestBuiltinDatabaseEnumeratorsSkipManagedProbeDatabase(t *testing.T) {
	doltSystemNeedle := "information_schema|mysql|dolt_cluster|performance_schema|sys|__gc_probe"
	maintenanceScratchNeedle := "benchdb|testdb_*|beads_pt*|beads_vr*|beads_test_bench_*|doctest_*|doctortest_*"
	maintenanceTempNeedle := "beads_t[0-9a-f]"
	for _, tt := range []struct {
		pack     string
		rel      string
		needle   string
		minCount int
	}{
		{"core", "assets/scripts/jsonl-export.sh", doltSystemNeedle, 1},
		{"core", "assets/scripts/jsonl-export.sh", maintenanceScratchNeedle, 1},
		{"core", "assets/scripts/jsonl-export.sh", maintenanceTempNeedle, 1},
		{"core", "assets/scripts/reaper.sh", doltSystemNeedle, 1},
		{"core", "assets/scripts/reaper.sh", maintenanceScratchNeedle, 1},
		{"core", "assets/scripts/reaper.sh", maintenanceTempNeedle, 1},
		{"core", "assets/scripts/reaper.sh", "expires_at", 1},
		{"dolt", "commands/list/run.sh", doltSystemNeedle, 1},
		{"dolt", "commands/cleanup/run.sh", doltSystemNeedle, 1},
		{"dolt", "commands/health/run.sh", doltSystemNeedle, 2},
		{"dolt", "commands/sync/run.sh", doltSystemNeedle, 2},
		{"dolt", "assets/scripts/mol-dog-doctor.sh", "__gc_probe", 1},
		{"dolt", "formulas/mol-dog-stale-db.toml", "__gc_probe", 1},
	} {
		data := readBundledPackFileForTest(t, tt.pack, tt.rel)
		if got := strings.Count(data, tt.needle); got < tt.minCount {
			t.Fatalf("%s/%s database enumeration must contain %q at least %d time(s), got %d", tt.pack, tt.rel, tt.needle, tt.minCount, got)
		}
	}
}

func TestDoltSyncRejectsManagedProbeDatabaseFilter(t *testing.T) {
	packDir := bundledPackDirForTest(t, "dolt")
	script := filepath.Join(packDir, "commands", "sync", "run.sh")
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

func TestBuiltinDoltDoctorAllowsAtMinimumVersionWhenProbeSucceeds(t *testing.T) {
	binDir := t.TempDir()
	for _, tool := range []struct {
		name string
		body string
	}{
		{name: "dolt", body: "#!/bin/sh\nprintf 'dolt version 2.1.0\\n'\n"},
		{name: "flock", body: "#!/bin/sh\nexit 0\n"},
		{name: "lsof", body: "#!/bin/sh\nexit 0\n"},
	} {
		if err := os.WriteFile(filepath.Join(binDir, tool.name), []byte(tool.body), 0o755); err != nil {
			t.Fatalf("WriteFile(%s): %v", tool.name, err)
		}
	}

	script := filepath.Join(bundledPackDirForTest(t, "dolt"), "doctor", "check-dolt", "run.sh")
	cmd := exec.Command(script)
	cmd.Env = append(sanitizedBaseEnv(), "PATH="+binDir+":"+os.Getenv("PATH"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("check-dolt unexpectedly rejected Dolt probe at minimum: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "dolt available (dolt version 2.1.0)") {
		t.Fatalf("check-dolt output = %s, want successful version probe", out)
	}
}

func TestBuiltinDoltDoctorBoundsVersionProbe(t *testing.T) {
	binDir := t.TempDir()
	capturePath := filepath.Join(t.TempDir(), "timeout-argv")
	for _, tool := range []struct {
		name string
		body string
	}{
		// Named gtimeout so the script's gtimeout-first preference picks
		// up the fake even on macOS dev hosts where Homebrew coreutils
		// exposes a real gtimeout from /opt/homebrew/bin. binDir is
		// prepended to PATH below, so the fake wins.
		{
			name: "gtimeout",
			body: "#!/bin/sh\nprintf '%s\\n' \"$*\" > \"$TIMEOUT_CAPTURE\"\nif [ \"$1\" = \"--kill-after=2\" ]; then\n  shift\nfi\nshift\nexec \"$@\"\n",
		},
		{name: "dolt", body: "#!/bin/sh\nprintf 'dolt version 2.1.10\\n'\n"},
		{name: "flock", body: "#!/bin/sh\nexit 0\n"},
		{name: "lsof", body: "#!/bin/sh\nexit 0\n"},
	} {
		if err := os.WriteFile(filepath.Join(binDir, tool.name), []byte(tool.body), 0o755); err != nil {
			t.Fatalf("WriteFile(%s): %v", tool.name, err)
		}
	}

	script := filepath.Join(bundledPackDirForTest(t, "dolt"), "doctor", "check-dolt", "run.sh")
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
	binDir := t.TempDir()
	for _, tool := range []struct {
		name string
		body string
	}{
		// Named gtimeout for the same reason as
		// TestBuiltinDoltDoctorBoundsVersionProbe.
		{name: "gtimeout", body: "#!/bin/sh\nexit 124\n"},
		{name: "dolt", body: "#!/bin/sh\nprintf 'dolt version 1.86.1\\n'\n"},
		{name: "flock", body: "#!/bin/sh\nexit 0\n"},
		{name: "lsof", body: "#!/bin/sh\nexit 0\n"},
	} {
		if err := os.WriteFile(filepath.Join(binDir, tool.name), []byte(tool.body), 0o755); err != nil {
			t.Fatalf("WriteFile(%s): %v", tool.name, err)
		}
	}

	script := filepath.Join(bundledPackDirForTest(t, "dolt"), "doctor", "check-dolt", "run.sh")
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

	script := filepath.Join(bundledPackDirForTest(t, "dolt"), "doctor", "check-dolt", "run.sh")
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

func TestBundledPiHookUsesCurrentExtensionAPI(t *testing.T) {
	data := readBundledPackFileForTest(t, "core", "overlay/per-provider/pi/.pi/extensions/gc-hooks.js")
	for _, want := range []string{
		"module.exports = function gascityPiExtension(pi)",
		`pi.on("session_start"`,
		`pi.on("session_compact"`,
		`pi.on("before_agent_start"`,
		"GC_PI_HOOK_VERSION",
		"gc hook --inject",
		`run(["prime", "--hook"], ctx.cwd, providerSessionEnv(ctx))`,
		"GC_PROVIDER_SESSION_ID",
		"GC_PROVIDER_SESSION_ID_REQUIRED",
		`stdio: ["ignore", "pipe", "inherit"]`,
		"gc handoff --auto",
		"mirrorTempCounter",
		"fs.rmSync(tmp",
		"gc-hooks run:",
		"gc-hooks mirrorTranscript:",
	} {
		if !strings.Contains(data, want) {
			t.Errorf("bundled Pi hook missing current extension API marker %q:\n%s", want, data)
		}
	}
	for _, legacy := range []string{
		"module.exports = {",
		`"session.created"`,
		`"session.compacted"`,
		`"session.deleted"`,
		`"experimental.chat.system.transform"`,
	} {
		if strings.Contains(data, legacy) {
			t.Errorf("bundled Pi hook still contains legacy API marker %q:\n%s", legacy, data)
		}
	}
}

func TestBundledOmpHookPublishesProviderSessionID(t *testing.T) {
	data := readBundledPackFileForTest(t, "core", "overlay/per-provider/omp/.omp/hooks/gc-hook.ts")
	for _, want := range []string{
		`import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent"`,
		`const GC_OMP_HOOK_VERSION = 2`,
		`export default function gascityOmpExtension(pi: ExtensionAPI)`,
		`pi.on("session_start"`,
		`pi.on("session_compact"`,
		`pi.on("before_agent_start"`,
		`GC_PROVIDER_SESSION_ID`,
		`GC_PROVIDER_SESSION_ID_REQUIRED`,
		`stdio: ["ignore", "pipe", "inherit"]`,
		`getSessionId`,
		`logRunFailure`,
	} {
		if !strings.Contains(data, want) {
			t.Errorf("bundled OMP hook missing provider-session marker %q:\n%s", want, data)
		}
	}
	for _, legacy := range []string{
		"export default {",
		`"session.created"`,
		`"session.compacted"`,
		`"experimental.chat.system.transform"`,
	} {
		if strings.Contains(data, legacy) {
			t.Errorf("bundled OMP hook still contains legacy API marker %q:\n%s", legacy, data)
		}
	}
}

func TestBundledBuiltinPackOrdersScanWithoutWarnings(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.City{
		FormulaLayers: config.FormulaLayers{
			City: []string{
				filepath.Join(bundledPackDirForTest(t, "core"), "formulas"),
				filepath.Join(bundledPackDirForTest(t, "dolt"), "formulas"),
				filepath.Join(bundledPackDirForTest(t, "gastown"), "formulas"),
			},
		},
	}

	var stderr bytes.Buffer
	aa, err := scanAllOrders(dir, cfg, &stderr, "gc order list")
	if err != nil {
		t.Fatalf("scanAllOrders: %v", err)
	}
	if strings.Contains(stderr.String(), "deprecated order path") {
		t.Fatalf("unexpected deprecation warning while scanning bundled builtin packs:\n%s", stderr.String())
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

func TestBundledWorkerPromptsIncludeFilesystemSearchGuidance(t *testing.T) {
	for _, name := range []string{"pool-worker.md", "graph-worker.md"} {
		t.Run(name, func(t *testing.T) {
			data := readBundledPackFileForTest(t, "core", "assets/prompts/"+name)
			if !strings.Contains(data, formulaFilesystemSearchGuidance) {
				t.Fatalf("bundled %s missing filesystem search guidance", name)
			}
		})
	}
}

func writeBuiltinPackLoadTestCity(dir string) error {
	return os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644)
}

func assertPackNamesForTest(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("requiredBuiltinPackNames = %v, want %v", got, want)
	}
}

func TestRequiredBuiltinPackNames(t *testing.T) {
	t.Run("default_provider", func(t *testing.T) {
		clearGCEnv(t)
		dir := t.TempDir()

		// Default provider (no env, no city.toml) → core and bd.
		assertPackNamesForTest(t, requiredBuiltinPackNames(dir), []string{"core", "bd"})

		// The matching [imports.<name>] entries carry the bundled source
		// and the canonical bundled pin.
		imports, ordered := requiredBuiltinImports(dir)
		if strings.Join(ordered, ",") != "core,bd" {
			t.Fatalf("requiredBuiltinImports order = %v, want [core bd]", ordered)
		}
		for _, name := range ordered {
			source, ok := builtinpacks.Source(name)
			if !ok {
				t.Fatalf("builtinpacks.Source(%q) not registered", name)
			}
			imp, ok := imports[name]
			if !ok {
				t.Fatalf("requiredBuiltinImports missing %q: %#v", name, imports)
			}
			if imp.Source != source {
				t.Errorf("imports[%q].Source = %q, want %q", name, imp.Source, source)
			}
			if imp.Version != config.BundledPackImportVersion {
				t.Errorf("imports[%q].Version = %q, want %q", name, imp.Version, config.BundledPackImportVersion)
			}
		}
	})

	t.Run("env_file_provider", func(t *testing.T) {
		clearGCEnv(t)
		dir := t.TempDir()
		t.Setenv("GC_BEADS", "file")
		assertPackNamesForTest(t, requiredBuiltinPackNames(dir), []string{"core"})
	})

	t.Run("city_toml_file_provider", func(t *testing.T) {
		clearGCEnv(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertPackNamesForTest(t, requiredBuiltinPackNames(dir), []string{"core"})
	})

	t.Run("city_toml_bd_provider", func(t *testing.T) {
		clearGCEnv(t)
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		assertPackNamesForTest(t, requiredBuiltinPackNames(dir), []string{"core", "bd"})
	})

	t.Run("exec_gc_beads_bd_override_adds_dolt", func(t *testing.T) {
		clearGCEnv(t)
		dir := t.TempDir()
		// An exec:gc-beads-bd provider that is NOT the city's own shim is a
		// direct exec lifecycle: it satisfies the bd store contract AND needs
		// the dolt pack for lifecycle tooling.
		t.Setenv("GC_BEADS", "exec:/tmp/gc-beads-bd")
		assertPackNamesForTest(t, requiredBuiltinPackNames(dir), []string{"core", "bd", "dolt"})
	})

	t.Run("city_shim_exec_normalizes_to_bd", func(t *testing.T) {
		clearGCEnv(t)
		dir := t.TempDir()
		// The city's own stable shim path normalizes back to the logical
		// "bd" provider, so it must NOT trigger the direct-exec dolt
		// requirement.
		t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(dir))
		assertPackNamesForTest(t, requiredBuiltinPackNames(dir), []string{"core", "bd"})
	})
}

func TestBuiltinImportsForNames(t *testing.T) {
	imports, ordered := builtinImportsForNames([]string{"core", "bd", "dolt", "not-a-pack"})

	// Unknown names are skipped; known names keep their input order.
	if strings.Join(ordered, ",") != "core,bd,dolt" {
		t.Fatalf("builtinImportsForNames order = %v, want [core bd dolt]", ordered)
	}
	if len(imports) != 3 {
		t.Fatalf("builtinImportsForNames imports = %#v, want 3 entries", imports)
	}
	for _, name := range ordered {
		source, ok := builtinpacks.Source(name)
		if !ok {
			t.Fatalf("builtinpacks.Source(%q) not registered", name)
		}
		imp := imports[name]
		if imp.Source != source {
			t.Errorf("imports[%q].Source = %q, want %q", name, imp.Source, source)
		}
		if imp.Version != config.BundledPackImportVersion {
			t.Errorf("imports[%q].Version = %q, want %q", name, imp.Version, config.BundledPackImportVersion)
		}
	}
}

func TestBuiltinImportsForInit(t *testing.T) {
	t.Run("provider_resolution", func(t *testing.T) {
		clearGCEnv(t)
		for _, tt := range []struct {
			provider string
			want     string
		}{
			{provider: "", want: "core,bd"},
			{provider: "bd", want: "core,bd"},
			{provider: "file", want: "core"},
			{provider: "exec:/tmp/custom-store", want: "core"},
			{provider: "exec:/tmp/gc-beads-bd", want: "core,bd"},
		} {
			_, ordered := builtinImportsForInit(tt.provider)
			if got := strings.Join(ordered, ","); got != tt.want {
				t.Errorf("builtinImportsForInit(%q) = %v, want %s", tt.provider, ordered, tt.want)
			}
		}
	})

	t.Run("gc_beads_env_wins_over_city_provider", func(t *testing.T) {
		clearGCEnv(t)
		t.Setenv("GC_BEADS", "file")
		_, ordered := builtinImportsForInit("bd")
		if got := strings.Join(ordered, ","); got != "core" {
			t.Errorf("builtinImportsForInit with GC_BEADS=file = %v, want core only", ordered)
		}
	})
}

func TestNoMaintenanceBuiltinPack(t *testing.T) {
	// The maintenance pack was folded into core: it must not exist in the
	// registry, and its housekeeping orders must ship with core instead.
	if _, ok := builtinpacks.ByName("maintenance"); ok {
		t.Error("builtinpacks.ByName(\"maintenance\") found, want absent")
	}
	for _, pack := range builtinpacks.All() {
		if pack.Name == "maintenance" {
			t.Error("builtinpacks.All contains retired maintenance pack")
		}
	}
	for _, rel := range []string{
		"orders/gate-sweep.toml",
		"orders/orphan-sweep.toml",
		"orders/wisp-compact.toml",
		"assets/scripts/gate-sweep.sh",
		"doctor/check-binaries/run.sh",
	} {
		// readBundledPackFileForTest fatals if the asset is missing.
		if data := readBundledPackFileForTest(t, "core", rel); data == "" {
			t.Errorf("core pack folded maintenance asset %s is empty", rel)
		}
	}
}

func TestEnsureBuiltinRuntimeAssetsHydratesCacheAndShim(t *testing.T) {
	clearGCEnv(t) // fresh GC_HOME → hydration starts from a cold cache
	city := t.TempDir()

	materializeBuiltinPacksForTest(t, city)

	// The default-provider city requires core and bd; both bundled-source
	// caches must validate against the running binary's embedded content.
	commit := bundledPackImportCommit()
	for _, name := range []string{"core", "bd"} {
		source, ok := builtinpacks.Source(name)
		if !ok {
			t.Fatalf("builtinpacks.Source(%q) not registered", name)
		}
		cachePath, err := packman.RepoCachePath(source, commit)
		if err != nil {
			t.Fatalf("RepoCachePath(%s): %v", name, err)
		}
		if err := builtinpacks.ValidateSyntheticRepo(cachePath, commit); err != nil {
			t.Errorf("%s cache invalid after hydration: %v", name, err)
		}
	}

	// The stable shim exists, is executable, and execs the cache-resolved
	// bundled bd lifecycle script.
	target := bundledGcBeadsBdScriptForTest(t)
	shimPath := gcBeadsBdScriptPath(city)
	info, err := os.Stat(shimPath)
	if err != nil {
		t.Fatalf("Stat(shim): %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("shim not executable: mode %v", info.Mode())
	}
	shim, err := os.ReadFile(shimPath)
	if err != nil {
		t.Fatalf("ReadFile(shim): %v", err)
	}
	if !strings.HasPrefix(string(shim), "#!/bin/sh") {
		t.Errorf("shim missing shebang:\n%s", shim)
	}
	if !strings.Contains(string(shim), target) {
		t.Errorf("shim does not exec bundled target %s:\n%s", target, shim)
	}

	// Idempotence: a second call succeeds and does not rewrite the shim.
	materializeBuiltinPacksForTest(t, city)
	after, err := os.Stat(shimPath)
	if err != nil {
		t.Fatalf("Stat(shim) after second call: %v", err)
	}
	if !after.ModTime().Equal(info.ModTime()) {
		t.Errorf("unchanged shim was rewritten: modtime %s → %s", info.ModTime(), after.ModTime())
	}
}

func TestEnsureBuiltinRuntimeAssetsSkipsShimForNonBdCity(t *testing.T) {
	clearGCEnv(t)
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, city)

	if _, err := os.Stat(gcBeadsBdScriptPath(city)); !os.IsNotExist(err) {
		t.Errorf("stat gc-beads-bd shim err = %v, want IsNotExist for non-bd city", err)
	}

	// Core (always required) is still hydrated for a file-provider city.
	commit := bundledPackImportCommit()
	coreSource, _ := builtinpacks.Source("core")
	coreCache, err := packman.RepoCachePath(coreSource, commit)
	if err != nil {
		t.Fatalf("RepoCachePath(core): %v", err)
	}
	if err := builtinpacks.ValidateSyntheticRepo(coreCache, commit); err != nil {
		t.Errorf("core cache invalid after hydration: %v", err)
	}
}

func TestEnsureBuiltinRuntimeAssetsPrunesRetiredSystemPacks(t *testing.T) {
	city := t.TempDir()

	// Simulate the retired materialized tree left behind by an older binary.
	stale := filepath.Join(city, citylayout.SystemPacksRoot, "maintenance")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pack.toml"), []byte("[pack]\nname = \"maintenance\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	materializeBuiltinPacksForTest(t, city)

	if _, err := os.Stat(filepath.Join(city, citylayout.SystemPacksRoot)); !os.IsNotExist(err) {
		t.Errorf("stat retired %s err = %v, want IsNotExist", citylayout.SystemPacksRoot, err)
	}
}

// TestPruneRetiredSystemPacksWaitsForLegacyIncludeMigration pins the legacy
// upgrade window ordering: on a city whose city.toml still composes builtin
// packs through .gc/system/packs includes, pruning must NOT delete the tree.
// Deleting it before the include migration leaves the city silently
// composing without those packs (dangling V1 includes skip with only a log
// line) until someone runs "gc doctor --fix". The tree is preserved and a
// once-per-city warning points at the doctor migration; once the includes
// are migrated the next prune removes the tree.
func TestPruneRetiredSystemPacksWaitsForLegacyIncludeMigration(t *testing.T) {
	city := t.TempDir()
	legacyToml := "[workspace]\nincludes = [\".gc/system/packs/core\"]\n\n[beads]\nprovider = \"file\"\n"
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(legacyToml), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(city, citylayout.SystemPacksRoot, "core")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "pack.toml"), []byte("[pack]\nname = \"core\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var warnings bytes.Buffer
	pruneRetiredSystemPacks(city, &warnings)

	if _, err := os.Stat(stale); err != nil {
		t.Fatalf("stat %s err = %v; the prune ran before the include migration", stale, err)
	}
	if !strings.Contains(warnings.String(), `run "gc doctor --fix"`) {
		t.Errorf("preserved-tree warning does not point at the doctor migration: %q", warnings.String())
	}

	// Once per city per process: a second gated prune does not re-warn.
	var second bytes.Buffer
	pruneRetiredSystemPacks(city, &second)
	if second.Len() != 0 {
		t.Errorf("second gated prune re-warned: %q", second.String())
	}

	// After the migration strips the legacy includes, the prune proceeds.
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\n\n[beads]\nprovider = \"file\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pruneRetiredSystemPacks(city, io.Discard)
	if _, err := os.Stat(filepath.Join(city, citylayout.SystemPacksRoot)); !os.IsNotExist(err) {
		t.Errorf("stat retired %s err = %v, want IsNotExist after migration", citylayout.SystemPacksRoot, err)
	}
}

// TestPruneRetiredSystemPacksGatesOnEveryCompositionRoute pins the widened
// prune gate: city config can compose through the retired tree on more
// routes than root workspace.includes — default-rig includes, rig includes,
// city / rig / default-rig import sources, the same surfaces in pack.toml,
// and any of these hosted in a local config fragment (fragments merge
// workspace includes / default-rig includes additively and concatenate
// [[rigs]]). The post-prune failure mode for rig includes and local-path
// import sources is a citywide hard config-load failure, and for workspace
// routes a silent core drop, so the gate must preserve the tree for every
// route until the reference is migrated.
func TestPruneRetiredSystemPacksGatesOnEveryCompositionRoute(t *testing.T) {
	const fragmentName = "legacy-fragment.toml"
	cases := []struct {
		name     string
		cityToml string
		packToml string
		fragment string
		wantKept bool
	}{
		{
			name:     "workspace-default-rig-includes",
			cityToml: "[workspace]\ndefault_rig_includes = [\".gc/system/packs/core\"]\n",
			wantKept: true,
		},
		{
			name:     "rig-includes",
			cityToml: "[workspace]\n\n[[rigs]]\nname = \"demo\"\npath = \"demo\"\nincludes = [\".gc/system/packs/core\"]\n",
			wantKept: true,
		},
		{
			name:     "rig-import-source",
			cityToml: "[workspace]\n\n[[rigs]]\nname = \"demo\"\npath = \"demo\"\n\n[rigs.imports.core]\nsource = \".gc/system/packs/core\"\n",
			wantKept: true,
		},
		{
			name:     "city-import-source",
			cityToml: "[workspace]\n\n[imports.core]\nsource = \".gc/system/packs/core\"\n",
			wantKept: true,
		},
		{
			name:     "default-rig-import-source",
			cityToml: "[workspace]\n\n[defaults.rig.imports.core]\nsource = \".gc/system/packs/core\"\n",
			wantKept: true,
		},
		{
			name:     "pack-toml-import-source",
			cityToml: "[workspace]\n",
			packToml: "[pack]\nname = \"demo\"\nschema = 2\n\n[imports.core]\nsource = \".gc/system/packs/core\"\n",
			wantKept: true,
		},
		{
			name:     "pack-toml-pack-includes",
			cityToml: "[workspace]\n",
			packToml: "[pack]\nname = \"demo\"\nschema = 2\nincludes = [\".gc/system/packs/core\"]\n",
			wantKept: true,
		},
		{
			name:     "fragment-workspace-includes",
			cityToml: "include = [\"" + fragmentName + "\"]\n\n[workspace]\n",
			fragment: "[workspace]\nincludes = [\".gc/system/packs/core\"]\n",
			wantKept: true,
		},
		{
			name:     "fragment-rig-import-source",
			cityToml: "include = [\"" + fragmentName + "\"]\n\n[workspace]\n",
			fragment: "[[rigs]]\nname = \"demo\"\npath = \"demo\"\n\n[rigs.imports.core]\nsource = \".gc/system/packs/core\"\n",
			wantKept: true,
		},
		{
			name:     "clean-fragment-and-rig-prunes",
			cityToml: "include = [\"" + fragmentName + "\"]\n\n[workspace]\n\n[[rigs]]\nname = \"demo\"\npath = \"demo\"\n",
			fragment: "[workspace]\n",
			wantKept: false,
		},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(tt.cityToml), 0o644); err != nil {
				t.Fatal(err)
			}
			if tt.packToml != "" {
				if err := os.WriteFile(filepath.Join(city, "pack.toml"), []byte(tt.packToml), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tt.fragment != "" {
				if err := os.WriteFile(filepath.Join(city, fragmentName), []byte(tt.fragment), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			stale := filepath.Join(city, citylayout.SystemPacksRoot, "core")
			if err := os.MkdirAll(stale, 0o755); err != nil {
				t.Fatal(err)
			}

			var warnings bytes.Buffer
			pruneRetiredSystemPacks(city, &warnings)

			if tt.wantKept {
				if _, err := os.Stat(stale); err != nil {
					t.Fatalf("stat %s err = %v; the prune deleted a tree this route still composes through", stale, err)
				}
				if !strings.Contains(warnings.String(), `run "gc doctor --fix"`) {
					t.Errorf("preserved-tree warning does not point at the doctor migration: %q", warnings.String())
				}
				return
			}
			if _, err := os.Stat(filepath.Join(city, citylayout.SystemPacksRoot)); !os.IsNotExist(err) {
				t.Errorf("stat retired %s err = %v, want IsNotExist for a config without legacy references", citylayout.SystemPacksRoot, err)
			}
			if warnings.Len() != 0 {
				t.Errorf("clean config emitted a preserved-tree warning: %q", warnings.String())
			}
		})
	}
}

// TestPruneRetiredSystemPacksFailsClosedOnUninspectableFragment pins the
// fail-closed contract for config fragments: when city.toml references a
// fragment the gate cannot inspect — missing, unreadable, or a remote
// include entry — the prune must preserve the (inert) tree silently. The
// fragment may declare legacy .gc/system/packs references the gate cannot
// see, and deleting the tree on a guess would reproduce the silent
// degraded window the gate exists to prevent.
func TestPruneRetiredSystemPacksFailsClosedOnUninspectableFragment(t *testing.T) {
	for _, tt := range []struct {
		name    string
		include string
	}{
		{name: "missing-local-fragment", include: "missing-fragment.toml"},
		{name: "remote-fragment", include: "https://github.com/example/config//fragment.toml"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			city := t.TempDir()
			cityToml := "include = [\"" + tt.include + "\"]\n\n[workspace]\n"
			if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte(cityToml), 0o644); err != nil {
				t.Fatal(err)
			}
			stale := filepath.Join(city, citylayout.SystemPacksRoot, "core")
			if err := os.MkdirAll(stale, 0o755); err != nil {
				t.Fatal(err)
			}

			var warnings bytes.Buffer
			pruneRetiredSystemPacks(city, &warnings)

			if _, err := os.Stat(stale); err != nil {
				t.Fatalf("stat %s err = %v; prune must fail closed for a fragment it cannot inspect", stale, err)
			}
			if warnings.Len() != 0 {
				t.Errorf("uninspectable fragment emitted a warning (expected silent preserve): %q", warnings.String())
			}
		})
	}
}

// TestPruneRetiredSystemPacksKeepsTreeWhenManifestUnreadable pins the
// fail-closed contract for the destructive prune: when city.toml exists but
// cannot be parsed, the prune must leave the retired tree alone (it is
// inert) rather than risk stripping composition state it cannot inspect.
func TestPruneRetiredSystemPacksKeepsTreeWhenManifestUnreadable(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace\nnot toml"), 0o644); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(city, citylayout.SystemPacksRoot, "core")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}

	pruneRetiredSystemPacks(city, io.Discard)

	if _, err := os.Stat(stale); err != nil {
		t.Errorf("stat %s err = %v; prune must not delete the tree under an unreadable manifest", stale, err)
	}
}

// TestEnsureBuiltinRuntimeAssetsRehydratesCorruptedCache pins the
// self-healing contract that replaced per-city materialization refresh:
// stale or corrupted bundled-source cache content is detected on the next
// EnsureBuiltinRuntimeAssets call — even after the per-city ready cache
// reported success, because the ready fast path revalidates via
// requiredBuiltinSourcesUsable — and rehydrated from the embedded packs.
func TestEnsureBuiltinRuntimeAssetsRehydratesCorruptedCache(t *testing.T) {
	clearGCEnv(t) // isolated GC_HOME so the corruption never touches the shared test cache
	city := t.TempDir()

	materializeBuiltinPacksForTest(t, city)

	target := bundledGcBeadsBdScriptForTest(t)
	if err := os.WriteFile(target, []byte("#!/bin/sh\necho corrupted\n"), 0o755); err != nil {
		t.Fatalf("corrupting cached script: %v", err)
	}

	if err := EnsureBuiltinRuntimeAssets(city, io.Discard); err != nil {
		t.Fatalf("EnsureBuiltinRuntimeAssets after corruption: %v", err)
	}

	want := readBundledPackFileForTest(t, "bd", "assets/scripts/gc-beads-bd.sh")
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(rehydrated script): %v", err)
	}
	if string(got) != want {
		t.Fatalf("corrupted cached script was not rehydrated to embedded content; got:\n%s", got)
	}
}

// TestEnsureBuiltinRuntimeAssetsRehydratesEvictedOptionalLockedBundledCache
// pins the ready-fast-path repair of optional locked bundled imports. Once a
// city is readied, EnsureBuiltinRuntimeAssets short-circuits on the ready fast
// path, and that fast path validates only the required builtin sources
// (core/bd). gastown is bundled but never required, so an eviction of its
// synthetic cache after the city is ready would otherwise be skipped by the
// fast path, leaving config load to fail on the locked-but-missing cache. The
// fast path must also validate every canonical bundled source pinned in
// packs.lock and re-hydrate gastown here.
func TestEnsureBuiltinRuntimeAssetsRehydratesEvictedOptionalLockedBundledCache(t *testing.T) {
	clearGCEnv(t) // isolated GC_HOME so eviction never touches the shared test cache
	city := t.TempDir()

	// Pin the optional gastown bundled source in packs.lock at its canonical
	// commit; the runtime preflight hydrates it as a locked bundled import
	// even though a default bd-provider city requires only core and bd.
	source := config.PublicGastownPackSource
	commit := strings.TrimPrefix(config.PublicGastownPackVersion, "sha:")
	writePreflightImportLock(t, city, commit)

	// Ready the city: hydrate required core/bd plus the locked bundled
	// gastown source and mark the runtime state ready.
	materializeBuiltinPacksForTest(t, city)

	cachePath, err := packman.RepoCachePath(source, commit)
	if err != nil {
		t.Fatalf("RepoCachePath(gastown): %v", err)
	}
	if err := builtinpacks.ValidateSyntheticRepo(cachePath, commit); err != nil {
		t.Fatalf("gastown cache invalid after ready: %v", err)
	}

	// Evict only the optional bundled cache. The required core/bd caches stay
	// valid, so the ready fast path's requiredBuiltinSourcesUsable check still
	// passes — the only thing that can force re-hydration is validating the
	// locked bundled sources too.
	if err := os.RemoveAll(cachePath); err != nil {
		t.Fatalf("evicting gastown cache: %v", err)
	}

	if err := EnsureBuiltinRuntimeAssets(city, io.Discard); err != nil {
		t.Fatalf("EnsureBuiltinRuntimeAssets after optional cache eviction: %v", err)
	}

	if err := builtinpacks.ValidateSyntheticRepo(cachePath, commit); err != nil {
		t.Fatalf("optional locked bundled cache not rehydrated after ready fast path: %v", err)
	}
}

func TestConfigLoadBoundarySkipsWithoutGCHome(t *testing.T) {
	// Under `go test` ImplicitGCHome returns "" when GC_HOME is unset
	// (hermetic-test guard on os.Args[0] suffix ".test"), so
	// config.GlobalRepoCacheRoot errors. HOME points at a throwaway dir so
	// that even if the guard ever regressed, the fallback could not touch
	// the developer's real ~/.gc.
	t.Setenv("GC_HOME", "")
	home := t.TempDir()
	t.Setenv("HOME", home)

	dir := t.TempDir()
	if err := writeBuiltinPackLoadTestCity(dir); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := ensureBuiltinPacksForConfigLoad(fsys.OSFS{}, filepath.Join(dir, "city.toml"), io.Discard); err != nil {
		t.Fatalf("ensureBuiltinPacksForConfigLoad without GC_HOME: %v", err)
	}

	// The retired tree is still pruned, but nothing is hydrated or written.
	if _, err := os.Stat(filepath.Join(dir, citylayout.SystemPacksRoot)); !os.IsNotExist(err) {
		t.Errorf("stat retired %s err = %v, want IsNotExist", citylayout.SystemPacksRoot, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gc", "scripts")); !os.IsNotExist(err) {
		t.Errorf("stat .gc/scripts err = %v, want IsNotExist (no shim without GC_HOME)", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".gc")); !os.IsNotExist(err) {
		t.Errorf("stat HOME/.gc err = %v, want IsNotExist (no cache without GC_HOME)", err)
	}
}

func TestLoadCityConfigWithoutBuiltinPackRefreshFSDoesNotTouchDisk(t *testing.T) {
	dir := t.TempDir()
	if err := writeBuiltinPackLoadTestCity(dir); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(dir, citylayout.SystemPacksRoot, "maintenance")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadCityConfigWithoutBuiltinPackRefreshFS(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("loadCityConfigWithoutBuiltinPackRefreshFS(OSFS) error: %v", err)
	}
	if cfg == nil {
		t.Fatal("loadCityConfigWithoutBuiltinPackRefreshFS returned nil config")
	}

	// The read-only loader must neither write the shim nor prune the
	// retired tree.
	if _, err := os.Stat(filepath.Join(dir, ".gc", "scripts")); !os.IsNotExist(err) {
		t.Errorf("stat .gc/scripts err = %v, want IsNotExist on read-only load path", err)
	}
	if _, err := os.Stat(legacy); err != nil {
		t.Errorf("retired tree was pruned on read-only load path: %v", err)
	}
}

func TestLoadCityConfigFSHydratesBuiltinRuntimeAssets(t *testing.T) {
	dir := t.TempDir()
	if err := writeBuiltinPackLoadTestCity(dir); err != nil {
		t.Fatal(err)
	}

	if _, err := loadCityConfigFS(fsys.OSFS{}, filepath.Join(dir, "city.toml"), io.Discard); err != nil {
		t.Fatalf("loadCityConfigFS(OSFS) error: %v", err)
	}

	// The config-load boundary self-heals the bundled caches and shim for
	// the default bd-provider city.
	if _, err := os.Stat(gcBeadsBdScriptPath(dir)); err != nil {
		t.Errorf("gc-beads-bd shim missing after loadCityConfigFS: %v", err)
	}
	commit := bundledPackImportCommit()
	for _, name := range []string{"core", "bd"} {
		source, ok := builtinpacks.Source(name)
		if !ok {
			t.Fatalf("builtinpacks.Source(%q) not registered", name)
		}
		cachePath, err := packman.RepoCachePath(source, commit)
		if err != nil {
			t.Fatalf("RepoCachePath(%s): %v", name, err)
		}
		if err := builtinpacks.ValidateSyntheticRepo(cachePath, commit); err != nil {
			t.Errorf("%s cache invalid after loadCityConfigFS: %v", name, err)
		}
	}
}
