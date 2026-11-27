package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
	"github.com/gastownhall/gascity/internal/platform/filelock"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/suspensionstate"
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

// appendInlineAgent appends an inline agent to city.toml. It returns its error
// rather than failing the test, so an edit running off the test goroutine can
// use it.
func appendInlineAgent(cityDir, name string) error {
	path := filepath.Join(cityDir, "city.toml")
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, "\n[[agent]]\nname = \""+name+"\"\nprovider = \"claude\"\n"...), 0o644)
}

// appendAgentToCityToml stands in for another process's edit: it appends an
// inline agent to city.toml.
func appendAgentToCityToml(t *testing.T, cityDir, name string) {
	t.Helper()
	if err := appendInlineAgent(cityDir, name); err != nil {
		t.Fatalf("appending agent %s to city.toml: %v", name, err)
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

// awaitExitCode is awaitErr for a command that reports an exit code.
func awaitExitCode(t *testing.T, ch <-chan int, what string) int {
	t.Helper()
	select {
	case code := <-ch:
		return code
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s did not finish after the city config lock was released", what)
		return 0
	}
}

// apiConfigEditCase is one way an API config edit reaches the city's config
// files.
type apiConfigEditCase struct {
	name string
	// edit makes the edit. It runs off the test goroutine.
	edit func(cs *controllerState, cityDir string) error
	// applied reports whether the edit's own change is on disk.
	applied func(agents map[string]config.Agent) bool
}

// apiConfigEditCases are a plain API mutation (mutateAndPoke) and an API
// config transaction (withConfigTransaction, through SerializeConfigWrite).
func apiConfigEditCases() []apiConfigEditCase {
	return []apiConfigEditCase{
		{
			name:    "API mutation",
			edit:    func(cs *controllerState, _ string) error { return cs.SuspendAgent("worker") },
			applied: func(agents map[string]config.Agent) bool { return agents["worker"].Suspended },
		},
		{
			name: "API config transaction",
			edit: func(cs *controllerState, cityDir string) error {
				return cs.SerializeConfigWrite(func() error { return appendInlineAgent(cityDir, "packer") })
			},
			applied: func(agents map[string]config.Agent) bool {
				_, ok := agents["packer"]
				return ok
			},
		},
	}
}

// An API config edit, a plain mutation or a config transaction, that finds
// another process mid-edit waits for that edit and applies its own change on
// top, so city.toml keeps both.
func TestControllerConfigMutationWaitsForCityConfigLock(t *testing.T) {
	for _, tc := range apiConfigEditCases() {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfigLockTestCity), 0o644); err != nil {
				t.Fatal(err)
			}
			cs := newCityConfigLockControllerState(t, cityDir)

			release := holdCityConfigLock(t, cityDir)
			done := make(chan error, 1)
			go func() { done <- tc.edit(cs, cityDir) }()
			testutil.AwaitFlockWaiters(t, configedit.CityLockFile(cityDir), 1)
			appendAgentToCityToml(t, cityDir, "builder")
			release()
			if err := awaitErr(t, done, tc.name); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}

			agents := loadAgentsByName(t, cityDir)
			if _, ok := agents["builder"]; !ok {
				t.Error("agent builder written by the other process was lost")
			}
			if !tc.applied(agents) {
				t.Errorf("the %s's own change was lost", tc.name)
			}
		})
	}
}

// An API config edit waiting for another process's edit of the city holds no
// in-process lock while it waits: the runtime reload still loads its candidate
// and publishes it, so the reconciler is never frozen behind another process's
// edit.
func TestRuntimeReloadPublishesWhileAPIConfigEditWaitsForCityConfigLock(t *testing.T) {
	for _, tc := range apiConfigEditCases() {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfigLockTestCity), 0o644); err != nil {
				t.Fatal(err)
			}
			cs := newCityConfigLockControllerState(t, cityDir)
			cfg, revision, err := cs.loadCurrentConfigSnapshot()
			if err != nil {
				t.Fatalf("loading the reload candidate: %v", err)
			}

			release := holdCityConfigLock(t, cityDir)
			done := make(chan error, 1)
			go func() { done <- tc.edit(cs, cityDir) }()
			testutil.AwaitFlockWaiters(t, configedit.CityLockFile(cityDir), 1)
			requireFinishesPromptly(t, "runtime reload", func() error {
				if !cs.tryWithConfigTransactionIdle(func() {}) {
					return errors.New("the candidate load was skipped: the waiting edit holds the config transaction fence")
				}
				if !cs.updateFromRuntime(cfg, runtime.NewFake(), revision) {
					return errors.New("the publication rejected the current on-disk config")
				}
				return nil
			})
			release()
			if err := awaitErr(t, done, tc.name); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
		})
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

