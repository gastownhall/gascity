package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/rogpeppe/go-internal/testscript"
)

func setTestscriptEnvDefault(key, value string) {
	if os.Getenv(key) != "" {
		return
	}
	_ = os.Setenv(key, value)
}

func configureTestscriptEnvDefaults() {
	// Testscript defaults to fake/local backends so a missing env line in a
	// txtar file never falls through to real tmux or auto-detected agent CLIs.
	// Tests can still opt into a specific backend explicitly, e.g.
	// GC_SESSION=fail or GC_SESSION=tmux.
	setTestscriptEnvDefault("GC_SESSION", "fake")
	setTestscriptEnvDefault("GC_BEADS", "file")
	setTestscriptEnvDefault("GC_DOLT", "skip")
	setTestscriptEnvDefault("GC_BOOTSTRAP", "skip")
}

func configureIsolatedRuntimeEnv(t *testing.T) {
	t.Helper()
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	if os.Getenv("GC_SESSION") == "" {
		t.Setenv("GC_SESSION", "fake")
	}
	if os.Getenv("GC_BEADS") == "" {
		t.Setenv("GC_BEADS", "file")
	}
	if os.Getenv("GC_DOLT") == "" {
		t.Setenv("GC_DOLT", "skip")
	}
	if os.Getenv("GC_BOOTSTRAP") == "" {
		t.Setenv("GC_BOOTSTRAP", "skip")
	}
}

func configureSupervisorHooksForTests() {
	ensureSupervisorRunningHook = func(_, _ io.Writer) int { return 0 }
	reloadSupervisorHook = func(_, _ io.Writer) int { return 0 }
	supervisorAliveHook = func() int { return 0 }
	startNudgePoller = func(string, string, string) error { return nil }
	initLookPath = func(file string) (string, error) { return file, nil }
	initProbeProvidersReadiness = func(_ context.Context, providers []string, _ bool) (map[string]api.ReadinessItem, error) {
		out := make(map[string]api.ReadinessItem, len(providers))
		for _, provider := range providers {
			displayName := provider
			if spec, ok := config.BuiltinProviders()[provider]; ok && spec.DisplayName != "" {
				displayName = spec.DisplayName
			}
			out[provider] = api.ReadinessItem{
				Name:        provider,
				Kind:        api.ProbeKindProvider,
				DisplayName: displayName,
				Status:      api.ProbeStatusConfigured,
			}
		}
		return out, nil
	}
	registerCityWithSupervisorTestHook = func(cityPath, commandName string, stdout, stderr io.Writer) (bool, int) {
		switch commandName {
		case "gc start":
			return true, doStartStandalone([]string{cityPath}, false, stdout, stderr)
		case "gc init", "gc register":
			return true, 0
		default:
			return false, 0
		}
	}
}

func markFakeCityScaffold(f *fsys.Fake, cityPath string) {
	f.Dirs[filepath.Join(cityPath, citylayout.RuntimeRoot)] = true
	f.Dirs[filepath.Join(cityPath, citylayout.CacheRoot)] = true
	f.Dirs[filepath.Join(cityPath, citylayout.SystemRoot)] = true
	f.Dirs[filepath.Join(cityPath, citylayout.RuntimeRoot, "runtime")] = true
	f.Files[filepath.Join(cityPath, citylayout.RuntimeRoot, "events.jsonl")] = nil
}

func explicitAgents(agents []config.Agent) []config.Agent {
	var out []config.Agent
	for _, a := range agents {
		if a.Implicit {
			continue
		}
		out = append(out, a)
	}
	return out
}

func TestMain(m *testing.M) {
	gcHome, err := os.MkdirTemp("", "gascity-gc-home-*")
	if err != nil {
		panic(err)
	}
	runtimeDir, err := os.MkdirTemp("", "gascity-runtime-*")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("GC_HOME", gcHome); err != nil {
		panic(err)
	}
	if err := os.Setenv("XDG_RUNTIME_DIR", runtimeDir); err != nil {
		panic(err)
	}
	providerStubDir, err := installTestProviderStubs()
	if err != nil {
		panic(err)
	}
	defer func() { _ = os.RemoveAll(providerStubDir) }()
	pathValue := providerStubDir
	if existingPath := os.Getenv("PATH"); existingPath != "" {
		pathValue += string(os.PathListSeparator) + existingPath
	}
	if err := os.Setenv("PATH", pathValue); err != nil {
		panic(err)
	}
	configureSupervisorHooksForTests()
	testscript.Main(m, map[string]func(){
		"gc": func() {
			configureTestscriptEnvDefaults()
			os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
		},
		"bd": bdTestCmd,
	})
}

func TestTutorial01(t *testing.T) {
	testscript.Run(t, newTestscriptParams(t))
}

func TestImportMigrateScript(t *testing.T) {
	testscript.Run(t, newTestscriptParams(t, filepath.Join("testdata", "migrate-v2.txtar")))
}

func TestPackV2ImportsScript(t *testing.T) {
	testscript.Run(t, newTestscriptParams(t, filepath.Join("testdata", "pack-v2-imports.txtar")))
}

func newTestscriptParams(t *testing.T, files ...string) testscript.Params {
	params := testscript.Params{
		Dir:         "testdata",
		WorkdirRoot: shortSocketTempDir(t, "gc-testscript-"),
		Setup: func(env *testscript.Env) error {
			gcHome := filepath.Join(env.WorkDir, ".gc-home")
			runtimeDir := filepath.Join(env.WorkDir, ".runtime")
			if err := os.MkdirAll(gcHome, 0o755); err != nil {
				return err
			}
			if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
				return err
			}
			env.Setenv("GC_HOME", gcHome)
			env.Setenv("XDG_RUNTIME_DIR", runtimeDir)
			return nil
		},
	}
	if len(files) > 0 {
		params.Dir = ""
		params.Files = append([]string(nil), files...)
	}
	return params
}

// --- gc version ---

func TestVersion(t *testing.T) {
	var stdout bytes.Buffer
	code := run([]string{"version"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Errorf("run([version]) = %d, want 0", code)
	}
	if got := strings.TrimSpace(stdout.String()); got != "dev" {
		t.Errorf("stdout = %q, want %q", got, "dev")
	}

	stdout.Reset()
	code = run([]string{"version", "--long"}, &stdout, &bytes.Buffer{})
	if code != 0 {
		t.Errorf("run([version --long]) = %d, want 0", code)
	}
	longOut := stdout.String()
	if !strings.Contains(longOut, "commit:") {
		t.Errorf("stdout missing 'commit:': %q", longOut)
	}
	if !strings.Contains(longOut, "built:") {
		t.Errorf("stdout missing 'built:': %q", longOut)
	}
}

func TestConfigureTestscriptEnvDefaultsSetsMissingValues(t *testing.T) {
	t.Setenv("GC_SESSION", "")
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_DOLT", "")

	configureTestscriptEnvDefaults()

	if got := os.Getenv("GC_SESSION"); got != "fake" {
		t.Fatalf("GC_SESSION = %q, want fake", got)
	}
	if got := os.Getenv("GC_BEADS"); got != "file" {
		t.Fatalf("GC_BEADS = %q, want file", got)
	}
	if got := os.Getenv("GC_DOLT"); got != "skip" {
		t.Fatalf("GC_DOLT = %q, want skip", got)
	}
}

func TestConfigureTestscriptEnvDefaultsPreservesOverrides(t *testing.T) {
	t.Setenv("GC_SESSION", "fail")
	t.Setenv("GC_BEADS", "exec:/tmp/custom-beads")
	t.Setenv("GC_DOLT", "run")

	configureTestscriptEnvDefaults()

	if got := os.Getenv("GC_SESSION"); got != "fail" {
		t.Fatalf("GC_SESSION = %q, want fail", got)
	}
	if got := os.Getenv("GC_BEADS"); got != "exec:/tmp/custom-beads" {
		t.Fatalf("GC_BEADS = %q, want explicit override", got)
	}
	if got := os.Getenv("GC_DOLT"); got != "run" {
		t.Fatalf("GC_DOLT = %q, want explicit override", got)
	}
}

// --- findCity ---

func TestFindCity(t *testing.T) {
	t.Run("canonical", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}

		got, err := findCity(dir)
		if err != nil {
			t.Fatalf("findCity(%q) error: %v", dir, err)
		}
		if got != dir {
			t.Errorf("findCity(%q) = %q, want %q", dir, got, dir)
		}
	})

	t.Run("found", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := findCity(dir)
		if err != nil {
			t.Fatalf("findCity(%q) error: %v", dir, err)
		}
		if got != dir {
			t.Errorf("findCity(%q) = %q, want %q", dir, got, dir)
		}
	})

	t.Run("nested", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(dir, "sub", "deep")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := findCity(nested)
		if err != nil {
			t.Fatalf("findCity(%q) error: %v", nested, err)
		}
		if got != dir {
			t.Errorf("findCity(%q) = %q, want %q", nested, got, dir)
		}
	})

	t.Run("parent_canonical_outranks_child_legacy", func(t *testing.T) {
		parent := t.TempDir()
		if err := os.WriteFile(filepath.Join(parent, "city.toml"), []byte("[workspace]\nname = \"parent\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		child := filepath.Join(parent, "child")
		if err := os.MkdirAll(filepath.Join(child, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(child, "deep")
		if err := os.MkdirAll(nested, 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := findCity(nested)
		if err != nil {
			t.Fatalf("findCity(%q) error: %v", nested, err)
		}
		if got != parent {
			t.Errorf("findCity(%q) = %q, want %q", nested, got, parent)
		}
	})

	t.Run("not_found", func(t *testing.T) {
		// Use an explicit /tmp-rooted dir so the upward walk cannot
		// accidentally hit a real .gc/ directory on the host (e.g.
		// a running city under $HOME).
		dir, err := os.MkdirTemp("/tmp", "gc-test-notfound-*")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.RemoveAll(dir) }()

		_, err = findCity(dir)
		if err == nil {
			t.Fatal("findCity() should fail without city.toml or .gc/")
		}
		if !strings.Contains(err.Error(), "not in a city directory") {
			t.Errorf("error = %q, want 'not in a city directory'", err)
		}
	})

	t.Run("not_found_ignores_stray_home_city_toml", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

		if err := os.WriteFile(filepath.Join(homeDir, "city.toml"), []byte("[workspace]\nname = \"stray\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(homeDir, "project", "deep")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		_, err := findCity(dir)
		if err == nil {
			t.Fatal("findCity() should fail when only a stray $HOME/city.toml exists")
		}
		if !strings.Contains(err.Error(), "not in a city directory") {
			t.Errorf("error = %q, want 'not in a city directory'", err)
		}
	})

	t.Run("not_found_ignores_supervisor_home_runtime_root", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

		if err := os.MkdirAll(filepath.Join(homeDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(homeDir, "project", "deep")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		_, err := findCity(dir)
		if err == nil {
			t.Fatal("findCity() should fail when only supervisor $HOME/.gc exists")
		}
		if !strings.Contains(err.Error(), "not in a city directory") {
			t.Errorf("error = %q, want 'not in a city directory'", err)
		}
	})

	t.Run("nested_city_below_home_boundary_still_found", func(t *testing.T) {
		homeDir := t.TempDir()
		t.Setenv("HOME", homeDir)
		t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))

		cityDir := filepath.Join(homeDir, "cities", "alpha")
		if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"alpha\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(cityDir, "project", "deep")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}

		got, err := findCity(dir)
		if err != nil {
			t.Fatalf("findCity(%q) error: %v", dir, err)
		}
		if got != cityDir {
			t.Errorf("findCity(%q) = %q, want %q", dir, got, cityDir)
		}
	})

	t.Run("respects_gc_ceiling_directories", func(t *testing.T) {
		root := t.TempDir()
		parent := filepath.Join(root, "parent")
		if err := os.MkdirAll(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(parent, "city.toml"), []byte("[workspace]\nname = \"parent\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(parent, "child", "deep")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GC_CEILING_DIRECTORIES", parent)

		_, err := findCity(dir)
		if err == nil {
			t.Fatal("findCity() should fail when GC_CEILING_DIRECTORIES excludes the ancestor city root")
		}
		if !strings.Contains(err.Error(), "not in a city directory") {
			t.Errorf("error = %q, want 'not in a city directory'", err)
		}
	})
}

// --- resolveCity ---

func TestResolveCityFlag(t *testing.T) {
	t.Run("flag_valid", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		old := cityFlag
		cityFlag = dir
		t.Cleanup(func() { cityFlag = old })

		got, err := resolveCity()
		if err != nil {
			t.Fatalf("resolveCity() error: %v", err)
		}
		if canonicalTestPath(got) != canonicalTestPath(dir) {
			t.Errorf("resolveCity() = %q, want %q", got, dir)
		}
	})

	t.Run("flag_no_gc_dir", func(t *testing.T) {
		dir := t.TempDir() // no .gc/ inside
		old := cityFlag
		cityFlag = dir
		t.Cleanup(func() { cityFlag = old })

		_, err := resolveCity()
		if err == nil {
			t.Fatal("resolveCity() should fail without .gc/")
		}
		if !strings.Contains(err.Error(), "not a city directory") {
			t.Errorf("error = %q, want 'not a city directory'", err)
		}
	})

	t.Run("flag_empty_fallback", func(t *testing.T) {
		// With empty flag, should fall back to cwd-based discovery.
		// Clear GC_CITY so the cwd fallback is actually exercised.
		t.Setenv("GC_CITY", "")
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		old := cityFlag
		cityFlag = ""
		t.Cleanup(func() { cityFlag = old })

		orig, _ := os.Getwd()
		t.Cleanup(func() { _ = os.Chdir(orig) })
		if err := os.Chdir(dir); err != nil {
			t.Fatal(err)
		}

		got, err := resolveCity()
		if err != nil {
			t.Fatalf("resolveCity() error: %v", err)
		}
		// os.Getwd() resolves symlinks (e.g. /var → /private/var on macOS),
		// so compare against the resolved path.
		want, _ := filepath.EvalSymlinks(dir)
		if got != want {
			t.Errorf("resolveCity() = %q, want %q", got, want)
		}
	})

	t.Run("gc_city_env_prefers_real_city_from_worktree", func(t *testing.T) {
		cityDir := t.TempDir()
		workDir := filepath.Join(cityDir, ".gc", "worktrees", "demo", "polecat-1")
		if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"),
			[]byte("[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(workDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}

		old := cityFlag
		cityFlag = ""
		t.Cleanup(func() { cityFlag = old })
		t.Setenv("GC_CITY", cityDir)

		orig, _ := os.Getwd()
		t.Cleanup(func() { _ = os.Chdir(orig) })
		if err := os.Chdir(workDir); err != nil {
			t.Fatal(err)
		}

		got, err := resolveCity()
		if err != nil {
			t.Fatalf("resolveCity() error: %v", err)
		}
		if canonicalTestPath(got) != canonicalTestPath(cityDir) {
			t.Errorf("resolveCity() = %q, want %q", got, cityDir)
		}
	})

	t.Run("gc_city_path_env_fallback", func(t *testing.T) {
		cityDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}

		old := cityFlag
		cityFlag = ""
		t.Cleanup(func() { cityFlag = old })
		t.Setenv("GC_CITY", "")
		t.Setenv("GC_CITY_PATH", cityDir)

		got, err := resolveCity()
		if err != nil {
			t.Fatalf("resolveCity() error: %v", err)
		}
		if canonicalTestPath(got) != canonicalTestPath(cityDir) {
			t.Errorf("resolveCity() = %q, want %q", got, cityDir)
		}
	})

	t.Run("gc_dir_env_fallback", func(t *testing.T) {
		cityDir := t.TempDir()
		workDir := filepath.Join(cityDir, "rigs", "demo")
		if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			t.Fatal(err)
		}

		old := cityFlag
		cityFlag = ""
		t.Cleanup(func() { cityFlag = old })
		t.Setenv("GC_CITY", "")
		t.Setenv("GC_CITY_PATH", "")
		t.Setenv("GC_CITY_ROOT", "")
		t.Setenv("GC_DIR", workDir)

		got, err := resolveCity()
		if err != nil {
			t.Fatalf("resolveCity() error: %v", err)
		}
		if got != cityDir {
			t.Errorf("resolveCity() = %q, want %q", got, cityDir)
		}
	})

	t.Run("gc_city_path_env_fallback", func(t *testing.T) {
		cityDir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}

		old := cityFlag
		cityFlag = ""
		t.Cleanup(func() { cityFlag = old })
		t.Setenv("GC_CITY", "")
		t.Setenv("GC_CITY_PATH", cityDir)

		got, err := resolveCity()
		if err != nil {
			t.Fatalf("resolveCity() error: %v", err)
		}
		if got != cityDir {
			t.Errorf("resolveCity() = %q, want %q", got, cityDir)
		}
	})

	t.Run("gc_dir_env_fallback", func(t *testing.T) {
		cityDir := t.TempDir()
		workDir := filepath.Join(cityDir, "rigs", "demo")
		if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			t.Fatal(err)
		}

		old := cityFlag
		cityFlag = ""
		t.Cleanup(func() { cityFlag = old })
		t.Setenv("GC_CITY", "")
		t.Setenv("GC_CITY_PATH", "")
		t.Setenv("GC_CITY_ROOT", "")
		t.Setenv("GC_DIR", workDir)

		got, err := resolveCity()
		if err != nil {
			t.Fatalf("resolveCity() error: %v", err)
		}
		if got != cityDir {
			t.Errorf("resolveCity() = %q, want %q", got, cityDir)
		}
	})
}

