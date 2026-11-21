package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
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