func requireFileLacks(t *testing.T, path, unwanted string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if strings.Contains(string(data), unwanted) {
		t.Fatalf("%s still contains %q:\n%s", path, unwanted, data)
	}
}

// cityConfigLockToolsSource is the remote pack the gc import writer cases
// import as "tools"; cityConfigLockToolsPackToml is a root pack.toml that
// imports it.
const (
	cityConfigLockToolsSource   = "https://github.com/example/tools.git"
	cityConfigLockToolsPackToml = "[pack]\nname = \"city1\"\nschema = 1\n\n[imports.tools]\nsource = \"" + cityConfigLockToolsSource + "\"\nversion = \"^1.4\"\n"
)

// stubImportResolution makes syncImports resolve every import to packs, and
// installLockedImports and the post-install config load succeed without
// fetching anything.
func stubImportResolution(t *testing.T, packs map[string]packman.LockedPack) {
	t.Helper()
	prevSync, prevInstall, prevValidate := syncImports, installLockedImports, validateComposedConfigAfterInstall
	t.Cleanup(func() {
		syncImports, installLockedImports, validateComposedConfigAfterInstall = prevSync, prevInstall, prevValidate
	})
	syncImports = func(string, map[string]config.Import, packman.InstallMode) (*packman.Lockfile, error) {
		return &packman.Lockfile{Schema: packman.LockfileSchema, Packs: packs}, nil
	}
	installLockedImports = func(cityRoot string) (*packman.Lockfile, error) {
		return packman.ReadLockfile(fsys.OSFS{}, cityRoot)
	}
	validateComposedConfigAfterInstall = func(string) error { return nil }
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
			name: "gc import remove",
			setup: func(t *testing.T, cityDir string) {
				clearGCEnv(t)
				writeCityToml(t, cityDir, "[workspace]\nname = \"city1\"\n")
				writePackToml(t, cityDir, cityConfigLockToolsPackToml)
				stubImportResolution(t, map[string]packman.LockedPack{})
			},
			run: func(cityDir string, stdout, stderr *bytes.Buffer) int {
				return doImportRemove(fsys.OSFS{}, cityDir, "tools", stdout, stderr)
			},
			verify: func(t *testing.T, cityDir string) {
				requireFileLacks(t, filepath.Join(cityDir, "pack.toml"), "[imports.tools]")
			},
		},
		{
			name: "gc import install",
			setup: func(t *testing.T, cityDir string) {
				clearGCEnv(t)
				writeCityToml(t, cityDir, "[workspace]\nname = \"city1\"\n")
				writePackToml(t, cityDir, cityConfigLockToolsPackToml)
				stubImportResolution(t, map[string]packman.LockedPack{cityConfigLockToolsSource: {Version: "1.4.2", Commit: "abc123"}})
			},
			run: func(cityDir string, stdout, stderr *bytes.Buffer) int {
				return doImportInstall(cityDir, stdout, stderr)
			},
			verify: func(t *testing.T, cityDir string) {
				requireFileContains(t, filepath.Join(cityDir, packman.LockfileName), "abc123")
			},
		},
		{
			name: "gc import upgrade",
			setup: func(t *testing.T, cityDir string) {
				clearGCEnv(t)
				writeCityToml(t, cityDir, "[workspace]\nname = \"city1\"\n")
				writePackToml(t, cityDir, cityConfigLockToolsPackToml)
				if err := packman.WriteLockfile(fsys.OSFS{}, cityDir, &packman.Lockfile{
					Schema: packman.LockfileSchema,
					Packs:  map[string]packman.LockedPack{cityConfigLockToolsSource: {Version: "1.4.2", Commit: "abc123"}},
				}); err != nil {
					t.Fatal(err)
				}
				stubImportResolution(t, map[string]packman.LockedPack{cityConfigLockToolsSource: {Version: "1.5.0", Commit: "def456"}})
			},
			run: func(cityDir string, stdout, stderr *bytes.Buffer) int {
				return doImportUpgrade(cityDir, "", stdout, stderr)
			},
			verify: func(t *testing.T, cityDir string) {
				requireFileContains(t, filepath.Join(cityDir, packman.LockfileName), "def456")
			},
		},
		{
			name: "gc rig remove",
			setup: func(t *testing.T, cityDir string) {
				gcHome := t.TempDir()
				t.Setenv("GC_HOME", gcHome)
				resetFlags(t)
				rigDir := filepath.Join(filepath.Dir(cityDir), "frontend")
				if err := os.MkdirAll(rigDir, 0o755); err != nil {
					t.Fatal(err)
				}
				bindRigForRigResolution(t, cityDir, "city1", "frontend", rigDir)
				registerCityForRigResolution(t, gcHome, cityDir, "city1")
				cityFlag = cityDir
			},
			run: func(_ string, stdout, stderr *bytes.Buffer) int {
				return cmdRigRemove("frontend", stdout, stderr)
			},
			verify: func(t *testing.T, cityDir string) {
				requireFileLacks(t, filepath.Join(cityDir, "city.toml"), "frontend")
			},
		},
		{
			name: "gc rig set-endpoint",
			setup: func(t *testing.T, cityDir string) {
				t.Setenv("GC_BEADS", "bd")
				rigDir := filepath.Join(filepath.Dir(cityDir), "frontend")
				if err := os.MkdirAll(rigDir, 0o755); err != nil {
					t.Fatal(err)
				}
				writeRigEndpointCityConfig(t, cityDir, rigDir)
				writeRigEndpointMetadata(t, rigDir, "fe")
			},
			run: func(cityDir string, stdout, stderr *bytes.Buffer) int {
				return doRigSetEndpoint(fsys.OSFS{}, cityDir, "frontend", rigEndpointOptions{
					External:        true,
					Host:            "rig-db.example.com",
					Port:            "4406",
					AdoptUnverified: true,
				}, stdout, stderr)
			},
			verify: func(t *testing.T, cityDir string) {
				state := readRigEndpointConfigState(t, filepath.Join(filepath.Dir(cityDir), "frontend"))
				if state.EndpointStatus != contract.EndpointStatusUnverified {
					t.Fatalf("frontend endpoint status = %q, want %q", state.EndpointStatus, contract.EndpointStatusUnverified)
				}
			},
		},
	}
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