// --- doRigAdd (with fsys.Fake) ---

func TestDoRigAddCreatesDirIfMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "newproject") // does not exist yet
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	// Verify the rig directory was created.
	fi, err := os.Stat(rigPath)
	if err != nil {
		t.Fatalf("rig dir not created: %v", err)
	}
	if !fi.IsDir() {
		t.Error("rig path is not a directory")
	}
}

func TestDoRigAddMkdirRigPathFails(t *testing.T) {
	f := fsys.NewFake()
	// rigPath doesn't exist and MkdirAll will fail.
	f.Errors["/projects/myapp"] = fmt.Errorf("permission denied")

	var stderr bytes.Buffer
	code := doRigAdd(f, "/city", "/projects/myapp", nil, "", "", false, false, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doRigAdd = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("stderr = %q, want 'permission denied'", stderr.String())
	}
}

func TestDoRigAddNotADirectory(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/projects/myapp"] = []byte("not a dir") // file, not directory

	var stderr bytes.Buffer
	code := doRigAdd(f, "/city", "/projects/myapp", nil, "", "", false, false, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doRigAdd = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "is not a directory") {
		t.Errorf("stderr = %q, want 'is not a directory'", stderr.String())
	}
}

func TestDoRigAddWithGit(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	// Use real temp dirs so writeAllRoutes (which uses os.MkdirAll) works.
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "myapp")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Detected git repo") {
		t.Errorf("stdout missing 'Detected git repo': %q", out)
	}
	if !strings.Contains(out, "Rig added.") {
		t.Errorf("stdout missing 'Rig added.': %q", out)
	}
}

func TestDoRigAddWithoutGit(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	cityPath := t.TempDir()
	rigPath := filepath.Join(t.TempDir(), "myapp")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"test\"\n\n[[agent]]\nname = \"mayor\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doRigAdd(fsys.OSFS{}, cityPath, rigPath, nil, "", "", false, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if strings.Contains(out, "Detected git repo") {
		t.Errorf("stdout should not contain 'Detected git repo': %q", out)
	}
	if !strings.Contains(out, "Rig added.") {
		t.Errorf("stdout missing 'Rig added.': %q", out)
	}
}

// --- doRigList (with fsys.Fake) ---

func TestDoRigListConfigLoadFails(t *testing.T) {
	f := fsys.NewFake()
	f.Errors[filepath.Join("/city", "city.toml")] = fmt.Errorf("no such file")

	var stderr bytes.Buffer
	code := doRigList(f, "/city", false, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doRigList = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no such file") {
		t.Errorf("stderr = %q, want 'no such file'", stderr.String())
	}
}

func TestDoRigListSuccess(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte("[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"alpha\"\npath = \"/projects/alpha\"\n\n[[rigs]]\nname = \"beta\"\npath = \"/projects/beta\"\n")

	var stdout, stderr bytes.Buffer
	code := doRigList(f, "/city", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "alpha:") {
		t.Errorf("stdout missing 'alpha:': %q", out)
	}
	if !strings.Contains(out, "beta:") {
		t.Errorf("stdout missing 'beta:': %q", out)
	}
}

func TestDoRigListJSON(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte("[workspace]\nname = \"test-city\"\n\n[[agent]]\nname = \"mayor\"\n\n[[rigs]]\nname = \"alpha\"\npath = \"/projects/alpha\"\n")

	var stdout, stderr bytes.Buffer
	code := doRigList(f, "/city", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doRigList --json = %d, want 0; stderr: %s", code, stderr.String())
	}
	var result RigListJSON
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.CityPath != "/city" {
		t.Errorf("city_path = %q, want /city", result.CityPath)
	}
	if len(result.Rigs) != 2 {
		t.Fatalf("got %d rigs, want 2", len(result.Rigs))
	}
	if !result.Rigs[0].HQ {
		t.Errorf("first rig should be HQ")
	}
	if result.Rigs[1].Name != "alpha" {
		t.Errorf("second rig name = %q, want alpha", result.Rigs[1].Name)
	}
	if result.Rigs[1].Path != "/projects/alpha" {
		t.Errorf("second rig path = %q, want /projects/alpha", result.Rigs[1].Path)
	}
}

// --- sessionName ---

func TestSessionName(t *testing.T) {
	got := sessionName(nil, "bright-lights", "mayor", "")
	want := "mayor"
	if got != want {
		t.Errorf("sessionName = %q, want %q", got, want)
	}
}

func TestSessionNameTmuxOverride(t *testing.T) {
	// GC_TMUX_SESSION overrides the computed session name, allowing
	// agents inside Docker/K8s containers to target the correct tmux
	// session for metadata (drain, restart).
	t.Setenv("GC_TMUX_SESSION", "agent")
	got := sessionName(nil, "bright-lights", "mayor", "")
	want := "agent"
	if got != want {
		t.Errorf("sessionName with GC_TMUX_SESSION = %q, want %q", got, want)
	}
}

func TestResolveSessionNameWithStore(t *testing.T) {
	store := beads.NewMemStore()

	// Create a session bead for "worker" template.
	b, err := store.Create(beads.Bead{
		Title: "worker",
		Type:  "session",
		Labels: []string{
			"gc:session",
			"template:worker",
		},
		Metadata: map[string]string{
			"template":     "worker",
			"common_name":  "worker",
			"session_name": "s-gc-42",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// lookupSessionNameOrLegacy should find the bead-derived name.
	got := lookupSessionNameOrLegacy(store, "city", "worker", "")
	if got != "s-gc-42" {
		t.Errorf("lookupSessionNameOrLegacy(store, worker) = %q, want %q", got, "s-gc-42")
	}

	// With nil store, should fall back to legacy.
	got = lookupSessionNameOrLegacy(nil, "city", "worker", "")
	if got != "worker" {
		t.Errorf("lookupSessionNameOrLegacy(nil, worker) = %q, want %q", got, "worker")
	}

	// sessionNameFromBeadID derivation.
	got = sessionNameFromBeadID(b.ID)
	want := "s-" + strings.ReplaceAll(b.ID, "/", "--")
	if got != want {
		t.Errorf("sessionNameFromBeadID(%q) = %q, want %q", b.ID, got, want)
	}
}

func TestFindSessionNameByTemplate_SkipsClosedBeads(t *testing.T) {
	store := beads.NewMemStore()
	b, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{"gc:session", "template:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"common_name":  "worker",
			"session_name": "s-gc-99",
			"state":        "asleep",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Close the bead — it should be skipped.
	if err := store.Close(b.ID); err != nil {
		t.Fatal(err)
	}
	got := findSessionNameByTemplate(store, "worker")
	if got != "" {
		t.Errorf("findSessionNameByTemplate(closed bead) = %q, want empty", got)
	}
}

func TestFindSessionNameByTemplate_SkipsPoolSlotBeads(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Title:  "worker-1",
		Type:   "session",
		Labels: []string{"gc:session", "template:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"common_name":  "worker-1",
			"session_name": "s-gc-50",
			"pool_slot":    "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Querying for the base template "worker" should NOT match the pool instance.
	got := findSessionNameByTemplate(store, "worker")
	if got != "" {
		t.Errorf("findSessionNameByTemplate(pool_slot bead) = %q, want empty", got)
	}
}

func TestFindSessionNameByTemplate_SkipsEmptySessionName(t *testing.T) {
	store := beads.NewMemStore()
	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{"gc:session", "template:worker"},
		Metadata: map[string]string{
			"template":    "worker",
			"common_name": "worker",
			// session_name intentionally missing
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := findSessionNameByTemplate(store, "worker")
	if got != "" {
		t.Errorf("findSessionNameByTemplate(empty session_name) = %q, want empty", got)
	}
}

func TestDiscoverSessionBeads_IncludesBeadCreatedSessions(t *testing.T) {
	store := beads.NewMemStore()

	// Create a session bead as if "gc session new" created it.
	_, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   "session",
		Labels: []string{sessionBeadLabel, "template:helper"},
		Metadata: map[string]string{
			"template":     "helper",
			"session_name": "s-gc-100",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "helper", StartCommand: "echo", MaxActiveSessions: intPtr(1)},
		},
	}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	desired := make(map[string]TemplateParams)
	discoverSessionBeads(bp, cfg, desired, io.Discard)

	if _, ok := desired["s-gc-100"]; !ok {
		t.Errorf("expected bead-created session s-gc-100 in desired state, got keys: %v", mapKeys(desired))
	}
}

func TestDiscoverSessionBeads_SkipsAlreadyDesired(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "helper",
		Type:   "session",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":     "helper",
			"session_name": "s-gc-100",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "helper"},
		},
	}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	// Pre-populate desired state — bead should be skipped.
	desired := map[string]TemplateParams{
		"s-gc-100": {SessionName: "s-gc-100"},
	}
	discoverSessionBeads(bp, cfg, desired, io.Discard)

	// Should still be exactly 1 entry (not duplicated).
	if len(desired) != 1 {
		t.Errorf("expected 1 desired entry, got %d", len(desired))
	}
}

func TestDiscoverSessionBeads_SkipsNoTemplate(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "orphan",
		Type:   "session",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"session_name": "s-gc-200",
			"state":        "active",
			// No template metadata
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	desired := make(map[string]TemplateParams)
	discoverSessionBeads(bp, cfg, desired, io.Discard)

	if len(desired) != 0 {
		t.Errorf("expected 0 desired entries for bead without template, got %d", len(desired))
	}
}

func TestDiscoverSessionBeads_SkipsPoolAgentWithZeroDesired(t *testing.T) {
	store := beads.NewMemStore()

	// A polecat pool session bead left over from a previous run.
	_, err := store.Create(beads.Bead{
		Title:  "polecat-1",
		Type:   "session",
		Labels: []string{sessionBeadLabel, "template:polecat"},
		Metadata: map[string]string{
			"template":     "polecat",
			"session_name": "polecat--polecat-1",
			"state":        "stopped",
			"pool_slot":    "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "polecat",
				StartCommand:      "echo",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1),
			},
		},
	}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	// Empty desired = pool eval returned 0 (no work).
	desired := make(map[string]TemplateParams)
	discoverSessionBeads(bp, cfg, desired, io.Discard)

	if len(desired) != 0 {
		t.Errorf("pool agent with 0 desired should not be re-added from stale bead, got %d entries: %v",
			len(desired), mapKeys(desired))
	}
}

func TestDiscoverSessionBeads_IncludesPoolAgentWithDesired(t *testing.T) {
	store := beads.NewMemStore()

	// Two pool session beads — slot 1 and slot 2.
	for _, slot := range []string{"1", "2"} {
		_, err := store.Create(beads.Bead{
			Title:  "polecat-" + slot,
			Type:   "session",
			Labels: []string{sessionBeadLabel, "template:polecat"},
			Metadata: map[string]string{
				"template":     "polecat",
				"session_name": "polecat--polecat-" + slot,
				"state":        "stopped",
				"pool_slot":    slot,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "polecat",
				StartCommand:      "echo",
				MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1),
			},
		},
	}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	// Simulate pool eval returning 1 — slot 1 is in desired.
	desired := map[string]TemplateParams{
		"polecat--polecat-1": {TemplateName: "polecat", SessionName: "polecat--polecat-1"},
	}
	discoverSessionBeads(bp, cfg, desired, io.Discard)

	// Slot 1 was already desired (should stay). Slot 2 is stopped and may
	// or may not be included depending on pool discovery logic.
	// Verify slot 1 is still present.
	if _, ok := desired["polecat--polecat-1"]; !ok {
		t.Errorf("slot 1 should remain in desired, got keys: %v", mapKeys(desired))
	}
}

