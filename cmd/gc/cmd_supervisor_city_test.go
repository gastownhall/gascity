package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

//nolint:unparam // tests override hook behavior but keep fixed timeout/poll values for determinism
func withSupervisorTestHooks(t *testing.T, ensure func(stdout, stderr io.Writer) int, reload func(stdout, stderr io.Writer) int, alive func() int, running func(string) (bool, string, bool), timeout, poll time.Duration) {
	t.Helper()

	oldEnsure := ensureSupervisorRunningHook
	oldReload := reloadSupervisorHook
	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	oldRegister := registerCityWithSupervisorTestHook
	oldTimeout := supervisorCityReadyTimeout
	oldPoll := supervisorCityPollInterval

	ensureSupervisorRunningHook = ensure
	reloadSupervisorHook = reload
	supervisorAliveHook = alive
	supervisorCityRunningHook = running
	registerCityWithSupervisorTestHook = nil
	supervisorCityReadyTimeout = timeout
	supervisorCityPollInterval = poll

	t.Cleanup(func() {
		ensureSupervisorRunningHook = oldEnsure
		reloadSupervisorHook = oldReload
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
		registerCityWithSupervisorTestHook = oldRegister
		supervisorCityReadyTimeout = oldTimeout
		supervisorCityPollInterval = oldPoll
	})
}