// An API mutation whose refresh fails restores the previous config before a
// CLI writer queued behind it reads, so the files end up holding the restored
// config plus only the CLI writer's change.
func TestControllerRollbackFinishesBeforeQueuedCLIWriterReads(t *testing.T) {
	want := cliAgentSuspendResult(t)

	cityDir := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfigLockTestCity), 0o644); err != nil {
		t.Fatal(err)
	}
	cs := newCityConfigLockControllerState(t, cityDir)

	inMutate := make(chan struct{})
	proceed := make(chan struct{})
	apiDone := make(chan error, 1)
	go func() {
		apiDone <- cs.mutateAndPoke(func() error {
			// An edit the refresh cannot load, so the controller must restore.
			if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
				return err
			}
			close(inMutate)
			<-proceed
			return nil
		})
	}()
	select {
	case <-inMutate:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("API mutation did not start")
	}

	var stdout, stderr bytes.Buffer
	cliDone := make(chan int, 1)
	go func() { cliDone <- doAgentSuspend(fsys.OSFS{}, cityDir, "worker", &stdout, &stderr) }()
	testutil.AwaitFlockWaiters(t, configedit.CityLockFile(cityDir), 1)
	close(proceed)

	if err := awaitErr(t, apiDone, "API mutation"); err == nil {
		t.Fatal("API mutation succeeded, want its refresh to fail")
	}
	select {
	case code := <-cliDone:
		if code != 0 {
			t.Fatalf("gc agent suspend = %d, stderr:\n%s", code, stderr.String())
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("gc agent suspend did not finish after the API mutation released the lock")
	}
	got, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != want {
		t.Fatalf("city.toml =\n%s\nwant the restored config plus only the CLI change:\n%s", got, want)
	}
}

