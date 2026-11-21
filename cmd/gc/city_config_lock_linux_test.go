package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
)

const cityConfigLockTestCity = "[workspace]\nname = \"city1\"\n\n[providers.claude]\nbase = \"builtin:claude\"\n\n[[agent]]\nname = \"worker\"\nprovider = \"claude\"\n"

// holdCityConfigLock takes the city config lock as another process would and
// returns its release, which the test cleanup also runs.
func holdCityConfigLock(t *testing.T, cityDir string) func() {
	t.Helper()
	release, err := configedit.LockCity(fsys.OSFS{}, cityDir, nil)
	if err != nil {
		t.Fatalf("LockCity: %v", err)
	}
	t.Cleanup(release)
	return release
}

// appendAgentToCityToml stands in for another process's edit: it appends an
// inline agent to city.toml.
func appendAgentToCityToml(t *testing.T, cityDir, name string) {
	t.Helper()
	path := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	data = append(data, []byte("\n[[agent]]\nname = \""+name+"\"\nprovider = \"claude\"\n")...)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

func newCityConfigLockControllerState(t *testing.T, cityDir string) *controllerState {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("loading city.toml: %v", err)
	}
	cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "city1", cityDir)
	cs.pokeCh = make(chan struct{}, 1)
	cs.configDirty = &atomic.Bool{}
	return cs
}

func loadAgentsByName(t *testing.T, cityDir string) map[string]config.Agent {
	t.Helper()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("loading city.toml: %v", err)
	}
	agents := make(map[string]config.Agent, len(cfg.Agents))
	for _, a := range cfg.Agents {
		agents[a.Name] = a
	}
	return agents
}

func awaitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s did not finish after the city config lock was released", what)
		return nil
	}
}

// An API config mutation that finds another process mid-edit waits for that
// edit and applies its own change on top, so city.toml keeps both.
func TestControllerConfigMutationWaitsForCityConfigLock(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfigLockTestCity), 0o644); err != nil {
		t.Fatal(err)
	}
	cs := newCityConfigLockControllerState(t, cityDir)

	release := holdCityConfigLock(t, cityDir)
	done := make(chan error, 1)
	go func() { done <- cs.SuspendAgent("worker") }()
	testutil.AwaitFlockWaiters(t, configedit.CityLockFile(cityDir), 1)
	appendAgentToCityToml(t, cityDir, "builder")
	release()
	if err := awaitErr(t, done, "SuspendAgent"); err != nil {
		t.Fatalf("SuspendAgent: %v", err)
	}

	agents := loadAgentsByName(t, cityDir)
	if _, ok := agents["builder"]; !ok {
		t.Error("agent builder written by the other process was lost")
	}
	if !agents["worker"].Suspended {
		t.Error("worker is not suspended: the API mutation was lost")
	}
}

// otherProcessDaemonEdit is the edit another process makes to city.toml while
// it holds the city config lock in the CLI writer tests.
const otherProcessDaemonEdit = "\n[daemon]\npatrol_interval = \"45s\"\n"

func appendToCityToml(t *testing.T, cityDir, text string) {
	t.Helper()
	path := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading city.toml: %v", err)
	}
	if err := os.WriteFile(path, append(data, text...), 0o644); err != nil {
		t.Fatalf("writing city.toml: %v", err)
	}
}

func requireOtherProcessDaemonEdit(t *testing.T, cityDir string) {
	t.Helper()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatalf("loading city.toml: %v", err)
	}
	if cfg.Daemon.PatrolInterval != "45s" {
		data, _ := os.ReadFile(filepath.Join(cityDir, "city.toml"))
		t.Fatalf("daemon.patrol_interval = %q, want the other process's 45s; city.toml:\n%s", cfg.Daemon.PatrolInterval, data)
	}
}

func requireFileContains(t *testing.T, path, want string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("%s does not contain %q:\n%s", path, want, data)
	}
}

// cliCityConfigWriterCase is one CLI entry point that rewrites an existing
// city's config files.
type cliCityConfigWriterCase struct {
	name string
	// setup lays out the city and stubs the command's outside seams.
	setup func(t *testing.T, cityDir string)
	// run performs the command's direct config write and returns its exit
	// code; stderr receives its diagnostics.
	run func(cityDir string, stdout, stderr *bytes.Buffer) int
	// verify checks that the command's own change is on disk.
	verify func(t *testing.T, cityDir string)
}