func TestRegisterCityWithSupervisorRollsBackWhenCityNeverBecomesReady(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n[session]\nstartup_timeout = \"20ms\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reloads := 0
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int {
			reloads++
			return 0
		},
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", true },
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisor(cityPath, &stdout, &stderr, "gc register", true)
	// The command reports failure (exit code 1) when the city doesn't start,
	// but keeps the registration so the supervisor can retry automatically.
	if code != 1 {
		t.Fatalf("registerCityWithSupervisor code = %d, want 1", code)
	}
	// The key behavioral change: registration is KEPT, not rolled back.
	if strings.Contains(stderr.String(), "registration rolled back") {
		t.Fatalf("stderr = %q, should NOT contain rollback message (registration should be kept)", stderr.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry (kept for retry), got %d", len(entries))
	}
}

func TestRegisterCityWithSupervisorWaitsForConfiguredStartupTimeout(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n[session]\nstartup_timeout = \"200ms\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	startedAt := time.Now().Add(75 * time.Millisecond)
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) {
			return time.Now().After(startedAt), "", true
		},
		20*time.Millisecond,
		5*time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisor(cityPath, &stdout, &stderr, "gc register", true)
	if code != 0 {
		t.Fatalf("registerCityWithSupervisor code = %d, want 0: %s", code, stderr.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	// Registry.Register resolves symlinks (e.g. /var → /private/var on macOS),
	// so compare against the resolved path.
	resolvedCityPath, _ := filepath.EvalSymlinks(cityPath)
	if len(entries) != 1 || entries[0].Path != resolvedCityPath {
		t.Fatalf("expected retained registry entry for %s, got %v", resolvedCityPath, entries)
	}
}

func TestRegisterCityWithSupervisorFetchesRemotePacksBeforeLoadingIncludes(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	remote := initBarePackRepo(t, "remote-pack")
	configText := strings.Join([]string{
		"[workspace]",
		`name = "bright-lights"`,
		`includes = ["remote-pack"]`,
		"",
		"[packs.remote-pack]",
		`source = "` + remote + `"`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(configText), 0o644); err != nil {
		t.Fatal(err)
	}

	// Before packs are fetched, the pack is not found and silently skipped.
	// effectiveCityName still succeeds (missing packs are non-fatal) but
	// the pack's agents/config won't be loaded until after fetch.
	name, err := effectiveCityName(cityPath)
	if err != nil {
		t.Fatalf("effectiveCityName should succeed even with missing pack: %v", err)
	}
	if name != "bright-lights" {
		t.Fatalf("expected name %q, got %q", "bright-lights", name)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 0 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisor(cityPath, &stdout, &stderr, "gc register", true)
	if code != 0 {
		t.Fatalf("registerCityWithSupervisor code = %d, want 0: %s", code, stderr.String())
	}

	cacheDir := config.PackCachePath(cityPath, "remote-pack", config.PackSource{Source: remote})
	if _, err := os.Stat(filepath.Join(cacheDir, "pack.toml")); err != nil {
		t.Fatalf("expected fetched pack cache at %s: %v", cacheDir, err)
	}
}

func TestRegisterCityWithSupervisorRejectsStandaloneController(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	root, err := os.MkdirTemp("", "gc-ctl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) }) //nolint:errcheck

	cityPath := filepath.Join(root, "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck // test cleanup

	go func() {
		conn, acceptErr := lis.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close() //nolint:errcheck // test cleanup
		buf := make([]byte, 32)
		n, _ := conn.Read(buf)
		if strings.Contains(string(buf[:n]), "ping") {
			conn.Write([]byte("4242\n")) //nolint:errcheck // best-effort reply
		}
	}()

	var stdout, stderr bytes.Buffer
	code := registerCityWithSupervisor(cityPath, &stdout, &stderr, "gc register", true)
	if code != 1 {
		t.Fatalf("registerCityWithSupervisor code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "standalone controller already running") {
		t.Fatalf("stderr = %q, want standalone-controller error", stderr.String())
	}
	if !strings.Contains(stderr.String(), "PID 4242") {
		t.Fatalf("stderr = %q, want controller PID", stderr.String())
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected empty registry after standalone-controller rejection, got %v", entries)
	}
}

func TestUnregisterCityFromSupervisorRestoresRegistrationOnReloadFailure(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 1 },
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	var stdout, stderr bytes.Buffer
	handled, code := unregisterCityFromSupervisor(cityPath, &stdout, &stderr, "gc unregister")
	if !handled || code != 1 {
		t.Fatalf("unregisterCityFromSupervisor = (%t, %d), want (true, 1)", handled, code)
	}
	if !strings.Contains(stderr.String(), "restored registration") {
		t.Fatalf("stderr = %q, want restore message", stderr.String())
	}

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	// Registry.Register resolves symlinks (e.g. /var → /private/var on macOS),
	// so compare against the resolved path.
	resolvedCityPath, _ := filepath.EvalSymlinks(cityPath)
	if len(entries) != 1 || entries[0].Path != resolvedCityPath {
		t.Fatalf("expected restored registry entry for %s, got %v", resolvedCityPath, entries)
	}
}

func TestControllerStatusForSupervisorManagedCityStopped(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4242 }
	supervisorCityRunningHook = func(string) (bool, string, bool) { return false, "", true }
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})

	ctrl := controllerStatusForCity(cityPath)
	if ctrl.Running || ctrl.PID != 4242 || ctrl.Mode != "supervisor" {
		t.Fatalf("controller status = %+v, want stopped supervisor PID", ctrl)
	}
}

func TestControllerStatusForSupervisorManagedCityPreservesInitStatus(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4242 }
	supervisorCityRunningHook = func(string) (bool, string, bool) { return false, "starting_bead_store", true }
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})

	ctrl := controllerStatusForCity(cityPath)
	if ctrl.Running || ctrl.PID != 4242 || ctrl.Mode != "supervisor" || ctrl.Status != "starting_bead_store" {
		t.Fatalf("controller status = %+v, want init-progress supervisor PID", ctrl)
	}
}