// cliAgentSuspendResult returns the city.toml gc agent suspend writes for
// worker when it runs alone on cityConfigLockTestCity.
func cliAgentSuspendResult(t *testing.T) string {
	t.Helper()
	cityDir := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfigLockTestCity), 0o644); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doAgentSuspend(fsys.OSFS{}, cityDir, "worker", &stdout, &stderr); code != 0 {
		t.Fatalf("gc agent suspend on a pristine city = %d, stderr:\n%s", code, stderr.String())
	}
	data, err := os.ReadFile(filepath.Join(cityDir, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// cityConfigLockFree reports whether nothing holds the city config lock,
// probing it without waiting.
func cityConfigLockFree(t *testing.T, cityDir string) bool {
	t.Helper()
	path := configedit.CityLockFile(cityDir)
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("opening city config lock: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only probe
	lock, err := filelock.Acquire(f, filelock.Options{Mode: filelock.Exclusive, NonBlocking: true, Label: path})
	if errors.Is(err, filelock.ErrBusy) {
		return false
	}
	if err != nil {
		t.Fatalf("probing city config lock: %v", err)
	}
	if err := lock.Release(); err != nil {
		t.Fatalf("releasing city config lock probe: %v", err)
	}
	return true
}

// gc rig add holds the city config lock only through its config write: it is
// free again by the time the command asks the controller to reload, and a
// rig add that fails after taking it leaves it free.
func TestRigAddReleasesCityConfigLock(t *testing.T) {
	t.Run("before the controller reload", func(t *testing.T) {
		cityDir := filepath.Join(t.TempDir(), "city")
		writeSchema2RigCity(t, cityDir, "city1", "[workspace]\n", "")
		rigDir := filepath.Join(filepath.Dir(cityDir), "frontend")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("GC_DOLT", "skip")
		t.Setenv("GC_BEADS", "bd")
		oldReload := rigReloadControllerConfig
		t.Cleanup(func() { rigReloadControllerConfig = oldReload })
		reloaded, freeAtReload := false, false
		rigReloadControllerConfig = func(string) error {
			reloaded, freeAtReload = true, cityConfigLockFree(t, cityDir)
			return nil
		}

		var stdout, stderr bytes.Buffer
		if code := doRigAdd(fsys.OSFS{}, cityDir, rigDir, nil, "", "", "", false, false, &stdout, &stderr); code != 0 {
			t.Fatalf("gc rig add = %d, stderr:\n%s", code, stderr.String())
		}
		if !reloaded {
			t.Fatal("gc rig add never asked the controller to reload")
		}
		if !freeAtReload {
			t.Fatal("gc rig add still held the city config lock when it asked the controller to reload")
		}
	})
	t.Run("after a failure", func(t *testing.T) {
		cityDir := filepath.Join(t.TempDir(), "city")
		writeSchema2RigCity(t, cityDir, "city1", "[workspace\n", "")
		rigDir := filepath.Join(filepath.Dir(cityDir), "frontend")
		if err := os.MkdirAll(rigDir, 0o755); err != nil {
			t.Fatal(err)
		}

		var stdout, stderr bytes.Buffer
		if code := doRigAdd(fsys.OSFS{}, cityDir, rigDir, nil, "", "", "", false, false, &stdout, &stderr); code == 0 {
			t.Fatal("gc rig add succeeded on an unparseable city.toml")
		}
		if !cityConfigLockFree(t, cityDir) {
			t.Fatalf("gc rig add failed but still holds the city config lock; stderr:\n%s", stderr.String())
		}
	})
}

// requireFinishesPromptly fails the test when run does not return within the
// race timeout, so a call that waits on a lock fails instead of hanging.
func requireFinishesPromptly(t *testing.T, what string, run func() error) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatalf("%s waited on a lock it must not take", what)
	}
}

func exitCodeErr(code int, stderr *bytes.Buffer) error {
	if code != 0 {
		return fmt.Errorf("exit %d, stderr:\n%s", code, stderr.String())
	}
	return nil
}

// The city config lock and the suspension fence guard different files: a
// config edit does not wait on the fence, and the gc rig suspend fallback,
// which writes only suspension state, does not wait on the city config lock.
func TestCityConfigLockAndSuspensionFenceAreIndependent(t *testing.T) {
	t.Run("config edits with the suspension fence held", func(t *testing.T) {
		apiCity := filepath.Join(t.TempDir(), "api-city")
		if err := os.MkdirAll(apiCity, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(apiCity, "city.toml"), []byte(cityConfigLockTestCity), 0o644); err != nil {
			t.Fatal(err)
		}
		cliCity := filepath.Join(t.TempDir(), "cli-city")
		writeSchema2RigCity(t, cliCity, "city1", "[workspace]\n", "")
		for _, cityDir := range []string{apiCity, cliCity} {
			releaseFence, err := suspensionstate.Fence(cityDir)
			if err != nil {
				t.Fatalf("taking suspension fence: %v", err)
			}
			t.Cleanup(releaseFence)
		}

		cs := newCityConfigLockControllerState(t, apiCity)
		requireFinishesPromptly(t, "API agent config edit", func() error { return cs.SuspendAgent("worker") })
		var stdout, stderr bytes.Buffer
		requireFinishesPromptly(t, "gc agent add", func() error {
			return exitCodeErr(doAgentAdd(fsys.OSFS{}, cliCity, "helper", "", "", false, &stdout, &stderr), &stderr)
		})
	})
	t.Run("gc rig suspend fallback with the city config lock held", func(t *testing.T) {
		cityDir := filepath.Join(t.TempDir(), "city")
		siteToml := "workspace_name = \"city1\"\n\n[[rig]]\nname = \"frontend\"\npath = \"/some/path\"\n"
		writeSchema2RigCity(t, cityDir, "city1", "[workspace]\n\n[[rigs]]\nname = \"frontend\"\n", siteToml)
		holdCityConfigLock(t, cityDir)

		var stdout, stderr bytes.Buffer
		requireFinishesPromptly(t, "gc rig suspend", func() error {
			return exitCodeErr(doRigSuspend(fsys.OSFS{}, cityDir, "frontend", &stdout, &stderr), &stderr)
		})
		st, err := suspensionstate.Load(fsys.OSFS{}, cityDir)
		if err != nil {
			t.Fatal(err)
		}
		if !suspensionstate.IsRigSuspended(st, "frontend") {
			t.Fatalf("rig frontend is not suspended: %+v", st)
		}
	})
}

// readCityFileTree returns every regular file under cityDir keyed by its
// path relative to cityDir.
func readCityFileTree(t *testing.T, cityDir string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(cityDir, func(path string, d os.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(cityDir, path)
		if err != nil {
			return err
		}
		files[rel] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("snapshotting %s: %v", cityDir, err)
	}
	return files
}

func requireCityFilesUnchanged(t *testing.T, cityDir string, before map[string]string) {
	t.Helper()
	after := readCityFileTree(t, cityDir)
	for rel, data := range before {
		if got, ok := after[rel]; !ok || got != data {
			t.Errorf("%s changed:\nbefore:\n%s\nafter (present=%v):\n%s", rel, data, ok, got)
		}
	}
	for rel := range after {
		if _, ok := before[rel]; !ok {
			t.Errorf("%s was created", rel)
		}
	}
}

// When the city config lock file cannot be created, a config edit fails with
// an error naming the lock file and leaves every config file as it was.
func TestCityConfigEditFailsWhenLockFileCannotBeCreated(t *testing.T) {
	newCity := func(t *testing.T) string {
		t.Helper()
		cityDir := filepath.Join(t.TempDir(), "city")
		if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfigLockTestCity), 0o644); err != nil {
			t.Fatal(err)
		}
		return cityDir
	}
	blockLockDir := func(t *testing.T, cityDir string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(cityDir, ".gc", "runtime"), []byte("not a directory\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("API mutation", func(t *testing.T) {
		cityDir := newCity(t)
		cs := newCityConfigLockControllerState(t, cityDir)
		blockLockDir(t, cityDir)
		before := readCityFileTree(t, cityDir)

		err := cs.SuspendAgent("worker")
		if err == nil {
			t.Fatal("SuspendAgent succeeded without the city config lock")
		}
		if !strings.Contains(err.Error(), configedit.CityLockFile(cityDir)) {
			t.Fatalf("SuspendAgent error = %v, want it to name %s", err, configedit.CityLockFile(cityDir))
		}
		requireCityFilesUnchanged(t, cityDir, before)
	})
	t.Run("CLI writer", func(t *testing.T) {
		cityDir := newCity(t)
		blockLockDir(t, cityDir)
		before := readCityFileTree(t, cityDir)

		var stdout, stderr bytes.Buffer
		if code := doAgentSuspend(fsys.OSFS{}, cityDir, "worker", &stdout, &stderr); code != 1 {
			t.Fatalf("gc agent suspend = %d, want 1; stderr:\n%s", code, stderr.String())
		}
		if !strings.Contains(stderr.String(), configedit.CityLockFile(cityDir)) {
			t.Fatalf("gc agent suspend stderr = %q, want it to name %s", stderr.String(), configedit.CityLockFile(cityDir))
		}
		requireCityFilesUnchanged(t, cityDir, before)
	})
}