func TestFindSessionNameByTemplate_PrefersAgentNameMatch(t *testing.T) {
	store := beads.NewMemStore()

	// Create a managed agent bead (has agent_name from syncSessionBeads).
	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{"gc:session", "agent:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"agent_name":   "worker",
			"session_name": "s-managed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create an ad-hoc session bead (no agent_name, from gc session new).
	_, err = store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{"gc:session", "template:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "s-adhoc",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Should prefer the managed bead (agent_name match).
	got := findSessionNameByTemplate(store, "worker")
	if got != "s-managed" {
		t.Errorf("findSessionNameByTemplate with managed + ad-hoc = %q, want s-managed", got)
	}
}

func TestFindSessionNameByTemplate_TemplateMismatchNotFound(t *testing.T) {
	store := beads.NewMemStore()

	// Create a bead with template "worker" but query "myrig/worker".
	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{"gc:session", "template:worker"},
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "s-gc-99",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Querying for rig-qualified name should NOT match bare template.
	got := findSessionNameByTemplate(store, "myrig/worker")
	if got != "" {
		t.Errorf("findSessionNameByTemplate(myrig/worker) = %q, want empty (template mismatch)", got)
	}
}

func TestFindSessionNameByTemplate_UsesLegacyAgentLabelForPoolInstance(t *testing.T) {
	store := beads.NewMemStore()

	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{sessionBeadLabel, "agent:myrig/worker-1"},
		Metadata: map[string]string{
			"template":     "worker",
			"pool_slot":    "1",
			"session_name": "s-legacy-worker-1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	got := findSessionNameByTemplate(store, "myrig/worker-1")
	if got != "s-legacy-worker-1" {
		t.Errorf("findSessionNameByTemplate(myrig/worker-1) = %q, want s-legacy-worker-1", got)
	}
}

func TestLookupPoolSessionNames_RejectsSharedPrefixSiblingTemplates(t *testing.T) {
	store := beads.NewMemStore()
	for _, bead := range []beads.Bead{
		{
			Title:  "worker",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "agent:frontend/worker-1"},
			Metadata: map[string]string{
				"template":     "worker",
				"pool_slot":    "1",
				"session_name": "s-worker-1",
			},
		},
		{
			Title:  "worker-supervisor",
			Type:   sessionBeadType,
			Labels: []string{sessionBeadLabel, "agent:frontend/worker-supervisor-1"},
			Metadata: map[string]string{
				"template":     "worker-supervisor",
				"pool_slot":    "1",
				"session_name": "s-worker-supervisor-1",
			},
		},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatal(err)
		}
	}

	got, err := lookupPoolSessionNames(store, "frontend/worker")
	if err != nil {
		t.Fatalf("lookupPoolSessionNames: %v", err)
	}
	if got["frontend/worker-1"] != "s-worker-1" {
		t.Fatalf("lookupPoolSessionNames(frontend/worker) missing worker-1: %#v", got)
	}
	if _, ok := got["frontend/worker-supervisor-1"]; ok {
		t.Fatalf("lookupPoolSessionNames(frontend/worker) wrongly matched sibling template: %#v", got)
	}
}

func TestDiscoverSessionBeads_RigQualifiedTemplate(t *testing.T) {
	store := beads.NewMemStore()

	// Create a bead with a rig-qualified template (as cmdSessionNew now stores).
	_, err := store.Create(beads.Bead{
		Title:  "worker",
		Type:   "session",
		Labels: []string{sessionBeadLabel, "template:myrig/worker"},
		Metadata: map[string]string{
			"template":     "myrig/worker",
			"session_name": "s-gc-300",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", StartCommand: "echo", MaxActiveSessions: intPtr(1)},
		},
	}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	desired := make(map[string]TemplateParams)
	discoverSessionBeads(bp, cfg, desired, io.Discard)

	if _, ok := desired["s-gc-300"]; !ok {
		t.Errorf("expected rig-qualified bead session s-gc-300 in desired state, got keys: %v", mapKeys(desired))
	}
}

func TestDiscoverSessionBeads_ForkGetsOwnSessionNameInEnv(t *testing.T) {
	store := beads.NewMemStore()

	// Create the primary (managed) session bead — has agent_name, as if
	// syncSessionBeads created it.
	_, err := store.Create(beads.Bead{
		Title:  "overseer",
		Type:   "session",
		Labels: []string{sessionBeadLabel, "agent:overseer"},
		Metadata: map[string]string{
			"template":     "overseer",
			"agent_name":   "overseer",
			"session_name": "s-primary",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create a fork bead — no agent_name, as if "gc session new" created it.
	_, err = store.Create(beads.Bead{
		Title:  "overseer fork",
		Type:   "session",
		Labels: []string{sessionBeadLabel, "template:overseer"},
		Metadata: map[string]string{
			"template":     "overseer",
			"session_name": "s-fork-1",
			"state":        "creating",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "overseer", StartCommand: "echo", MaxActiveSessions: intPtr(1)},
		},
	}
	sp := runtime.NewFake()
	bp := newAgentBuildParams("test", t.TempDir(), cfg, sp, time.Now(), store, io.Discard)

	// Phase 1: the primary should be selected by resolveSessionName.
	desired := make(map[string]TemplateParams)
	// Simulate Phase 1 by adding the primary to desired.
	desired["s-primary"] = TemplateParams{
		SessionName: "s-primary",
		Env:         map[string]string{"GC_SESSION_NAME": "s-primary"},
	}

	// Phase 2: discover the fork.
	discoverSessionBeads(bp, cfg, desired, io.Discard)

	// Fork must be in desired state.
	forkTP, ok := desired["s-fork-1"]
	if !ok {
		t.Fatalf("expected fork s-fork-1 in desired state, got keys: %v", mapKeys(desired))
	}

	// GC_SESSION_NAME must be the fork's own session name, not the primary's.
	if got := forkTP.Env["GC_SESSION_NAME"]; got != "s-fork-1" {
		t.Errorf("fork GC_SESSION_NAME = %q, want %q", got, "s-fork-1")
	}
}

func mapKeys(m map[string]TemplateParams) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}

// --- gc init (doInit with fsys.Fake) ---

func TestDoInitSuccess(t *testing.T) {
	f := fsys.NewFake()
	// No pre-existing files — doInit creates everything from scratch.

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Welcome to Gas City!") {
		t.Errorf("stdout missing 'Welcome to Gas City!': %q", out)
	}
	if !strings.Contains(out, "Initialized city") {
		t.Errorf("stdout missing 'Initialized city': %q", out)
	}
	if !strings.Contains(out, "bright-lights") {
		t.Errorf("stdout missing city name: %q", out)
	}

	// Verify .gc/ and the new city-root conventions were created (no rigs/ — created on demand by gc rig add).
	if !f.Dirs[filepath.Join("/bright-lights", ".gc")] {
		t.Error(".gc/ not created")
	}
	if f.Dirs[filepath.Join("/bright-lights", "rigs")] {
		t.Error("rigs/ should not be created by init")
	}
	for _, dir := range []string{
		"agents",
		"commands",
		"doctor",
		"formulas",
		"orders",
		"template-fragments",
		"overlays",
		"assets",
	} {
		if !f.Dirs[filepath.Join("/bright-lights", dir)] {
			t.Errorf("%s/ not created", dir)
		}
	}
	for _, dir := range []string{"packs", "prompts"} {
		if f.Dirs[filepath.Join("/bright-lights", dir)] {
			t.Errorf("%s/ should not be created by init", dir)
		}
	}

	// Verify only the explicit init agent prompt template was written.
	if _, ok := f.Files[filepath.Join("/bright-lights", "agents", "mayor", "prompt.template.md")]; !ok {
		t.Error("agents/mayor/prompt.template.md not written")
	}
	if _, ok := f.Files[filepath.Join("/bright-lights", "agents", "worker", "prompt.template.md")]; ok {
		t.Error("agents/worker/prompt.template.md should not be written by default init")
	}

	// Verify pack.toml was written.
	packToml := string(f.Files[filepath.Join("/bright-lights", "pack.toml")])
	if !strings.Contains(packToml, `name = "bright-lights"`) {
		t.Errorf("pack.toml missing pack name:\n%s", packToml)
	}
	if !strings.Contains(packToml, "schema = 2") {
		t.Errorf("pack.toml missing schema 2:\n%s", packToml)
	}

	// Verify written config parses correctly.
	data := f.Files[filepath.Join("/bright-lights", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "bright-lights")
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", cfg.Agents[0].Name, "mayor")
	}
	if cfg.Agents[0].PromptTemplate != "agents/mayor/prompt.template.md" {
		t.Errorf("Agents[0].PromptTemplate = %q, want %q", cfg.Agents[0].PromptTemplate, "agents/mayor/prompt.template.md")
	}
}

func TestDoInitWritesExpectedTOML(t *testing.T) {
	f := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	got := string(f.Files[filepath.Join("/bright-lights", "city.toml")])
	want := `[workspace]
name = "bright-lights"

[[agent]]
name = "mayor"
prompt_template = "agents/mayor/prompt.template.md"

[[named_session]]
template = "mayor"
mode = "always"
`
	if got != want {
		t.Errorf("city.toml content:\ngot:\n%s\nwant:\n%s", got, want)
	}

	packGot := string(f.Files[filepath.Join("/bright-lights", "pack.toml")])
	packWant := `[pack]
name = "bright-lights"
schema = 2
`
	if packGot != packWant {
		t.Errorf("pack.toml content:\ngot:\n%s\nwant:\n%s", packGot, packWant)
	}
}

func TestDoInitGastownWritesCanonicalPackV2Shape(t *testing.T) {
	f := fsys.NewFake()

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", wizardConfig{configName: "gastown", provider: "claude"}, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	cityToml := string(f.Files[filepath.Join("/bright-lights", "city.toml")])
	if strings.Contains(cityToml, "default_rig_includes") {
		t.Fatalf("city.toml should not keep legacy default_rig_includes in fresh init:\n%s", cityToml)
	}

	packToml := string(f.Files[filepath.Join("/bright-lights", "pack.toml")])
	if !strings.Contains(packToml, "[imports.gastown]") || !strings.Contains(packToml, `source = ".gc/system/packs/gastown"`) {
		t.Fatalf("pack.toml missing gastown import:\n%s", packToml)
	}
	if !strings.Contains(packToml, "[defaults.rig.imports.gastown]") {
		t.Fatalf("pack.toml missing canonical default-rig import:\n%s", packToml)
	}
}

func TestDoInitAlreadyInitialized(t *testing.T) {
	f := fsys.NewFake()
	markFakeCityScaffold(f, "/city")

	var stderr bytes.Buffer
	code := doInit(f, "/city", defaultWizardConfig(), "", &bytes.Buffer{}, &stderr)
	if code != initExitAlreadyInitialized {
		t.Errorf("doInit = %d, want %d", code, initExitAlreadyInitialized)
	}
	if !strings.Contains(stderr.String(), "already initialized") {
		t.Errorf("stderr = %q, want 'already initialized'", stderr.String())
	}
}

func TestCityAlreadyInitializedFSIgnoresSupervisorHomeState(t *testing.T) {
	f := fsys.NewFake()
	f.Dirs[filepath.Join("/home", citylayout.RuntimeRoot)] = true
	f.Files[filepath.Join("/home", citylayout.RuntimeRoot, "events.jsonl")] = nil
	f.Files[filepath.Join("/home", citylayout.RuntimeRoot, "cities.toml")] = []byte("[[city]]\n")

	if cityAlreadyInitializedFS(f, "/home") {
		t.Fatal("cityAlreadyInitializedFS should ignore global supervisor state without a city scaffold")
	}
}

func TestDoInitBootstrapsExistingCityToml(t *testing.T) {
	f := fsys.NewFake()
	original := []byte("[workspace]\nname = \"city\"\n")
	f.Files[filepath.Join("/city", "city.toml")] = original

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/city", defaultWizardConfig(), "", &stdout, &stderr)
	if code != 0 {
		t.Errorf("doInit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Bootstrapped city") {
		t.Errorf("stdout = %q, want bootstrap message", stdout.String())
	}
	if got := string(f.Files[filepath.Join("/city", "city.toml")]); got != string(original) {
		t.Errorf("city.toml overwritten:\ngot:\n%s\nwant:\n%s", got, original)
	}
	if !f.Dirs[filepath.Join("/city", ".gc")] {
		t.Error(".gc/ should be created during bootstrap")
	}
	if _, ok := f.Files[filepath.Join("/city", ".gc", "settings.json")]; !ok {
		t.Error(".gc/settings.json should be created during bootstrap")
	}
	if _, ok := f.Files[filepath.Join("/city", "hooks", "claude.json")]; ok {
		t.Error("hooks/claude.json should not be created during bootstrap")
	}
}

func TestDoInitBootstrapWithNameOverride(t *testing.T) {
	f := fsys.NewFake()
	f.Files[filepath.Join("/city", "city.toml")] = []byte("[workspace]\nname = \"old-name\"\n")

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/city", defaultWizardConfig(), "new-name", &stdout, &stderr)
	if code != 0 {
		t.Errorf("doInit = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "new-name") {
		t.Errorf("stdout = %q, want name override in output", stdout.String())
	}
	data := f.Files[filepath.Join("/city", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing updated city.toml: %v", err)
	}
	if cfg.Workspace.Name != "new-name" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "new-name")
	}
}

func TestDoInitMkdirGCFails(t *testing.T) {
	f := fsys.NewFake()
	f.Errors[filepath.Join("/city", ".gc")] = fmt.Errorf("permission denied")

	var stderr bytes.Buffer
	code := doInit(f, "/city", defaultWizardConfig(), "", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doInit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "permission denied") {
		t.Errorf("stderr = %q, want 'permission denied'", stderr.String())
	}
}

func TestDoInitWriteFails(t *testing.T) {
	f := fsys.NewFake()
	f.Errors[filepath.Join("/city", "city.toml")] = fmt.Errorf("read-only fs")

	var stderr bytes.Buffer
	code := doInit(f, "/city", defaultWizardConfig(), "", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doInit = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "read-only fs") {
		t.Errorf("stderr = %q, want 'read-only fs'", stderr.String())
	}
}

// --- settings.json ---

func TestDoInitCreatesSettings(t *testing.T) {
	f := fsys.NewFake()
	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	settingsPath := filepath.Join("/bright-lights", ".gc", "settings.json")
	data, ok := f.Files[settingsPath]
	if !ok {
		t.Fatal(".gc/settings.json not created")
	}
	if len(data) == 0 {
		t.Fatal(".gc/settings.json is empty")
	}
	if _, ok := f.Files[filepath.Join("/bright-lights", "hooks", "claude.json")]; ok {
		t.Fatal("hooks/claude.json should not be created on fresh install")
	}
}

func TestDoInitSettingsIsValidJSON(t *testing.T) {
	f := fsys.NewFake()
	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", defaultWizardConfig(), "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	settingsPath := filepath.Join("/bright-lights", ".gc", "settings.json")
	data := f.Files[settingsPath]

	var parsed map[string]any
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("settings.json is not valid JSON: %v", err)
	}

	// Verify hooks structure exists.
	hooks, ok := parsed["hooks"]
	if !ok {
		t.Fatal("settings.json missing 'hooks' key")
	}
	hookMap, ok := hooks.(map[string]any)
	if !ok {
		t.Fatal("settings.json 'hooks' is not an object")
	}
	for _, event := range []string{"SessionStart", "PreCompact", "UserPromptSubmit", "Stop"} {
		if _, ok := hookMap[event]; !ok {
			t.Errorf("settings.json missing hook event %q", event)
		}
	}
}

func TestDoInitDoesNotOverwriteExistingSettings(t *testing.T) {
	f := fsys.NewFake()
	// Pre-populate .gc/ and settings.json with custom content.
	// doInit will see .gc/ exists and return "already initialized".
	// So test installClaudeHooks directly instead.
	settingsPath := filepath.Join("/city", "hooks", "claude.json")
	f.Dirs[filepath.Join("/city", "hooks")] = true
	f.Files[settingsPath] = []byte(`{"custom": true}`)

	code := installClaudeHooks(f, "/city", &bytes.Buffer{})
	if code != 0 {
		t.Fatalf("installClaudeHooks = %d, want 0", code)
	}
	got := string(f.Files[settingsPath])
	if !strings.Contains(got, `"custom": true`) {
		t.Errorf("custom hook key was lost: %q", got)
	}
	if !strings.Contains(got, "SessionStart") {
		t.Errorf("default hooks were not merged into hook file: %q", got)
	}
	if runtime := string(f.Files[filepath.Join("/city", ".gc", "settings.json")]); runtime != got {
		t.Errorf("runtime settings were not mirrored from existing hooks file: %q", runtime)
	}
}

// --- settings flag injection ---

func TestSettingsArgsClaude(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(runtimeDir, "settings.json")
	if err := os.WriteFile(settingsPath, []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := settingsArgs(dir, "claude")
	// Must be absolute so K8s command remapping converts cityPath → /workspace.
	// A relative path breaks agents whose workingDir differs from the city root.
	// Path is quoted to handle spaces in city paths.
	want := fmt.Sprintf("--settings %q", filepath.Join(dir, ".gc", "settings.json"))
	if got != want {
		t.Errorf("settingsArgs(claude) = %q, want %q", got, want)
	}
}

// TestSettingsArgsRemapping verifies that the absolute path produced by
// settingsArgs survives K8s command remapping (strings.ReplaceAll of cityPath
// with /workspace) and resolves to the correct container path.
func TestSettingsArgsRemapping(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	sa := settingsArgs(dir, "claude")
	command := "claude " + sa

	// Simulate K8s pod.go remapping: replace cityPath with /workspace.
	remapped := strings.ReplaceAll(command, dir, "/workspace")
	want := fmt.Sprintf("claude --settings %q", "/workspace/.gc/settings.json")
	if remapped != want {
		t.Errorf("remapped command = %q, want %q", remapped, want)
	}
}

func TestSettingsArgsNonClaude(t *testing.T) {
	dir := t.TempDir()
	runtimeDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runtimeDir, "settings.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, provider := range []string{"codex", "gemini", "cursor", "copilot", "amp", "opencode"} {
		got := settingsArgs(dir, provider)
		if got != "" {
			t.Errorf("settingsArgs(%q) = %q, want empty", provider, got)
		}
	}
}

func TestSettingsArgsHookWithoutRuntimeFile(t *testing.T) {
	dir := t.TempDir()
	hooksDir := filepath.Join(dir, "hooks")
	if err := os.MkdirAll(hooksDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooksDir, "claude.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got := settingsArgs(dir, "claude")
	want := fmt.Sprintf("--settings %q", filepath.Join(dir, "hooks", "claude.json"))
	if got != want {
		t.Errorf("settingsArgs(claude, hook only) = %q, want %q", got, want)
	}
}

func TestSettingsArgsMissingFile(t *testing.T) {
	dir := t.TempDir()
	got := settingsArgs(dir, "claude")
	if got != "" {
		t.Errorf("settingsArgs(claude, no file) = %q, want empty", got)
	}
}

// --- runWizard ---

func TestRunWizardDefaults(t *testing.T) {
	// Two enters → default template (tutorial) + default agent (claude).
	stdin := strings.NewReader("\n\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if !wiz.interactive {
		t.Error("expected interactive = true")
	}
	if wiz.configName != "tutorial" {
		t.Errorf("configName = %q, want %q", wiz.configName, "tutorial")
	}
	if wiz.provider != "claude" {
		t.Errorf("provider = %q, want %q", wiz.provider, "claude")
	}
	// Verify both prompts were printed.
	out := stdout.String()
	if !strings.Contains(out, "Welcome to Gas City SDK!") {
		t.Errorf("stdout missing welcome message: %q", out)
	}
	if !strings.Contains(out, "Choose a config template:") {
		t.Errorf("stdout missing template prompt: %q", out)
	}
	if !strings.Contains(out, "Choose your coding agent:") {
		t.Errorf("stdout missing agent prompt: %q", out)
	}
}

func TestRunWizardNilStdin(t *testing.T) {
	var stdout bytes.Buffer
	wiz := runWizard(nil, &stdout)

	if wiz.interactive {
		t.Error("expected interactive = false for nil stdin")
	}
	if wiz.configName != "tutorial" {
		t.Errorf("configName = %q, want %q", wiz.configName, "tutorial")
	}
	if wiz.provider != "" {
		t.Errorf("provider = %q, want empty", wiz.provider)
	}
	// No prompts should be printed.
	if stdout.Len() > 0 {
		t.Errorf("unexpected stdout for nil stdin: %q", stdout.String())
	}
}

func TestRunWizardSelectGemini(t *testing.T) {
	// Default template + Gemini CLI.
	stdin := strings.NewReader("\nGemini CLI\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "gemini" {
		t.Errorf("provider = %q, want %q", wiz.provider, "gemini")
	}
}

func TestRunWizardSelectCodex(t *testing.T) {
	// Default template + Codex by number.
	stdin := strings.NewReader("\n2\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "codex" {
		t.Errorf("provider = %q, want %q", wiz.provider, "codex")
	}
}

func TestRunWizardCustomTemplate(t *testing.T) {
	// Select custom template → skips agent question, returns minimal config.
	stdin := strings.NewReader("3\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.configName != "custom" {
		t.Errorf("configName = %q, want %q", wiz.configName, "custom")
	}
	if wiz.provider != "" {
		t.Errorf("provider = %q, want empty for custom", wiz.provider)
	}
	if wiz.startCommand != "" {
		t.Errorf("startCommand = %q, want empty for custom", wiz.startCommand)
	}
	// Agent prompt should NOT appear.
	out := stdout.String()
	if strings.Contains(out, "Choose your coding agent:") {
		t.Errorf("stdout should not contain agent prompt for custom template: %q", out)
	}
}

func TestRunWizardGastownTemplate(t *testing.T) {
	// Select gastown template + default agent.
	stdin := strings.NewReader("2\n\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.configName != "gastown" {
		t.Errorf("configName = %q, want %q", wiz.configName, "gastown")
	}
	if wiz.provider == "" {
		t.Error("provider should be set to default for gastown")
	}
}

func TestRunWizardGastownByName(t *testing.T) {
	stdin := strings.NewReader("gastown\n\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.configName != "gastown" {
		t.Errorf("configName = %q, want %q", wiz.configName, "gastown")
	}
}

func TestRunWizardSelectCursorByNumber(t *testing.T) {
	// Cursor is #4 in the order.
	stdin := strings.NewReader("\n4\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "cursor" {
		t.Errorf("provider = %q, want %q", wiz.provider, "cursor")
	}
}

func TestRunWizardSelectCopilotByName(t *testing.T) {
	stdin := strings.NewReader("\nGitHub Copilot\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "copilot" {
		t.Errorf("provider = %q, want %q", wiz.provider, "copilot")
	}
}

func TestRunWizardSelectByProviderKey(t *testing.T) {
	stdin := strings.NewReader("\namp\n")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "amp" {
		t.Errorf("provider = %q, want %q", wiz.provider, "amp")
	}
}

func TestRunWizardCustomCommand(t *testing.T) {
	// Default template + custom command (last option = len(providers)+1).
	customNum := len(config.BuiltinProviderOrder()) + 1
	stdin := strings.NewReader(fmt.Sprintf("\n%d\nmy-agent --auto --skip-confirm\n", customNum))
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	if wiz.provider != "" {
		t.Errorf("provider = %q, want empty for custom command", wiz.provider)
	}
	if wiz.startCommand != "my-agent --auto --skip-confirm" {
		t.Errorf("startCommand = %q, want %q", wiz.startCommand, "my-agent --auto --skip-confirm")
	}
}

func TestRunWizardEOFStdin(t *testing.T) {
	stdin := strings.NewReader("")
	var stdout bytes.Buffer
	wiz := runWizard(stdin, &stdout)

	// EOF means default for both questions.
	if wiz.configName != "tutorial" {
		t.Errorf("configName = %q, want %q", wiz.configName, "tutorial")
	}
	if wiz.provider != "claude" {
		t.Errorf("provider = %q, want %q", wiz.provider, "claude")
	}
}

func TestDoInitWithWizardConfig(t *testing.T) {
	f := fsys.NewFake()
	wiz := wizardConfig{
		interactive: true,
		configName:  "tutorial",
		provider:    "claude",
	}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", wiz, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	// Verify output message.
	out := stdout.String()
	if !strings.Contains(out, "Created tutorial config") {
		t.Errorf("stdout missing wizard message: %q", out)
	}

	// Verify written config has one agent and provider.
	data := f.Files[filepath.Join("/bright-lights", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Provider != "claude" {
		t.Errorf("Workspace.Provider = %q, want %q", cfg.Workspace.Provider, "claude")
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", cfg.Agents[0].Name, "mayor")
	}
	if cfg.Agents[0].PromptTemplate != "agents/mayor/prompt.template.md" {
		t.Errorf("Agents[0].PromptTemplate = %q, want %q", cfg.Agents[0].PromptTemplate, "agents/mayor/prompt.template.md")
	}
	// Verify provider appears in TOML.
	if !strings.Contains(string(data), `provider = "claude"`) {
		t.Errorf("city.toml missing provider:\n%s", data)
	}
}

func TestDoInitWithCustomCommand(t *testing.T) {
	f := fsys.NewFake()
	wiz := wizardConfig{
		interactive:  true,
		configName:   "tutorial",
		startCommand: "my-agent --auto",
	}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", wiz, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Verify written config has start_command and no provider.
	data := f.Files[filepath.Join("/bright-lights", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.StartCommand != "my-agent --auto" {
		t.Errorf("Workspace.StartCommand = %q, want %q", cfg.Workspace.StartCommand, "my-agent --auto")
	}
	if cfg.Workspace.Provider != "" {
		t.Errorf("Workspace.Provider = %q, want empty", cfg.Workspace.Provider)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
}

func TestDoInitWithGastownTemplate(t *testing.T) {
	f := fsys.NewFake()
	wiz := wizardConfig{
		interactive: true,
		configName:  "gastown",
		provider:    "claude",
	}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/bright-lights", wiz, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Verify output message.
	out := stdout.String()
	if !strings.Contains(out, "Created gastown config") {
		t.Errorf("stdout missing gastown message: %q", out)
	}

	// Verify written config has gastown shape.
	data := f.Files[filepath.Join("/bright-lights", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Provider != "claude" {
		t.Errorf("Workspace.Provider = %q, want %q", cfg.Workspace.Provider, "claude")
	}
	if len(cfg.Workspace.Includes) != 0 {
		t.Errorf("Workspace.Includes = %v, want empty", cfg.Workspace.Includes)
	}
	if len(cfg.Workspace.DefaultRigIncludes) != 0 {
		t.Errorf("Workspace.DefaultRigIncludes = %v, want empty", cfg.Workspace.DefaultRigIncludes)
	}
	// No inline agents.
	if len(cfg.Agents) != 0 {
		t.Errorf("len(Agents) = %d, want 0 (agents come from pack)", len(cfg.Agents))
	}
	packToml := string(f.Files[filepath.Join("/bright-lights", "pack.toml")])
	if !strings.Contains(packToml, "[imports.gastown]") || !strings.Contains(packToml, "[defaults.rig.imports.gastown]") {
		t.Errorf("pack.toml missing gastown pack wiring:\n%s", packToml)
	}
	// Daemon config.
	if cfg.Daemon.PatrolInterval != "30s" {
		t.Errorf("Daemon.PatrolInterval = %q, want %q", cfg.Daemon.PatrolInterval, "30s")
	}
}

func TestDoInitWithCustomTemplate(t *testing.T) {
	f := fsys.NewFake()
	wiz := wizardConfig{
		interactive: true,
		configName:  "custom",
	}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/my-city", wiz, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Custom template → DefaultCity (one mayor, no provider).
	data := f.Files[filepath.Join("/my-city", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("len(Agents) = %d, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("Agents[0].Name = %q, want %q", cfg.Agents[0].Name, "mayor")
	}
	if cfg.Workspace.Provider != "" {
		t.Errorf("Workspace.Provider = %q, want empty", cfg.Workspace.Provider)
	}
}

func TestDoInitWithProviderFlagAndBootstrapProfile(t *testing.T) {
	f := fsys.NewFake()
	wiz := wizardConfig{
		configName:       "tutorial",
		provider:         "codex",
		bootstrapProfile: bootstrapProfileK8sCell,
	}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/hosted-city", wiz, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, `default provider "codex"`) {
		t.Errorf("stdout missing provider message: %q", out)
	}

	data := f.Files[filepath.Join("/hosted-city", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Provider != "codex" {
		t.Errorf("Workspace.Provider = %q, want %q", cfg.Workspace.Provider, "codex")
	}
	if cfg.API.Bind != "0.0.0.0" {
		t.Errorf("API.Bind = %q, want %q", cfg.API.Bind, "0.0.0.0")
	}
	if cfg.API.Port != config.DefaultAPIPort {
		t.Errorf("API.Port = %d, want %d", cfg.API.Port, config.DefaultAPIPort)
	}
	if !cfg.API.AllowMutations {
		t.Error("API.AllowMutations = false, want true")
	}
}

func TestDoInitWithOpenCodeProviderInstallsWorkspaceHooks(t *testing.T) {
	f := fsys.NewFake()
	wiz := wizardConfig{
		configName: "tutorial",
		provider:   "opencode",
	}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/open-city", wiz, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	data := f.Files[filepath.Join("/open-city", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Provider != "opencode" {
		t.Errorf("Workspace.Provider = %q, want %q", cfg.Workspace.Provider, "opencode")
	}
	if len(cfg.Workspace.InstallAgentHooks) != 1 || cfg.Workspace.InstallAgentHooks[0] != "opencode" {
		t.Errorf("Workspace.InstallAgentHooks = %v, want [opencode]", cfg.Workspace.InstallAgentHooks)
	}
	if !strings.Contains(string(data), "install_agent_hooks") {
		t.Errorf("city.toml missing install_agent_hooks:\n%s", data)
	}
}

func TestDoInitWithClaudeProviderLeavesWorkspaceHooksEmpty(t *testing.T) {
	f := fsys.NewFake()
	wiz := wizardConfig{
		configName: "tutorial",
		provider:   "claude",
	}

	var stdout, stderr bytes.Buffer
	code := doInit(f, "/claude-city", wiz, "", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	data := f.Files[filepath.Join("/claude-city", "city.toml")]
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if len(cfg.Workspace.InstallAgentHooks) != 0 {
		t.Errorf("Workspace.InstallAgentHooks = %v, want empty", cfg.Workspace.InstallAgentHooks)
	}
	if strings.Contains(string(data), "install_agent_hooks") {
		t.Errorf("city.toml unexpectedly contains install_agent_hooks:\n%s", data)
	}
}

func TestInitWizardConfigRejectsUnknownProvider(t *testing.T) {
	if _, err := initWizardConfig("not-a-provider", ""); err == nil {
		t.Fatal("expected error for unknown provider")
	}
}

func TestInitWizardConfigNormalizesBootstrapAliases(t *testing.T) {
	wiz, err := initWizardConfig("codex", "kubernetes")
	if err != nil {
		t.Fatalf("initWizardConfig returned error: %v", err)
	}
	if wiz.bootstrapProfile != bootstrapProfileK8sCell {
		t.Errorf("bootstrapProfile = %q, want %q", wiz.bootstrapProfile, bootstrapProfileK8sCell)
	}
}

// --- cmdInitFromTOMLFile ---

func TestCmdInitFromTOMLFileSuccess(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	// Use real temp dirs since cmdInitFromTOMLFile calls initBeads which
	// uses real filesystem via beadsProvider.
	dir := t.TempDir()
	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	src := filepath.Join(dir, "my-config.toml")
	tomlContent := []byte(`[workspace]
name = "placeholder"
provider = "claude"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[agent]]
name = "worker"
min_active_sessions = 0
max_active_sessions = 5
scale_check = "echo 3"
`)
	if err := os.WriteFile(src, tomlContent, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := cmdInitFromTOMLFile(fsys.OSFS{}, src, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdInitFromTOMLFile = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Welcome to Gas City!") {
		t.Errorf("stdout missing welcome: %q", out)
	}
	if !strings.Contains(out, "bright-lights") {
		t.Errorf("stdout missing city name: %q", out)
	}
	if !strings.Contains(out, "my-config.toml") {
		t.Errorf("stdout missing source filename: %q", out)
	}

	// Verify city.toml was written with updated name.
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Errorf("Workspace.Name = %q, want %q (should be overridden)", cfg.Workspace.Name, "bright-lights")
	}
	if cfg.Workspace.Provider != "claude" {
		t.Errorf("Workspace.Provider = %q, want %q", cfg.Workspace.Provider, "claude")
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("len(Agents) = %d, want 2", len(cfg.Agents))
	}
	if cfg.Agents[1].Name != "worker" {
		t.Errorf("Agents[1].Name = %q, want %q", cfg.Agents[1].Name, "worker")
	}
	if cfg.Agents[1].MaxActiveSessions == nil {
		t.Fatal("Agents[1].MaxActiveSessions is nil, want non-nil")
	}
	if *cfg.Agents[1].MaxActiveSessions != 5 {
		t.Errorf("Agents[1].MaxActiveSessions = %d, want 5", *cfg.Agents[1].MaxActiveSessions)
	}
	if cfg.Agents[0].PromptTemplate != "agents/mayor/prompt.template.md" {
		t.Errorf("Agents[0].PromptTemplate = %q, want %q", cfg.Agents[0].PromptTemplate, "agents/mayor/prompt.template.md")
	}

	packData, err := os.ReadFile(filepath.Join(cityPath, "pack.toml"))
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}
	if !strings.Contains(string(packData), `name = "bright-lights"`) {
		t.Errorf("pack.toml missing pack name:\n%s", packData)
	}
	if _, err := os.Stat(filepath.Join(cityPath, "agents", "mayor", "prompt.template.md")); err != nil {
		t.Errorf("agents/mayor/prompt.template.md missing: %v", err)
	}
	for _, dir := range []string{"packs", "prompts"} {
		if _, err := os.Stat(filepath.Join(cityPath, dir)); !os.IsNotExist(err) {
			t.Errorf("%s/ should not be created by init: %v", dir, err)
		}
	}
}

func TestCmdInitFromTOMLFileNotFound(t *testing.T) {
	f := fsys.NewFake()
	var stderr bytes.Buffer
	code := cmdInitFromTOMLFile(f, "/nonexistent.toml", "/city", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "reading") {
		t.Errorf("stderr = %q, want reading error", stderr.String())
	}
}

func TestCmdInitFromTOMLFileInvalidTOML(t *testing.T) {
	f := fsys.NewFake()
	dir := t.TempDir()
	src := filepath.Join(dir, "bad.toml")
	if err := os.WriteFile(src, []byte("[[[invalid"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := cmdInitFromTOMLFile(f, src, "/city", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "parsing") {
		t.Errorf("stderr = %q, want parsing error", stderr.String())
	}
}

func TestCmdInitFromTOMLFileAlreadyInitialized(t *testing.T) {
	f := fsys.NewFake()
	markFakeCityScaffold(f, "/city")

	dir := t.TempDir()
	src := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(src, []byte("[workspace]\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := cmdInitFromTOMLFile(f, src, "/city", &bytes.Buffer{}, &stderr)
	if code != initExitAlreadyInitialized {
		t.Errorf("code = %d, want %d", code, initExitAlreadyInitialized)
	}
	if !strings.Contains(stderr.String(), "already initialized") {
		t.Errorf("stderr = %q, want 'already initialized'", stderr.String())
	}
}

func TestCmdInitFromTOMLFileAlreadyInitializedByCityToml(t *testing.T) {
	f := fsys.NewFake()
	f.Files[filepath.Join("/city", "city.toml")] = []byte("[workspace]\nname = \"city\"\n")

	dir := t.TempDir()
	src := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(src, []byte("[workspace]\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := cmdInitFromTOMLFile(f, src, "/city", &bytes.Buffer{}, &stderr)
	if code != initExitAlreadyInitialized {
		t.Errorf("code = %d, want %d", code, initExitAlreadyInitialized)
	}
	if !strings.Contains(stderr.String(), "already initialized") {
		t.Errorf("stderr = %q, want 'already initialized'", stderr.String())
	}
}

func TestRunInitFromFileAlreadyInitializedPropagatesExitCode(t *testing.T) {
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	src := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(src, []byte("[workspace]\nname = \"x\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "city")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "events.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := run([]string{"init", "--file", src, cityPath}, &stdout, &stderr)
	if code != initExitAlreadyInitialized {
		t.Fatalf("run(init --file ...) = %d, want %d; stderr=%s", code, initExitAlreadyInitialized, stderr.String())
	}
	if !strings.Contains(stderr.String(), "already initialized") {
		t.Fatalf("stderr = %q, want already initialized message", stderr.String())
	}
}

// --- gc init --from tests ---

func TestDoInitFromDirSuccess(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()

	// Create a minimal source city.
	srcDir := filepath.Join(dir, "my-template")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"template\"\nprovider = \"claude\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "prompts", "mayor.md"), []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}

	out := stdout.String()
	if !strings.Contains(out, "Welcome to Gas City!") {
		t.Errorf("stdout missing welcome: %q", out)
	}
	if !strings.Contains(out, "bright-lights") {
		t.Errorf("stdout missing city name: %q", out)
	}

	// Verify city.toml was copied and name updated.
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if cfg.Workspace.Name != "bright-lights" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "bright-lights")
	}
	if cfg.Workspace.Provider != "claude" {
		t.Errorf("Workspace.Provider = %q, want %q", cfg.Workspace.Provider, "claude")
	}

	// Verify files were copied.
	if _, err := os.Stat(filepath.Join(cityPath, "prompts", "mayor.md")); err != nil {
		t.Errorf("prompts/mayor.md not copied: %v", err)
	}

	// Verify .gc/ was created.
	if _, err := os.Stat(filepath.Join(cityPath, ".gc")); err != nil {
		t.Errorf(".gc/ not created: %v", err)
	}
}

func TestResolveCityName(t *testing.T) {
	tests := []struct {
		name         string
		nameOverride string
		sourceName   string
		cityPath     string
		want         string
	}{
		{"override wins over dir", "custom", "", "/path/to/dir", "custom"},
		{"source name beats dir", "", "template-city", "/path/to/dir", "template-city"},
		{"dir basename used as fallback", "", "", "/path/to/dir", "dir"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveCityName(tt.nameOverride, tt.sourceName, tt.cityPath)
			if got != tt.want {
				t.Errorf("resolveCityName(%q, %q, %q) = %q, want %q",
					tt.nameOverride, tt.sourceName, tt.cityPath, got, tt.want)
			}
		})
	}
}

func TestInitNameFlagWithFrom(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()

	// Create source template directory.
	srcDir := filepath.Join(dir, "template")
	if err := os.MkdirAll(filepath.Join(srcDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"template\"\nprovider = \"claude\"\n\n[[agent]]\nname = \"mayor\"\nprompt_template = \"prompts/mayor.md\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "prompts", "mayor.md"), []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "target-dir")

	var stdout, stderr bytes.Buffer
	code := doInitFromDirWithOptions(srcDir, cityPath, "my-custom-name", &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("doInitFromDirWithOptions = %d, want 0; stderr: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	if cfg.Workspace.Name != "my-custom-name" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "my-custom-name")
	}
}

func TestInitNameFlagWithFile(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()

	tomlFile := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlFile,
		[]byte("[workspace]\nname = \"original\"\nprovider = \"claude\"\n\n[[agent]]\nname = \"mayor\"\nprompt_template = \"prompts/mayor.md\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "target-dir")

	var stdout, stderr bytes.Buffer
	code := cmdInitFromFileWithOptions(tomlFile, []string{cityPath}, "my-file-name", &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("cmdInitFromFileWithOptions = %d, want 0; stderr: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	if cfg.Workspace.Name != "my-file-name" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "my-file-name")
	}
}

func TestInitNameFlagWithBareInit(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "target-dir")

	var stdout, stderr bytes.Buffer
	code := doInit(fsys.OSFS{}, cityPath, wizardConfig{
		configName: "tutorial",
		provider:   "claude",
	}, "my-bare-name", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	if cfg.Workspace.Name != "my-bare-name" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "my-bare-name")
	}
	packData, err := os.ReadFile(filepath.Join(cityPath, "pack.toml"))
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}
	if !strings.Contains(string(packData), `name = "my-bare-name"`) {
		t.Errorf("pack.toml should keep init name aligned with workspace.name, got:\n%s", string(packData))
	}
}

func TestInitFromDefaultsToTargetDirBasename(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()

	// Source has workspace.name = "template" — should NOT propagate.
	srcDir := filepath.Join(dir, "template")
	if err := os.MkdirAll(filepath.Join(srcDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"template\"\nprovider = \"claude\"\n\n[[agent]]\nname = \"mayor\"\nprompt_template = \"prompts/mayor.md\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "prompts", "mayor.md"), []byte("prompt"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "my-new-city")

	var stdout, stderr bytes.Buffer
	code := doInitFromDirWithOptions(srcDir, cityPath, "", &stdout, &stderr, true)
	if code != 0 {
		t.Fatalf("doInitFromDirWithOptions = %d, want 0; stderr: %s", code, stderr.String())
	}

	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	cfg, err := config.Parse(data)
	if err != nil {
		t.Fatalf("parsing config: %v", err)
	}
	if cfg.Workspace.Name != "my-new-city" {
		t.Errorf("Workspace.Name = %q, want %q (--from should default to target dir basename)", cfg.Workspace.Name, "my-new-city")
	}
}

func TestDoInitFromDirSkipsGCDir(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()

	// Source with a .gc/ directory.
	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, ".gc", "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, ".gc", "state.json"), []byte("state"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"src\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "dst")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}

	// .gc/ should exist (created fresh by init), but should NOT contain
	// the source's state.json or agents/ subdir.
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "state.json")); !os.IsNotExist(err) {
		t.Error(".gc/state.json should not have been copied from source")
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "agents")); !os.IsNotExist(err) {
		t.Error(".gc/agents/ should not have been copied from source")
	}
}

func TestDoInitFromDirSkipsTestFiles(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()

	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"src\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "gastown_test.go"),
		[]byte("package test"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "helper.go"),
		[]byte("package helper"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "dst")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}

	// Test files should be skipped.
	if _, err := os.Stat(filepath.Join(cityPath, "gastown_test.go")); !os.IsNotExist(err) {
		t.Error("gastown_test.go should not have been copied")
	}
	// Non-test Go files should be copied.
	if _, err := os.Stat(filepath.Join(cityPath, "helper.go")); err != nil {
		t.Errorf("helper.go should have been copied: %v", err)
	}
}

func TestDoInitFromDirNoCityToml(t *testing.T) {
	srcDir := t.TempDir() // no city.toml

	var stderr bytes.Buffer
	code := doInitFromDir(srcDir, filepath.Join(t.TempDir(), "dst"), &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "no city.toml") {
		t.Errorf("stderr = %q, want 'no city.toml'", stderr.String())
	}
}

func TestDoInitFromDirAlreadyInitialized(t *testing.T) {
	dir := t.TempDir()

	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"src\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "dst")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "events.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &bytes.Buffer{}, &stderr)
	if code != initExitAlreadyInitialized {
		t.Errorf("code = %d, want %d", code, initExitAlreadyInitialized)
	}
	if !strings.Contains(stderr.String(), "already initialized") {
		t.Errorf("stderr = %q, want 'already initialized'", stderr.String())
	}
}

func TestDoInitFromDirAlreadyInitializedByCityToml(t *testing.T) {
	dir := t.TempDir()

	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(srcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"src\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "dst")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"dst\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &bytes.Buffer{}, &stderr)
	if code != initExitAlreadyInitialized {
		t.Errorf("code = %d, want %d", code, initExitAlreadyInitialized)
	}
	if !strings.Contains(stderr.String(), "already initialized") {
		t.Errorf("stderr = %q, want 'already initialized'", stderr.String())
	}
}

func TestDoInitFromDirPreservesPermissions(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	dir := t.TempDir()

	srcDir := filepath.Join(dir, "src")
	if err := os.MkdirAll(filepath.Join(srcDir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "city.toml"),
		[]byte("[workspace]\nname = \"src\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	scriptPath := filepath.Join(srcDir, "scripts", "run.sh")
	if err := os.WriteFile(scriptPath, []byte("#!/bin/sh\necho hello"), 0o755); err != nil {
		t.Fatal(err)
	}

	cityPath := filepath.Join(dir, "dst")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doInitFromDir(srcDir, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doInitFromDir = %d, want 0; stderr: %s", code, stderr.String())
	}

	info, err := os.Stat(filepath.Join(cityPath, "scripts", "run.sh"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Errorf("permissions = %o, want 755", info.Mode().Perm())
	}
}

func TestInitFromSkip(t *testing.T) {
	tests := []struct {
		relPath string
		isDir   bool
		want    bool
	}{
		{".gc", true, true},
		{".gc/state.json", false, true},
		{filepath.Join(".gc", "agents", "mayor.json"), false, true},
		{filepath.Join(".gc", "prompts"), true, true},
		{filepath.Join(".gc", "prompts", "mayor.md"), false, true},
		{"gastown_test.go", false, true},
		{filepath.Join("sub", "foo_test.go"), false, true},
		{"city.toml", false, false},
		{"scripts", true, false},
		{"helper.go", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.relPath, func(t *testing.T) {
			got := initFromSkip(tt.relPath, tt.isDir)
			if got != tt.want {
				t.Errorf("initFromSkip(%q, %v) = %v, want %v", tt.relPath, tt.isDir, got, tt.want)
			}
		})
	}
}

// --- gc stop (doStop with runtime.Fake) ---

func TestDoStopOneAgentRunning(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "mayor", runtime.Config{})

	var stdout, stderr bytes.Buffer
	code := doStop([]string{"mayor"}, sp, nil, nil, 0, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doStop = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Stopped agent 'mayor'") {
		t.Errorf("stdout missing stop message: %q", out)
	}
	if !strings.Contains(out, "City stopped.") {
		t.Errorf("stdout missing 'City stopped.': %q", out)
	}
}

func TestDoStopNoAgents(t *testing.T) {
	sp := runtime.NewFake()
	var stdout, stderr bytes.Buffer
	code := doStop(nil, sp, nil, nil, 0, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doStop = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "City stopped.") {
		t.Errorf("stdout missing 'City stopped.': %q", out)
	}
	// Should not contain any "Stopped agent" messages.
	if strings.Contains(out, "Stopped agent") {
		t.Errorf("stdout should not contain 'Stopped agent' with no agents: %q", out)
	}
}

func TestDoStopAgentNotRunning(t *testing.T) {
	sp := runtime.NewFake()
	// "mayor" not started in provider — IsRunning returns false.

	var stdout, stderr bytes.Buffer
	code := doStop([]string{"mayor"}, sp, nil, nil, 0, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doStop = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "City stopped.") {
		t.Errorf("stdout missing 'City stopped.': %q", out)
	}
	// Should not contain "Stopped agent" since session wasn't running.
	if strings.Contains(out, "Stopped agent") {
		t.Errorf("stdout should not contain 'Stopped agent' for non-running session: %q", out)
	}
}

func TestDoStopMultipleAgents(t *testing.T) {
	sp := runtime.NewFake()
	_ = sp.Start(context.Background(), "mayor", runtime.Config{})
	_ = sp.Start(context.Background(), "worker", runtime.Config{})

	var stdout, stderr bytes.Buffer
	code := doStop([]string{"mayor", "worker"}, sp, nil, nil, 0, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doStop = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "Stopped agent 'mayor'") {
		t.Errorf("stdout missing stop message for mayor: %q", out)
	}
	if !strings.Contains(out, "Stopped agent 'worker'") {
		t.Errorf("stdout missing stop message for worker: %q", out)
	}
	if !strings.Contains(out, "City stopped.") {
		t.Errorf("stdout missing 'City stopped.': %q", out)
	}
}

func TestDoStop_UsesDependencyAwareOrdering(t *testing.T) {
	sp := newGatedStopProvider()
	for _, name := range []string{"db", "api", "worker"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1), DependsOn: []string{"api"}},
			{Name: "api", MaxActiveSessions: intPtr(1), DependsOn: []string{"db"}},
			{Name: "db"},
		},
	}

	var stdout, stderr bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- doStop([]string{"db", "api", "worker"}, sp, cfg, nil, 0, events.Discard, &stdout, &stderr)
	}()

	firstWave := sp.waitForStops(t, 1)
	if !containsAll(firstWave, "worker") {
		t.Fatalf("first stop wave = %v, want worker", firstWave)
	}
	sp.ensureNoFurtherStop(t)
	sp.release("worker")

	secondWave := sp.waitForStops(t, 1)
	if !containsAll(secondWave, "api") {
		t.Fatalf("second stop wave = %v, want api", secondWave)
	}
	sp.release("api")

	thirdWave := sp.waitForStops(t, 1)
	if !containsAll(thirdWave, "db") {
		t.Fatalf("third stop wave = %v, want db", thirdWave)
	}
	sp.release("db")

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("doStop = %d, want 0; stderr: %s", code, stderr.String())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("doStop did not finish")
	}
}

func TestDoStopStopError(t *testing.T) {
	sp := runtime.NewFailFake() // Stop will fail

	var stdout, stderr bytes.Buffer
	code := doStop([]string{"mayor"}, sp, nil, nil, 0, events.Discard, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doStop = %d, want 0 (errors are non-fatal); stderr: %s", code, stderr.String())
	}
	// FailFake makes IsRunning return false, so no stop attempt.
	// Should still print "City stopped."
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Errorf("stdout missing 'City stopped.': %q", stdout.String())
	}
}

// --- doAgentAdd (with fsys.Fake) ---

func TestDoAgentAddSuccess(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data
	f.Files[filepath.Join("/city", "pack.toml")] = []byte(`[pack]
name = "bright-lights"
schema = 2
`)

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(f, "/city", "worker", "", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() > 0 {
		t.Errorf("unexpected stderr: %q", stderr.String())
	}
	if !strings.Contains(stdout.String(), "Scaffolded agent 'worker'") {
		t.Errorf("stdout = %q, want scaffold message", stdout.String())
	}

	// Verify the scaffolded agent directory is visible through config load.
	if _, ok := f.Files[filepath.Join("/city", "agents", "worker", "prompt.template.md")]; !ok {
		t.Fatal("agents/worker/prompt.template.md not written")
	}
	got, err := loadCityConfigFS(f, filepath.Join("/city", "city.toml"))
	if err != nil {
		t.Fatalf("loadCityConfigFS: %v", err)
	}
	explicit := explicitAgents(got.Agents)
	found := false
	for _, a := range explicit {
		if a.Name != "worker" {
			continue
		}
		found = true
		if !strings.HasSuffix(a.PromptTemplate, "agents/worker/prompt.template.md") {
			t.Errorf("Agents[worker].PromptTemplate = %q, want canonical agent scaffold path", a.PromptTemplate)
		}
	}
	if !found {
		t.Fatalf("explicit agents = %#v, want worker scaffold", explicit)
	}
}

func TestDoAgentAddDuplicate(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data
	f.Files[filepath.Join("/city", "pack.toml")] = []byte(`[pack]
name = "bright-lights"
schema = 2
`)

	var stdout, stderr bytes.Buffer
	if code := doAgentAdd(f, "/city", "dupe", "", "", false, &stdout, &stderr); code != 0 {
		t.Fatalf("first doAgentAdd = %d, want 0; stderr: %s", code, stderr.String())
	}
	stderr.Reset()
	stdout.Reset()
	if code := doAgentAdd(f, "/city", "dupe", "", "", false, &stdout, &stderr); code != 1 {
		t.Errorf("second doAgentAdd = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already exists") {
		t.Errorf("stderr = %q, want 'already exists'", stderr.String())
	}
}

func TestDoAgentAddLoadFails(t *testing.T) {
	f := fsys.NewFake()
	f.Files[filepath.Join("/city", "city.toml")] = []byte(`[workspace]
name = "test"
`)

	var stderr bytes.Buffer
	code := doAgentAdd(f, "/city", "worker", "", "", false, &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doAgentAdd = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "city directory with pack.toml") {
		t.Errorf("stderr = %q, want pack.toml city requirement", stderr.String())
	}
}

// --- doAgentAdd with --prompt-template ---

// --- mergeEnv ---

func TestMergeEnvNil(t *testing.T) {
	got := mergeEnv(nil, nil)
	if got != nil {
		t.Errorf("mergeEnv(nil, nil) = %v, want nil", got)
	}
}

func TestMergeEnvSingle(t *testing.T) {
	got := mergeEnv(map[string]string{"A": "1"})
	if got["A"] != "1" {
		t.Errorf("got[A] = %q, want %q", got["A"], "1")
	}
}

func TestMergeEnvOverride(t *testing.T) {
	got := mergeEnv(
		map[string]string{"A": "base", "B": "keep"},
		map[string]string{"A": "override", "C": "new"},
	)
	if got["A"] != "override" {
		t.Errorf("got[A] = %q, want %q (later map wins)", got["A"], "override")
	}
	if got["B"] != "keep" {
		t.Errorf("got[B] = %q, want %q", got["B"], "keep")
	}
	if got["C"] != "new" {
		t.Errorf("got[C] = %q, want %q", got["C"], "new")
	}
}

func TestMergeEnvProviderEnvFlowsThrough(t *testing.T) {
	// Simulate what cmd_start does: provider env + GC_AGENT.
	providerEnv := map[string]string{"OPENCODE_PERMISSION": `{"*":"allow"}`}
	got := mergeEnv(providerEnv, map[string]string{"GC_AGENT": "worker"})
	if got["OPENCODE_PERMISSION"] != `{"*":"allow"}` {
		t.Errorf("provider env lost: %v", got)
	}
	if got["GC_AGENT"] != "worker" {
		t.Errorf("GC_AGENT lost: %v", got)
	}
}

// --- resolveAgentChoice ---

func TestResolveAgentChoiceEmpty(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	got := resolveAgentChoice("", order, builtins, len(order)+1)
	if got != order[0] {
		t.Errorf("resolveAgentChoice('') = %q, want %q", got, order[0])
	}
}

func TestResolveAgentChoiceByNumber(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	got := resolveAgentChoice("2", order, builtins, len(order)+1)
	if got != order[1] {
		t.Errorf("resolveAgentChoice('2') = %q, want %q", got, order[1])
	}
}

func TestResolveAgentChoiceByDisplayName(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	got := resolveAgentChoice("Gemini CLI", order, builtins, len(order)+1)
	if got != "gemini" {
		t.Errorf("resolveAgentChoice('Gemini CLI') = %q, want %q", got, "gemini")
	}
}

func TestResolveAgentChoiceByKey(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	got := resolveAgentChoice("amp", order, builtins, len(order)+1)
	if got != "amp" {
		t.Errorf("resolveAgentChoice('amp') = %q, want %q", got, "amp")
	}
}

func TestResolveAgentChoiceOutOfRange(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	customNum := len(order) + 1

	for _, input := range []string{"0", "-1", "99", fmt.Sprintf("%d", customNum)} {
		got := resolveAgentChoice(input, order, builtins, customNum)
		if got != "" {
			t.Errorf("resolveAgentChoice(%q) = %q, want empty", input, got)
		}
	}
}

func TestResolveAgentChoiceUnknown(t *testing.T) {
	order := config.BuiltinProviderOrder()
	builtins := config.BuiltinProviders()
	got := resolveAgentChoice("vim", order, builtins, len(order)+1)
	if got != "" {
		t.Errorf("resolveAgentChoice('vim') = %q, want empty", got)
	}
}

func TestDoAgentAddWithPromptTemplate(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data
	f.Files[filepath.Join("/city", "pack.toml")] = []byte(`[pack]
name = "bright-lights"
schema = 2
`)
	f.Files[filepath.Join("/city", "templates", "worker.md")] = []byte("prompt")

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(f, "/city", "worker", "templates/worker.md", "", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentAdd = %d, want 0; stderr: %s", code, stderr.String())
	}

	got, ok := f.Files[filepath.Join("/city", "agents", "worker", "prompt.template.md")]
	if !ok {
		t.Fatal("agents/worker/prompt.template.md missing")
	}
	if string(got) != "prompt" {
		t.Errorf("prompt.template.md = %q, want copied prompt", got)
	}
	cfg2, err := loadCityConfigFS(f, filepath.Join("/city", "city.toml"))
	if err != nil {
		t.Fatalf("loadCityConfigFS: %v", err)
	}
	explicit := explicitAgents(cfg2.Agents)
	found := false
	for _, a := range explicit {
		if a.Name == "worker" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("explicit agents = %#v, want worker", explicit)
	}
}

// --- gc prime tests ---

func TestDoPrimeWithKnownAgent(t *testing.T) {
	// Set up a temp city with a mayor agent that has a prompt_template.
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptContent := "You are the mayor. Plan and delegate work.\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "mayor.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	// Chdir into the city so findCity works.
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrime([]string{"mayor"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != promptContent {
		t.Errorf("stdout = %q, want %q", stdout.String(), promptContent)
	}
}

func TestDoPrimeUsesGCAgentEnv(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptContent := "You are the mayor. Plan and delegate work.\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "mayor.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_AGENT", "mayor")

	var stdout, stderr bytes.Buffer
	code := doPrime(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != promptContent {
		t.Errorf("stdout = %q, want %q", stdout.String(), promptContent)
	}
}

func TestDoPrimeWithDiscoveredCityAgent(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.toml"), []byte("[pack]\nname = \"backstage\"\nschema = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "agents", "ada"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptContent := "You are Ada.\n"
	if err := os.WriteFile(filepath.Join(dir, "agents", "ada", "prompt.template.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrime([]string{"ada"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != promptContent {
		t.Errorf("stdout = %q, want %q", stdout.String(), promptContent)
	}
}

func TestDoPrimeWithUnknownAgent(t *testing.T) {
	// Set up a temp city with a mayor agent.
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrime([]string{"nonexistent"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0", code)
	}
	if stdout.String() != defaultPrimePrompt {
		t.Errorf("stdout = %q, want default prompt", stdout.String())
	}
}

// TestDoPrimeStrictUnknownAgent verifies --strict returns a non-zero exit
// code and writes a descriptive error to stderr when the named agent is
// not in the city config. Regression test for #445.
func TestDoPrimeStrictUnknownAgent(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"nonexistent"}, &stdout, &stderr, false, true)
	if code == 0 {
		t.Fatalf("doPrimeWithMode(strict=true, unknown agent) = 0, want non-zero; stderr: %s", stderr.String())
	}
	if stdout.String() == defaultPrimePrompt {
		t.Errorf("strict mode should not emit default prompt, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), `agent "nonexistent" not found`) {
		t.Errorf("stderr = %q, want to contain 'agent \"nonexistent\" not found'", stderr.String())
	}
}

// TestDoPrimeStrictKnownAgent verifies --strict does NOT error when the
// agent exists and has a renderable prompt.
func TestDoPrimeStrictKnownAgent(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptContent := "mayor prompt content"
	if err := os.WriteFile(filepath.Join(promptDir, "mayor.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"mayor"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict=true, known agent) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), promptContent) {
		t.Errorf("stdout = %q, want to contain %q", stdout.String(), promptContent)
	}
}

// TestDoPrimeStrictNoCity verifies --strict errors when no city config
// can be resolved, rather than silently emitting the default prompt.
func TestDoPrimeStrictNoCity(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"anyname"}, &stdout, &stderr, false, true)
	if code == 0 {
		t.Fatalf("doPrimeWithMode(strict=true, no city) = 0, want non-zero; stderr: %s", stderr.String())
	}
	if stdout.String() == defaultPrimePrompt {
		t.Errorf("strict mode should not emit default prompt when no city, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "no city config") {
		t.Errorf("stderr = %q, want to contain 'no city config'", stderr.String())
	}
}

// TestDoPrimeStrictNoAgentName verifies --strict errors when no agent name
// is available from args, GC_ALIAS, or GC_AGENT.
func TestDoPrimeStrictNoAgentName(t *testing.T) {
	t.Setenv("GC_ALIAS", "")
	t.Setenv("GC_AGENT", "")

	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode(nil, &stdout, &stderr, false, true)
	if code == 0 {
		t.Fatalf("doPrimeWithMode(strict=true, no name) = 0, want non-zero; stderr: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), "--strict requires an agent name") {
		t.Errorf("stderr = %q, want to contain '--strict requires an agent name'", stderr.String())
	}
}

// TestDoPrimeStrictAgentWithEmptyPromptTemplate verifies that a
// single-session agent with no prompt_template configured — a supported
// config shape — falls through to the default prompt even under --strict,
// rather than being reported as an error. Strict is for debugging typos
// and template mistakes, not for rejecting valid minimal configs.
func TestDoPrimeStrictAgentWithEmptyPromptTemplate(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Agent is in the config but has no prompt_template and isn't a pool
	// or formula_v2 agent. Non-strict today emits defaultPrimePrompt.
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"mayor"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict=true, agent without prompt_template) = %d, want 0 (supported config); stderr: %s", code, stderr.String())
	}
	if stdout.String() != defaultPrimePrompt {
		t.Errorf("stdout = %q, want defaultPrimePrompt (agent without prompt_template should fall through)", stdout.String())
	}
}

// TestDoPrimeStrictMissingTemplateFile verifies --strict errors with a
// distinct, diagnostic message when the agent's prompt_template points
// at a file that doesn't exist. This is the error case renderPrompt
// silently swallows by returning "", which strict mode needs to surface
// with the underlying stat reason.
func TestDoPrimeStrictMissingTemplateFile(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/does-not-exist.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"mayor"}, &stdout, &stderr, false, true)
	if code == 0 {
		t.Fatalf("doPrimeWithMode(strict=true, missing template file) = 0, want non-zero; stderr: %s", stderr.String())
	}
	if stdout.String() == defaultPrimePrompt {
		t.Errorf("strict mode should not emit default prompt when template file missing, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), `prompt_template "prompts/does-not-exist.md"`) {
		t.Errorf("stderr = %q, want to reference the missing template path", stderr.String())
	}
}

func TestDoPrimeStrictAbsoluteTemplatePath(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptDir := t.TempDir()
	promptPath := filepath.Join(promptDir, "mayor.md")
	if err := os.WriteFile(promptPath, []byte("absolute mayor prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf(`[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = %q
`, promptPath)
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"mayor"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict=true, absolute template path) = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != "absolute mayor prompt" {
		t.Errorf("stdout = %q, want absolute template content", stdout.String())
	}
}

// TestDoPrimeStrictTemplateRendersLegitimatelyEmpty verifies that --strict
// does NOT error when a template file exists but produces empty output.
// Templates with conditional blocks (e.g., `{{if .RigName}}...{{end}}`)
// can legitimately evaluate to empty under some contexts; strict mode is
// a typo/missing-file detector, not a check that templates produce
// substantial content. The absence of this test would let the missing-
// file strict check quietly regress into a broader empty-render check.
func TestDoPrimeStrictTemplateRendersLegitimatelyEmpty(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Template file exists but renders to empty string under this context.
	// {{if}} with a missing/empty key (RigName is empty when GC_RIG isn't set)
	// short-circuits the whole template body.
	emptyTemplate := `{{if .RigName}}You are in rig {{.RigName}}.{{end}}`
	if err := os.WriteFile(filepath.Join(promptDir, "mayor.md"), []byte(emptyTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	// Clear GC_RIG so .RigName evaluates to empty and the conditional
	// short-circuits. Without this, an ambient GC_RIG would produce output.
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_RIG_ROOT", "")
	t.Setenv("GC_AGENT", "")
	t.Setenv("GC_ALIAS", "")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"mayor"}, &stdout, &stderr, false, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict=true, legitimately-empty template) = %d, want 0; stderr: %s", code, stderr.String())
	}
}

// TestDoPrimeStrictHookModeDoesNotPersistSessionOnFailure verifies that
// when --strict fails because the agent isn't found, hook-mode side
// effects (persisting the session ID to .runtime/session_id) do NOT fire.
// A failing strict invocation must not leave partial state behind.
func TestDoPrimeStrictHookModeDoesNotPersistSessionOnFailure(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	// Present a session ID the way a runtime hook would.
	t.Setenv("GC_SESSION_ID", "test-session-123")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"nonexistent"}, &stdout, &stderr, true, true)
	if code == 0 {
		t.Fatalf("doPrimeWithMode(strict=true, hook=true, unknown agent) = 0, want non-zero; stderr: %s", stderr.String())
	}

	// The critical assertion: no .runtime/session_id should have been created.
	sessionFile := filepath.Join(dir, ".runtime", "session_id")
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Errorf("strict failure should not persist session id, but %s exists (err=%v)", sessionFile, err)
	}
}

// TestDoPrimeStrictHookModeMissingTemplateDoesNotPersistSessionOnFailure
// verifies that strict template validation also runs before hook-mode side
// effects. A missing prompt_template is a strict failure, so it must not
// leave behind a session id for the failed hook invocation.
func TestDoPrimeStrictHookModeMissingTemplateDoesNotPersistSessionOnFailure(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/does-not-exist.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_SESSION_ID", "test-session-missing-template")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"mayor"}, &stdout, &stderr, true, true)
	if code == 0 {
		t.Fatalf("doPrimeWithMode(strict=true, hook=true, missing template) = 0, want non-zero; stderr: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), `prompt_template "prompts/does-not-exist.md"`) {
		t.Errorf("stderr = %q, want to reference the missing template path", stderr.String())
	}

	sessionFile := filepath.Join(dir, ".runtime", "session_id")
	if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
		t.Errorf("strict template failure should not persist session id, but %s exists (err=%v)", sessionFile, err)
	}
}

// TestDoPrimeStrictHookModePersistsSessionOnSuccess is the contrast test:
// when --strict + --hook succeeds (agent is found, prompt renders),
// session-id persistence DOES fire — the deferral is not a regression of
// hook behavior for the success path.
func TestDoPrimeStrictHookModePersistsSessionOnSuccess(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptDir, "mayor.md"), []byte("mayor prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_SESSION_ID", "test-session-456")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"mayor"}, &stdout, &stderr, true, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict=true, hook=true, known agent) = %d, want 0; stderr: %s", code, stderr.String())
	}
	sessionFile := filepath.Join(dir, ".runtime", "session_id")
	content, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatalf("expected session id persisted to %s on strict success, got err: %v", sessionFile, err)
	}
	if !strings.Contains(string(content), "test-session-456") {
		t.Errorf("session id file contents = %q, want to contain 'test-session-456'", string(content))
	}
}

// TestDoPrimeStrictUnreadableTemplateFile verifies the template-read check
// catches permission-denied as well as not-exists. os.Stat would succeed on
// a chmod-000 file, but renderPrompt cannot read it — strict needs to
// surface that as an error rather than letting the empty render fall
// through to the default prompt. Skips if running as root, since root
// bypasses POSIX permission checks.
func TestDoPrimeStrictUnreadableTemplateFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission-denied check is bypassed when running as root")
	}

	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	templatePath := filepath.Join(promptDir, "mayor.md")
	if err := os.WriteFile(templatePath, []byte("mayor prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Strip read permission so the file exists (Stat succeeds) but cannot be read.
	if err := os.Chmod(templatePath, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(templatePath, 0o644) })

	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"mayor"}, &stdout, &stderr, false, true)
	if code == 0 {
		t.Fatalf("doPrimeWithMode(strict=true, unreadable template) = 0, want non-zero; stderr: %s", stderr.String())
	}
	if stdout.String() == defaultPrimePrompt {
		t.Errorf("strict mode should not emit default prompt for unreadable template, got %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), `prompt_template "prompts/mayor.md"`) {
		t.Errorf("stderr = %q, want to reference the unreadable template path", stderr.String())
	}
}

// TestDoPrimeStrictHookModeOnSuspendedAgentPersistsSessionID guards a
// behavior parity that was missed in the first pass: a suspended agent
// is a legitimate quiet state, not a strict failure, so strict+hook on
// a suspended agent must still persist the session-id (matching what
// non-strict+hook does via its eager call at the top of the function).
// Without this guard, the strict deferral silently drops session-id
// persistence on the suspended-agent success path.
func TestDoPrimeStrictHookModeOnSuspendedAgentPersistsSessionID(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
suspended = true
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	t.Setenv("GC_SESSION_ID", "test-session-suspended")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode([]string{"mayor"}, &stdout, &stderr, true, true)
	if code != 0 {
		t.Fatalf("doPrimeWithMode(strict=true, hook=true, suspended agent) = %d, want 0 (suspended is a quiet success); stderr: %s", code, stderr.String())
	}
	if stdout.String() != "" {
		t.Errorf("stdout = %q, want empty (suspended)", stdout.String())
	}
	sessionFile := filepath.Join(dir, ".runtime", "session_id")
	content, err := os.ReadFile(sessionFile)
	if err != nil {
		t.Fatalf("expected session id persisted to %s on strict+hook+suspended success, got err: %v", sessionFile, err)
	}
	if !strings.Contains(string(content), "test-session-suspended") {
		t.Errorf("session id file contents = %q, want to contain 'test-session-suspended'", string(content))
	}
}

func TestDoPrimeNoArgs(t *testing.T) {
	// Outside any city — should still output default prompt.
	dir := t.TempDir()
	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrime(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0", code)
	}
	if stdout.String() != defaultPrimePrompt {
		t.Errorf("stdout = %q, want default prompt", stdout.String())
	}
}

func TestDoPrimeBareName(t *testing.T) {
	// "gc prime polecat" should find agent with name="polecat" even when
	// it has dir="myrig" — bare template name lookup for pool agents.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptContent := "You are a pool worker.\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "polecat.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	tomlContent := `[workspace]
name = "test-city"

[[agent]]
name = "polecat"
dir = "myrig"
prompt_template = "prompts/polecat.md"

[agent.pool]
min = 1
max = 3
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(tomlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrime([]string{"polecat"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != promptContent {
		t.Errorf("stdout = %q, want pool worker prompt %q", stdout.String(), promptContent)
	}
}

func TestDoPrimePoolAgentFallback(t *testing.T) {
	// An explicit pool agent with no prompt_template reads the materialized
	// pool-worker prompt from the materialized core system pack.
	dir := t.TempDir()
	if err := materializeBuiltinPrompts(dir); err != nil {
		t.Fatalf("materializeBuiltinPrompts: %v", err)
	}
	tomlContent := `[workspace]
name = "test-city"

[[agent]]
name = "polecat"
dir = "myrig"
start_command = "echo"

[agent.pool]
min = 0
max = -1
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(tomlContent), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrime([]string{"polecat"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	// Should get pool-worker prompt, not the generic default.
	if out == defaultPrimePrompt {
		t.Error("pool agent got generic defaultPrimePrompt, want pool-worker prompt")
	}
	if !strings.Contains(out, "Molecules") {
		t.Error("pool-worker prompt missing molecule instructions")
	}
	if !strings.Contains(out, "GUPP") {
		t.Error("pool-worker prompt missing GUPP")
	}
}

func materializeBuiltinPrompts(cityPath string) error {
	return MaterializeBuiltinPacks(cityPath)
}

func TestDoPrimeHookPersistsSessionID(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptContent := "You are the mayor. Plan and delegate work.\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "mayor.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_AGENT", "mayor")

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldPrimeStdin := primeStdin
	primeStdin = func() *os.File { return reader }
	t.Cleanup(func() {
		primeStdin = oldPrimeStdin
		_ = reader.Close()
	})
	if err := json.NewEncoder(writer).Encode(map[string]string{
		"session_id": "sess-123",
		"source":     "startup",
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode(nil, &stdout, &stderr, true, false)
	if code != 0 {
		t.Fatalf("doPrimeWithMode = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, promptContent) {
		t.Errorf("stdout = %q, want prompt content %q", out, promptContent)
	}
	if !strings.Contains(out, "[test-city] mayor") {
		t.Errorf("stdout = %q, want hook beacon", out)
	}
	if strings.Contains(out, "Run `gc prime`") {
		t.Errorf("stdout = %q, hook beacon should not add manual gc prime instruction", out)
	}

	data, err := os.ReadFile(filepath.Join(dir, ".runtime", "session_id"))
	if err != nil {
		t.Fatalf("reading persisted session ID: %v", err)
	}
	if got := strings.TrimSpace(string(data)); got != "sess-123" {
		t.Errorf("persisted session ID = %q, want %q", got, "sess-123")
	}
}

func TestDoPrimeGeminiHookPersistsProviderSessionKey(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(promptsDir, "probe.md"), []byte("probe prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"
provider = "gemini"

[[agent]]
name = "probe"
prompt_template = "prompts/probe.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Title: "probe",
		Type:  "task",
		Labels: []string{
			"gc:session",
			"template:probe",
		},
		Metadata: map[string]string{
			"template":     "probe",
			"provider":     "gemini",
			"session_name": "probe",
			"state":        "active",
			"work_dir":     dir,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_AGENT", "probe")
	t.Setenv("GC_SESSION_ID", sessionBead.ID)
	t.Setenv("GEMINI_SESSION_ID", "gemini-provider-session")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode(nil, &stdout, &stderr, true, false)
	if code != 0 {
		t.Fatalf("doPrimeWithMode = %d, want 0; stderr: %s", code, stderr.String())
	}

	updatedStore, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := updatedStore.Get(sessionBead.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(updated.Metadata["session_key"]); got != "gemini-provider-session" {
		t.Fatalf("session_key = %q, want Gemini provider session id", got)
	}
}

func TestDoPrimeHookFallsBackToGCTemplateForManualSessionAlias(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const promptContent = "worker inference probe prompt\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "probe.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "probe"
prompt_template = "prompts/probe.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_ALIAS", "probe-live")
	t.Setenv("GC_TEMPLATE", "probe")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode(nil, &stdout, &stderr, true, false)
	if code != 0 {
		t.Fatalf("doPrimeWithMode = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, promptContent) {
		t.Fatalf("stdout = %q, want probe prompt", out)
	}
	if strings.Contains(out, defaultPrimePrompt) || strings.Contains(out, "Check for available work") {
		t.Fatalf("stdout = %q, want no default worker prompt", out)
	}
}

func TestDoPrimeHookFallsBackToSessionTemplateForManualSessionAlias(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const promptContent = "worker inference probe prompt\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "probe.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "probe"
prompt_template = "prompts/probe.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	store, err := openCityStoreAt(dir)
	if err != nil {
		t.Fatal(err)
	}
	sessionBead, err := store.Create(beads.Bead{
		Title:  "probe",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "template:probe"},
		Metadata: map[string]string{
			"alias":        "probe-live",
			"template":     "probe",
			"session_name": "s-probe-live",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_ALIAS", "probe-live")
	t.Setenv("GC_SESSION_ID", sessionBead.ID)
	t.Setenv("GC_TEMPLATE", "")

	var stdout, stderr bytes.Buffer
	code := doPrimeWithMode(nil, &stdout, &stderr, true, false)
	if code != 0 {
		t.Fatalf("doPrimeWithMode = %d, want 0; stderr: %s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, promptContent) {
		t.Fatalf("stdout = %q, want probe prompt", out)
	}
	if strings.Contains(out, defaultPrimePrompt) || strings.Contains(out, "Check for available work") {
		t.Fatalf("stdout = %q, want no default worker prompt", out)
	}
}

func TestDoPrimeFallsBackToGCAliasWhenGCAgentUnresolvable(t *testing.T) {
	// When GC_AGENT is a session bead ID (not an agent name), gc prime should
	// fall back to GC_ALIAS to resolve the agent.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	promptsDir := filepath.Join(dir, "prompts")
	if err := os.MkdirAll(promptsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	promptContent := "You are the mayor. Plan and delegate work.\n"
	if err := os.WriteFile(filepath.Join(promptsDir, "mayor.md"), []byte(promptContent), 0o644); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "test-city"

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"
`
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}

	orig, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(orig) })
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_AGENT", "bl-9jl") // bead ID, not an agent name
	t.Setenv("GC_ALIAS", "mayor")

	var stdout, stderr bytes.Buffer
	code := doPrime(nil, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doPrime = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stdout.String() != promptContent {
		t.Errorf("stdout = %q, want %q (got default prompt instead of mayor template)", stdout.String(), promptContent)
	}
}

// --- findEnclosingRig tests ---

func TestFindEnclosingRig(t *testing.T) {
	rigs := []config.Rig{
		{Name: "alpha", Path: "/projects/alpha"},
		{Name: "beta", Path: "/projects/beta"},
	}

	// Exact match.
	name, rp, found := findEnclosingRig("/projects/alpha", rigs)
	if !found || name != "alpha" || rp != "/projects/alpha" {
		t.Errorf("exact match: name=%q path=%q found=%v", name, rp, found)
	}

	// Subdirectory match.
	name, rp, found = findEnclosingRig("/projects/beta/src/main", rigs)
	if !found || name != "beta" || rp != "/projects/beta" {
		t.Errorf("subdir match: name=%q path=%q found=%v", name, rp, found)
	}

	// No match.
	_, _, found = findEnclosingRig("/other/project", rigs)
	if found {
		t.Error("expected no match for /other/project")
	}

	// Picks correct rig (not prefix collision).
	rigs2 := []config.Rig{
		{Name: "app", Path: "/projects/app"},
		{Name: "app-web", Path: "/projects/app-web"},
	}
	name, _, found = findEnclosingRig("/projects/app-web/src", rigs2)
	if !found || name != "app-web" {
		t.Errorf("prefix collision: name=%q found=%v, want app-web", name, found)
	}
}

func makeRigSymlinkAliasFixture(t *testing.T) (rigPath, aliasRigPath string) {
	t.Helper()

	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	rigPath = filepath.Join(realRoot, "my-project")
	if err := os.MkdirAll(filepath.Join(rigPath, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(root, "alias")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlink setup unavailable: %v", err)
	}
	return rigPath, filepath.Join(aliasRoot, "my-project")
}

func TestFindEnclosingRigResolvesSymlinkAlias(t *testing.T) {
	rigPath, aliasRigPath := makeRigSymlinkAliasFixture(t)
	rigs := []config.Rig{{Name: "my-project", Path: rigPath}}
	dirViaAlias := filepath.Join(aliasRigPath, "src")

	name, rp, found := findEnclosingRig(dirViaAlias, rigs)
	if !found || name != "my-project" || rp != rigPath {
		t.Fatalf("symlink alias match: name=%q path=%q found=%v, want name=%q path=%q found=true", name, rp, found, "my-project", rigPath)
	}
}

func TestFindEnclosingRigPrefersDeepestNormalizedMatch(t *testing.T) {
	root := t.TempDir()
	realRoot := filepath.Join(root, "real")
	parentRigPath := filepath.Join(realRoot, "my-project")
	nestedRigPath := filepath.Join(parentRigPath, "nested")
	if err := os.MkdirAll(filepath.Join(nestedRigPath, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	aliasRoot := filepath.Join(root, "extremely-long-alias-name")
	if err := os.Symlink(realRoot, aliasRoot); err != nil {
		t.Skipf("symlink setup unavailable: %v", err)
	}

	rigs := []config.Rig{
		{Name: "parent", Path: filepath.Join(aliasRoot, "my-project")},
		{Name: "nested", Path: nestedRigPath},
	}
	dirViaAlias := filepath.Join(aliasRoot, "my-project", "nested", "src")

	name, rp, found := findEnclosingRig(dirViaAlias, rigs)
	if !found || name != "nested" || rp != nestedRigPath {
		t.Fatalf("deepest normalized match: name=%q path=%q found=%v, want name=%q path=%q found=true", name, rp, found, "nested", nestedRigPath)
	}
}

func TestCurrentRigContextUsesGCDirThroughSymlinkAlias(t *testing.T) {
	rigPath, aliasRigPath := makeRigSymlinkAliasFixture(t)
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "my-project", Path: rigPath}},
	}

	t.Setenv("GC_DIR", aliasRigPath)
	if got := currentRigContext(cfg); got != "my-project" {
		t.Fatalf("currentRigContext() = %q, want %q", got, "my-project")
	}
}

func TestCurrentRigContextUsesWorkingDirThroughSymlinkAlias(t *testing.T) {
	rigPath, aliasRigPath := makeRigSymlinkAliasFixture(t)
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "my-project", Path: rigPath}},
	}

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(aliasRigPath); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	t.Setenv("GC_DIR", "")
	if got := currentRigContext(cfg); got != "my-project" {
		t.Fatalf("currentRigContext() = %q, want %q", got, "my-project")
	}
}

// --- doAgentAdd with --dir and --suspended ---

func TestDoAgentAddWithDir(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data
	f.Files[filepath.Join("/city", "pack.toml")] = []byte(`[pack]
name = "bright-lights"
schema = 2
`)

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(f, "/city", "builder", "", "hello-world", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentAdd = %d, want 0; stderr: %s", code, stderr.String())
	}

	agentToml, ok := f.Files[filepath.Join("/city", "agents", "builder", "agent.toml")]
	if !ok {
		t.Fatal("agents/builder/agent.toml missing")
	}
	if !strings.Contains(string(agentToml), "dir = \"hello-world\"") {
		t.Errorf("agent.toml = %q, want dir", agentToml)
	}
	got, err := loadCityConfigFS(f, filepath.Join("/city", "city.toml"))
	if err != nil {
		t.Fatalf("loadCityConfigFS: %v", err)
	}
	explicit := explicitAgents(got.Agents)
	found := false
	for _, a := range explicit {
		if a.Name != "builder" {
			continue
		}
		found = true
		if a.Dir != "hello-world" {
			t.Errorf("Agents[builder].Dir = %q, want %q", a.Dir, "hello-world")
		}
	}
	if !found {
		t.Fatalf("explicit agents = %#v, want builder", explicit)
	}
}

func TestDoAgentAddWithSuspended(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data
	f.Files[filepath.Join("/city", "pack.toml")] = []byte(`[pack]
name = "bright-lights"
schema = 2
`)

	var stdout, stderr bytes.Buffer
	code := doAgentAdd(f, "/city", "builder", "", "hello-world", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentAdd = %d, want 0; stderr: %s", code, stderr.String())
	}

	agentToml, ok := f.Files[filepath.Join("/city", "agents", "builder", "agent.toml")]
	if !ok {
		t.Fatal("agents/builder/agent.toml missing")
	}
	if !strings.Contains(string(agentToml), "suspended = true") {
		t.Errorf("agent.toml = %q, want suspended = true", agentToml)
	}
	got, err := loadCityConfigFS(f, filepath.Join("/city", "city.toml"))
	if err != nil {
		t.Fatalf("loadCityConfigFS: %v", err)
	}
	explicit := explicitAgents(got.Agents)
	found := false
	for _, a := range explicit {
		if a.Name != "builder" {
			continue
		}
		found = true
		if !a.Suspended {
			t.Error("Agents[builder].Suspended = false, want true")
		}
		if a.Dir != "hello-world" {
			t.Errorf("Agents[builder].Dir = %q, want %q", a.Dir, "hello-world")
		}
	}
	if !found {
		t.Fatalf("explicit agents = %#v, want builder", explicit)
	}
}

// --- doAgentSuspend ---

func TestDoAgentSuspend(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.City{
		Workspace: config.Workspace{Name: "bright-lights"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "builder"},
		},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stdout, stderr bytes.Buffer
	code := doAgentSuspend(f, "/city", "builder", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentSuspend = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Suspended agent 'builder'") {
		t.Errorf("stdout = %q, want suspend message", stdout.String())
	}

	// Verify config was updated.
	written := f.Files[filepath.Join("/city", "city.toml")]
	got, err := config.Parse(written)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if !got.Agents[1].Suspended {
		t.Error("Agents[1].Suspended = false after suspend, want true")
	}
	// Verify TOML contains the field.
	if !strings.Contains(string(written), "suspended = true") {
		t.Errorf("written TOML missing 'suspended = true':\n%s", written)
	}
}

func TestDoAgentSuspendNotFound(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stderr bytes.Buffer
	code := doAgentSuspend(f, "/city", "nonexistent", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doAgentSuspend = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want 'not found'", stderr.String())
	}
}

// --- doAgentResume ---

func TestDoAgentResume(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.City{
		Workspace: config.Workspace{Name: "bright-lights"},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)},
			{Name: "builder", Suspended: true, MaxActiveSessions: intPtr(1)},
		},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stdout, stderr bytes.Buffer
	code := doAgentResume(f, "/city", "builder", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doAgentResume = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Resumed agent 'builder'") {
		t.Errorf("stdout = %q, want resume message", stdout.String())
	}

	// Verify config was updated.
	written := f.Files[filepath.Join("/city", "city.toml")]
	got, err := config.Parse(written)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if got.Agents[1].Suspended {
		t.Error("Agents[1].Suspended = true after resume, want false")
	}
	// Verify TOML omits the field (omitempty).
	if strings.Contains(string(written), "suspended") {
		t.Errorf("written TOML should omit 'suspended' when false:\n%s", written)
	}
}

func TestDoAgentResumeNotFound(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stderr bytes.Buffer
	code := doAgentResume(f, "/city", "nonexistent", &bytes.Buffer{}, &stderr)
	if code != 1 {
		t.Errorf("doAgentResume = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not found") {
		t.Errorf("stderr = %q, want 'not found'", stderr.String())
	}
}