func TestCmdStopSupervisorManagedCityReliesOnSupervisorCleanup(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	root, err := os.MkdirTemp("", "gcstop-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) }) //nolint:errcheck

	cityPath := filepath.Join(root, "city")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "bright-lights"); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int {
			if err := shutdownBeadsProvider(cityPath); err != nil {
				t.Fatalf("shutdownBeadsProvider: %v", err)
			}
			return 0
		},
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck

	stopped := make(chan struct{}, 1)
	go func() {
		conn, acceptErr := lis.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close() //nolint:errcheck
		buf := make([]byte, 32)
		n, _ := conn.Read(buf)
		if strings.Contains(string(buf[:n]), "stop") {
			stopped <- struct{}{}
		}
		conn.Write([]byte("ok\n")) //nolint:errcheck
	}()

	var stdout, stderr bytes.Buffer
	code := cmdStop([]string{cityPath}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdStop code = %d, want 0: %s", code, stderr.String())
	}
	select {
	case <-stopped:
		t.Fatal("did not expect a legacy controller stop request for a supervisor-managed city")
	case <-time.After(100 * time.Millisecond):
	}

	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected city to be unregistered after stop, got %v", entries)
	}

	ops := readOpLog(t, logFile)
	if len(ops) != 1 {
		t.Fatalf("expected bead provider stop, got %v", ops)
	}
	if !strings.HasPrefix(ops[0], "stop") {
		t.Fatalf("unexpected bead provider op: %v", ops)
	}
}

func TestReconcileCitiesNameDriftStopsBeadsProvider(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	root, err := os.MkdirTemp("", "gc-drift-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) }) //nolint:errcheck

	cityPath := filepath.Join(root, "city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "new-name"); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultCity("old-name")
	sp := runtime.NewFake()
	var cityOut, cityErr bytes.Buffer
	cr := newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "old-name",
		Cfg:      &cfg,
		SP:       sp,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) map[string]TemplateParams {
			return nil
		},
		Rec:    events.Discard,
		Stdout: &cityOut,
		Stderr: &cityErr,
	})

	done := make(chan struct{})
	close(done)
	registry := newCityRegistry()
	registry.Add(cityPath, &managedCity{
		cr:      cr,
		name:    "old-name",
		started: true,
		cancel:  func() {},
		done:    done,
	})
	var stdout, stderr bytes.Buffer

	reconcileCities(reg, registry, supervisor.PublicationConfig{}, &stdout, &stderr)

	ops := readOpLog(t, logFile)
	if len(ops) != 1 {
		t.Fatalf("expected bead provider stop during name-drift restart, got %v", ops)
	}
	if !strings.HasPrefix(ops[0], "stop") {
		t.Fatalf("unexpected bead provider op: %v", ops)
	}
}

func TestSupervisorCreatesControllerSocketForManagedCity(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"),
		[]byte("[workspace]\nname = \"test-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "test-city"); err != nil {
		t.Fatal(err)
	}

	cr := newCityRegistry()
	var stdout, stderr bytes.Buffer
	reconcileCities(reg, cr, supervisor.PublicationConfig{}, &stdout, &stderr)

	sockPath := filepath.Join(cityPath, ".gc", "controller.sock")
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("controller.sock not created at %s after reconcileCities: %v", sockPath, err)
	}

	if pid := controllerAlive(cityPath); pid == 0 {
		t.Fatal("controller socket exists but does not respond to ping")
	}

	// Verify convergence commands are routed through the event loop.
	// An unknown command returns a domain error rather than the "no bead store"
	// sentinel, proving the full socket → event-loop → handler path is wired.
	reply, err := sendConvergenceRequest(cityPath, convergenceRequest{
		Command: "list", // not a valid command; exercises the handler dispatch path
	})
	if err != nil {
		t.Fatalf("sendConvergenceRequest: %v", err)
	}
	if strings.Contains(reply.Error, "convergence not available") {
		t.Fatalf("convergence event loop wired but convHandler is nil; got: %q", reply.Error)
	}
	if !strings.Contains(reply.Error, "unknown convergence command") {
		t.Fatalf("expected 'unknown convergence command' error, got: %q", reply.Error)
	}

	// Cleanup: cancel the city goroutine and wait for it to exit.
	if done := cr.CancelCity(cityPath); done != nil {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("city goroutine did not exit in time")
		}
	}
}