func cliCityConfigWriterCases() []cliCityConfigWriterCase {
	return []cliCityConfigWriterCase{
		{
			name: "gc agent add",
			setup: func(t *testing.T, cityDir string) {
				writeSchema2RigCity(t, cityDir, "city1", "[workspace]\n", "")
			},
			run: func(cityDir string, stdout, stderr *bytes.Buffer) int {
				return doAgentAdd(fsys.OSFS{}, cityDir, "helper", "", "", false, stdout, stderr)
			},
			verify: func(t *testing.T, cityDir string) {
				if _, err := os.Stat(filepath.Join(cityDir, "agents", "helper", "prompt.template.md")); err != nil {
					t.Fatalf("agent helper scaffold: %v", err)
				}
			},
		},
		{
			name: "gc agent suspend fallback",
			setup: func(t *testing.T, cityDir string) {
				if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfigLockTestCity), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			run: func(cityDir string, stdout, stderr *bytes.Buffer) int {
				return doAgentSuspend(fsys.OSFS{}, cityDir, "worker", stdout, stderr)
			},
			verify: func(t *testing.T, cityDir string) {
				if !loadAgentsByName(t, cityDir)["worker"].Suspended {
					t.Fatal("worker is not suspended")
				}
			},
		},
		{
			name: "gc rig add",
			setup: func(t *testing.T, cityDir string) {
				writeSchema2RigCity(t, cityDir, "city1", "[workspace]\n", "")
				if err := os.MkdirAll(filepath.Join(cityDir, "..", "frontend"), 0o755); err != nil {
					t.Fatal(err)
				}
				t.Setenv("GC_DOLT", "skip")
				t.Setenv("GC_BEADS", "bd")
				oldSend := sendReloadControlRequestHook
				t.Cleanup(func() { sendReloadControlRequestHook = oldSend })
				sendReloadControlRequestHook = func(string, reloadControlRequest) (reloadControlReply, error) {
					return reloadControlReply{}, errControllerUnavailable
				}
			},
			run: func(cityDir string, stdout, stderr *bytes.Buffer) int {
				return doRigAdd(fsys.OSFS{}, cityDir, filepath.Join(cityDir, "..", "frontend"), nil, "", "", "", false, false, stdout, stderr)
			},
			verify: func(t *testing.T, cityDir string) {
				requireFileContains(t, filepath.Join(cityDir, "city.toml"), "frontend")
			},
		},
		{
			name: "gc import add",
			setup: func(t *testing.T, cityDir string) {
				clearGCEnv(t)
				writeCityToml(t, cityDir, "[workspace]\nname = \"city1\"\n")
				prevResolve, prevConstraint, prevSync := resolveImportVersion, defaultImportConstraint, syncImports
				t.Cleanup(func() {
					resolveImportVersion, defaultImportConstraint, syncImports = prevResolve, prevConstraint, prevSync
				})
				resolveImportVersion = func(_, _, _ string) (packman.ResolvedVersion, error) {
					return packman.ResolvedVersion{Version: "1.4.2", Commit: "abc123"}, nil
				}
				defaultImportConstraint = func(_ string) (string, error) { return "^1.4", nil }
				syncImports = func(_ string, _ map[string]config.Import, _ packman.InstallMode) (*packman.Lockfile, error) {
					return &packman.Lockfile{
						Schema: packman.LockfileSchema,
						Packs: map[string]packman.LockedPack{
							"https://github.com/example/tools.git": {Version: "1.4.2", Commit: "abc123"},
						},
					}, nil
				}
			},
			run: func(cityDir string, stdout, stderr *bytes.Buffer) int {
				return doImportAdd(fsys.OSFS{}, cityDir, "https://github.com/example/tools.git", "", "", stdout, stderr)
			},
			verify: func(t *testing.T, cityDir string) {
				requireFileContains(t, filepath.Join(cityDir, "pack.toml"), "[imports.tools]")
			},
		},
		{
			name: "gc doctor --fix provider aliases",
			setup: func(t *testing.T, cityDir string) {
				if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nprovider = \"claude\"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			run: func(cityDir string, _, stderr *bytes.Buffer) int {
				return doctorFixExitCode(newProviderCatalogDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir, Output: stderr}), stderr)
			},
			verify: func(t *testing.T, cityDir string) {
				requireFileContains(t, filepath.Join(cityDir, "city.toml"), "[providers.claude]")
			},
		},
		{
			name: "gc doctor --fix v2 pack migration",
			setup: func(t *testing.T, cityDir string) {
				writeDoctorFile(t, cityDir, "city.toml", "[workspace]\nname = \"legacy-city\"\n\n[[agent]]\nname = \"worker\"\nprompt_template = \"prompts/worker.md\"\n")
				writeDoctorFile(t, cityDir, "prompts/worker.md", "Hello {{.Agent}}\n")
			},
			run: func(cityDir string, _, stderr *bytes.Buffer) int {
				return doctorFixExitCode(v2AgentFormatCheck{}.Fix(&doctor.CheckContext{CityPath: cityDir, Output: stderr}), stderr)
			},
			verify: func(t *testing.T, cityDir string) {
				if _, err := os.Stat(filepath.Join(cityDir, "agents", "worker")); err != nil {
					t.Fatalf("migrated agent dir: %v", err)
				}
			},
		},
	}
}

func doctorFixExitCode(err error, stderr *bytes.Buffer) int {
	if err != nil {
		stderr.WriteString(err.Error())
		return 1
	}
	return 0
}

// A CLI config writer that finds another process mid-edit waits for that
// edit and applies its own change on top, so neither change is lost.
func TestCLICityConfigWritersWaitForCityConfigLock(t *testing.T) {
	for _, tc := range cliCityConfigWriterCases() {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := filepath.Join(t.TempDir(), "city")
			if err := os.MkdirAll(cityDir, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, cityDir)

			release := holdCityConfigLock(t, cityDir)
			var stdout, stderr bytes.Buffer
			done := make(chan int, 1)
			go func() { done <- tc.run(cityDir, &stdout, &stderr) }()
			testutil.AwaitFlockWaiters(t, configedit.CityLockFile(cityDir), 1)
			appendToCityToml(t, cityDir, otherProcessDaemonEdit)
			release()
			select {
			case code := <-done:
				if code != 0 {
					t.Fatalf("%s = %d, stderr:\n%s", tc.name, code, stderr.String())
				}
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatalf("%s did not finish after the city config lock was released", tc.name)
			}
			tc.verify(t, cityDir)
			requireOtherProcessDaemonEdit(t, cityDir)
		})
	}
}