// A CLI config writer that finds the city config lock held says so once on
// stderr, naming the lock file; one that finds it free says nothing.
func TestCLICityConfigWriterWaitNotice(t *testing.T) {
	const notice = "waiting for another config edit of this city to finish"
	newCity := func(t *testing.T) string {
		t.Helper()
		cityDir := filepath.Join(t.TempDir(), "city")
		if err := os.MkdirAll(cityDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfigLockTestCity), 0o644); err != nil {
			t.Fatal(err)
		}
		return cityDir
	}

	t.Run("lock held", func(t *testing.T) {
		cityDir := newCity(t)
		release := holdCityConfigLock(t, cityDir)
		var stdout, stderr bytes.Buffer
		done := make(chan int, 1)
		go func() { done <- doAgentSuspend(fsys.OSFS{}, cityDir, "worker", &stdout, &stderr) }()
		testutil.AwaitFlockWaiters(t, configedit.CityLockFile(cityDir), 1)
		release()
		select {
		case code := <-done:
			if code != 0 {
				t.Fatalf("gc agent suspend = %d, stderr:\n%s", code, stderr.String())
			}
		case <-time.After(testutil.GoroutineRaceTimeout):
			t.Fatal("gc agent suspend did not finish after the city config lock was released")
		}
		want := "gc agent suspend: " + notice + " (" + configedit.CityLockFile(cityDir) + ")\n"
		if got := strings.Count(stderr.String(), notice); got != 1 {
			t.Fatalf("stderr has %d wait notices, want 1:\n%s", got, stderr.String())
		}
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want the notice %q", stderr.String(), want)
		}
	})
	t.Run("lock free", func(t *testing.T) {
		cityDir := newCity(t)
		var stdout, stderr bytes.Buffer
		if code := doAgentSuspend(fsys.OSFS{}, cityDir, "worker", &stdout, &stderr); code != 0 {
			t.Fatalf("gc agent suspend = %d, stderr:\n%s", code, stderr.String())
		}
		if strings.Contains(stderr.String(), notice) {
			t.Fatalf("stderr has a wait notice with the lock free:\n%s", stderr.String())
		}
	})
}