var testGitEnvBlacklist = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
}

func initBarePackRepo(t *testing.T, name string) string {
	t.Helper()

	root := t.TempDir()
	workDir := filepath.Join(root, "work")
	bareDir := filepath.Join(root, name+".git")

	mustGit(t, "", "init", workDir)
	if err := os.MkdirAll(filepath.Join(workDir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	packToml := strings.Join([]string{
		"[pack]",
		`name = "` + name + `"`,
		`version = "1.0.0"`,
		"schema = 1",
		"",
		"[[agent]]",
		`name = "worker"`,
		"",
	}, "\n")
	if err := os.WriteFile(filepath.Join(workDir, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "prompts", "worker.md"), []byte("you are a worker"), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "-A")
	mustGit(t, workDir, "commit", "-m", "initial")
	mustGit(t, workDir, "clone", "--bare", workDir, bareDir)
	return bareDir
}

func mustGit(t *testing.T, dir string, args ...string) {
	t.Helper()

	fullArgs := append([]string{"-c", "core.hooksPath="}, args...)
	cmd := exec.Command("git", fullArgs...)
	if dir != "" {
		cmd.Dir = dir
	}
	for _, env := range os.Environ() {
		if key, _, ok := strings.Cut(env, "="); ok && testGitEnvBlacklist[key] {
			continue
		}
		cmd.Env = append(cmd.Env, env)
	}
	cmd.Env = append(cmd.Env,
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com",
	)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %s: %v", strings.Join(args, " "), string(out), err)
	}
}

func TestWaitForSupervisorCityPrintsStatusChanges(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	statuses := []string{"loading_config", "starting_bead_store", "resolving_formulas", "starting_agents"}
	callIdx := 0
	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) {
			if callIdx >= len(statuses) {
				return true, "", true
			}
			s := statuses[callIdx]
			callIdx++
			return false, s, true
		},
		2*time.Second,
		time.Millisecond,
	)

	var stdout bytes.Buffer
	err := waitForSupervisorCity("/fake/city", true, 2*time.Second, &stdout)
	if err != nil {
		t.Fatalf("waitForSupervisorCity error = %v", err)
	}
	out := stdout.String()
	for _, expected := range []string{
		"Loading configuration...",
		"Starting bead store...",
		"Resolving formulas...",
		"Starting agents...",
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("stdout = %q, want %q", out, expected)
		}
	}
}

func TestListCitiesIncludesInitStatus(t *testing.T) {
	reg := newCityRegistry()
	reg.Add("/running", &managedCity{
		name:    "running-city",
		started: true,
		cr:      &CityRuntime{cityName: "running-city"},
	})
	// Add init status via BatchUpdate (city not in main map yet).
	reg.BatchUpdate(func(
		_ map[string]*managedCity,
		initStatus map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		initStatus["/loading"] = cityInitProgress{name: "loading-city", status: "starting_bead_store"}
	})

	list := reg.ListCities()
	if len(list) != 2 {
		t.Fatalf("ListCities() returned %d cities, want 2", len(list))
	}

	found := map[string]bool{}
	for _, ci := range list {
		found[ci.Name] = true
		switch ci.Name {
		case "running-city":
			if !ci.Running {
				t.Error("running-city should be Running=true")
			}
			if ci.Status != "" {
				t.Errorf("running-city Status = %q, want empty", ci.Status)
			}
		case "loading-city":
			if ci.Running {
				t.Error("loading-city should be Running=false")
			}
			if ci.Status != "starting_bead_store" {
				t.Errorf("loading-city Status = %q, want starting_bead_store", ci.Status)
			}
		}
	}
	if !found["running-city"] || !found["loading-city"] {
		t.Fatalf("missing expected cities in %v", list)
	}
}