// gc storage split's two city.toml writes, P4's [storage] append and
// --abandon's removal, hold the city config lock from their read of city.toml
// through their write: each waits for another process's edit, and --abandon
// keeps that edit.
func TestStorageSplitCityTOMLWritesWaitForCityConfigLock(t *testing.T) {
	t.Run("P4 configure", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		request := c.reload()
		release := holdCityConfigLock(t, c.request.CityPath)
		var stdout, stderr bytes.Buffer
		done := make(chan int, 1)
		go func() { done <- doStorageSplit(context.Background(), request, splitOptions{}, &stdout, &stderr) }()
		testutil.AwaitFlockWaiters(t, configedit.CityLockFile(c.request.CityPath), 1)
		release()
		if code := awaitExitCode(t, done, "gc storage split"); code != 0 {
			t.Fatalf("gc storage split = %d:\n%s\n%s", code, stdout.String(), stderr.String())
		}
		c.assertSplit()
	})
	t.Run("--abandon", func(t *testing.T) {
		c := newGuidedSplitCity(t)
		c.killSplitAt("copying rows")
		request := c.reload()
		release := holdCityConfigLock(t, c.request.CityPath)
		var stdout, stderr bytes.Buffer
		done := make(chan int, 1)
		go func() {
			done <- doStorageSplit(context.Background(), request, splitOptions{Abandon: true}, &stdout, &stderr)
		}()
		testutil.AwaitFlockWaiters(t, configedit.CityLockFile(c.request.CityPath), 1)
		appendToCityToml(t, c.request.CityPath, otherProcessDaemonEdit)
		release()
		if code := awaitExitCode(t, done, "gc storage split --abandon"); code != 0 {
			t.Fatalf("gc storage split --abandon = %d:\n%s\n%s", code, stdout.String(), stderr.String())
		}
		if got := c.readTOML(); strings.Contains(got, "[storage") {
			t.Fatalf("city.toml after --abandon still declares [storage]:\n%s", got)
		}
		requireOtherProcessDaemonEdit(t, c.request.CityPath)
	})
}

// doctorConfigFixChecks are the gc doctor checks whose --fix repair rewrites
// the city's config files, built for cityDir.
func doctorConfigFixChecks(cityDir string) []doctor.Check {
	return []doctor.Check{
		newProviderCatalogDoctorCheck(cityDir),
		newBuiltinImportDoctorCheck(cityDir),
		newImportStateDoctorCheck(cityDir),
		newGascityPackBindingDoctorCheck(cityDir),
		v2AgentFormatCheck{},
		v2ImportFormatCheck{},
		v2DefaultRigImportFormatCheck{},
		v2PackSourcesCheck{},
		v2RigPathSiteBindingCheck{},
		v2WorkspaceNameCheck{},
	}
}

// A gc doctor --fix repair never waits for the city config lock: gc doctor
// abandons a fix that outlasts its check timeout and may exit while the fix
// still runs. A repair that finds another config edit of the city in progress
// changes nothing and fails at once, naming the lock file and hinting to
// re-run gc doctor --fix.
func TestDoctorConfigFixesFailAtOnceWhileCityConfigLockHeld(t *testing.T) {
	cityDir := filepath.Join(t.TempDir(), "city")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityConfigLockTestCity), 0o644); err != nil {
		t.Fatal(err)
	}
	holdCityConfigLock(t, cityDir)
	before := readCityFileTree(t, cityDir)
	for _, check := range doctorConfigFixChecks(cityDir) {
		t.Run(check.Name(), func(t *testing.T) {
			done := make(chan error, 1)
			go func() { done <- check.Fix(&doctor.CheckContext{CityPath: cityDir, Output: io.Discard}) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Fatal("the fix waited for the city config lock")
			}
			var hinted *doctor.FixHintError
			if !errors.As(err, &hinted) || !errors.Is(err, configedit.ErrCityLocked) {
				t.Fatalf("fix with the city config lock held = %v, want a *doctor.FixHintError wrapping configedit.ErrCityLocked", err)
			}
			if !strings.Contains(err.Error(), configedit.CityLockFile(cityDir)) {
				t.Errorf("fix error %q does not name %s", err, configedit.CityLockFile(cityDir))
			}
			if !strings.Contains(hinted.Hint, "gc doctor --fix") {
				t.Errorf("fix hint %q does not say to re-run gc doctor --fix", hinted.Hint)
			}
			requireCityFilesUnchanged(t, cityDir, before)
		})
	}
}

// A plain API edit and a config transaction (a pack import) serialize on the
// city config lock: a pack import started while a plain edit is paused waits
// for the edit there, holding none of the in-process config locks, so the two
// never interleave their publication and Editor locks, and both finish once
// the edit goes on.
func TestControllerStatePlainEditFinishesBesideConfigTransaction(t *testing.T) {
	cs, tomlPath := newControllerStateMutationHarness(t)
	editEntered := make(chan struct{})
	releaseEdit := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseEdit) }) }
	defer release()
	editDone := make(chan error, 1)
	go func() {
		editDone <- cs.mutateAndPoke(func() error {
			close(editEntered)
			<-releaseEdit
			return cs.editor.SuspendAgent("rig1/worker")
		})
	}()
	awaitClose(t, editEntered, "plain edit holding the city config and publication locks")

	txDone := make(chan error, 1)
	go func() { txDone <- cs.SerializeConfigWrite(func() error { return nil }) }()
	testutil.AwaitFlockWaiters(t, configedit.CityLockFile(cs.cityPath), 1)
	if !cs.configTransactionMu.TryLock() {
		t.Fatal("the pack import holds the config transaction fence while it waits for the city config lock")
	}
	cs.configTransactionMu.Unlock()

	release()
	for _, w := range []struct {
		name string
		done <-chan error
	}{{"plain edit", editDone}, {"pack import", txDone}} {
		select {
		case err := <-w.done:
			if err != nil {
				t.Fatalf("%s: %v", w.name, err)
			}
		case <-time.After(hangBudget):
			t.Fatalf("%s did not finish after the plain edit went on", w.name)
		}
	}
	cfg, err := config.Load(fsys.OSFS{}, tomlPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Agents) != 1 || !cfg.Agents[0].Suspended {
		t.Fatalf("agents after the edit = %+v, want rig1/worker suspended", cfg.Agents)
	}
}
