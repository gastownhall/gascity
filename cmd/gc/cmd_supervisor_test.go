package main

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

type closerSpy struct {
	closed bool
}

func (c *closerSpy) Close() error {
	c.closed = true
	return nil
}

func startTestSupervisorSocket(t *testing.T, sockPath string, handler func(string) string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("MkdirAll(%q): %v", filepath.Dir(sockPath), err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen(unix, %q): %v", sockPath, err)
	}
	t.Cleanup(func() {
		lis.Close()         //nolint:errcheck
		os.Remove(sockPath) //nolint:errcheck
	})

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close() //nolint:errcheck
				buf := make([]byte, 64)
				n, err := conn.Read(buf)
				if err != nil || n == 0 {
					return
				}
				resp := handler(strings.TrimSpace(string(buf[:n])))
				if resp != "" {
					io.WriteString(conn, resp) //nolint:errcheck
				}
			}(conn)
		}
	}()
}

func shortTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", prefix)
	if err != nil {
		t.Fatalf("MkdirTemp(/tmp, %q): %v", prefix, err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) }) //nolint:errcheck
	return dir
}

func TestDoSupervisorLogsNoFile(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := doSupervisorLogs(50, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSupervisorLogs code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "log file not found") {
		t.Fatalf("stderr = %q, want missing log file message", stderr.String())
	}
}

func TestSupervisorAliveFallsBackToDefaultHomeSocket(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		if cmd == "ping" {
			return "4242\n"
		}
		return ""
	})

	gotPath, gotPID := runningSupervisorSocket()
	if gotPath != sockPath {
		t.Fatalf("runningSupervisorSocket path = %q, want %q", gotPath, sockPath)
	}
	if gotPID != 4242 {
		t.Fatalf("runningSupervisorSocket pid = %d, want 4242", gotPID)
	}
	if pid := supervisorAlive(); pid != 4242 {
		t.Fatalf("supervisorAlive() = %d, want 4242", pid)
	}
}

func TestReloadSupervisorFallsBackToDefaultHomeSocket(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		switch cmd {
		case "ping":
			return "4242\n"
		case "reload":
			return "ok\n"
		default:
			return ""
		}
	})

	var stdout, stderr bytes.Buffer
	if code := reloadSupervisor(&stdout, &stderr); code != 0 {
		t.Fatalf("reloadSupervisor code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Reconciliation triggered.") {
		t.Fatalf("stdout = %q, want reload confirmation", stdout.String())
	}
}

func TestRenderSupervisorLaunchdTemplate(t *testing.T) {
	data := &supervisorServiceData{
		GCPath:  "/usr/local/bin/gc",
		LogPath: "/home/user/.gc/supervisor.log",
		GCHome:  "/home/user/.gc",
		Path:    "/usr/local/bin:/usr/bin:/bin",
	}

	content, err := renderSupervisorTemplate(supervisorLaunchdTemplate, data)
	if err != nil {
		t.Fatal(err)
	}

	for _, check := range []string{
		"com.gascity.supervisor",
		"/usr/local/bin/gc",
		"supervisor",
		"run",
		"/home/user/.gc/supervisor.log",
		"GC_HOME",
		"<key>PATH</key>",
	} {
		if !strings.Contains(content, check) {
			t.Fatalf("launchd template missing %q", check)
		}
	}
}

func TestRenderSupervisorSystemdTemplate(t *testing.T) {
	data := &supervisorServiceData{
		GCPath:  "/usr/local/bin/gc",
		LogPath: "/home/user/.gc/supervisor.log",
		GCHome:  "/home/user/.gc",
		Path:    "/usr/local/bin:/usr/bin:/bin",
	}

	content, err := renderSupervisorTemplate(supervisorSystemdTemplate, data)
	if err != nil {
		t.Fatal(err)
	}

	for _, check := range []string{
		"[Service]",
		`ExecStart=/usr/local/bin/gc supervisor run`,
		`StandardOutput=append:/home/user/.gc/supervisor.log`,
		`Environment=GC_HOME="/home/user/.gc"`,
		`Environment=PATH="/usr/local/bin:/usr/bin:/bin"`,
	} {
		if !strings.Contains(content, check) {
			t.Fatalf("systemd template missing %q", check)
		}
	}
}

func TestBuildSupervisorServiceDataExpandsUserManagedPath(t *testing.T) {
	homeDir := t.TempDir()
	nvmBin := filepath.Join(homeDir, ".nvm", "versions", "node", "v22.14.0", "bin")
	if err := os.MkdirAll(nvmBin, 0o755); err != nil {
		t.Fatalf("mkdir nvm bin: %v", err)
	}
	t.Setenv("HOME", homeDir)
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	if !slices.Contains(filepath.SplitList(data.Path), nvmBin) {
		t.Fatalf("buildSupervisorServiceData PATH %q missing nvm bin %q", data.Path, nvmBin)
	}
}

func TestSupervisorInstallUnsupportedOS(t *testing.T) {
	if goruntime.GOOS == "darwin" || goruntime.GOOS == "linux" {
		t.Skip("unsupported-os test only applies outside darwin/linux")
	}
	t.Setenv("GC_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	code := doSupervisorInstall(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSupervisorInstall code = %d, want 1", code)
	}
}

func TestDoSupervisorStartAlreadyRunning(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	lock, err := acquireSupervisorLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close() //nolint:errcheck // test cleanup

	var stdout, stderr bytes.Buffer
	code := doSupervisorStart(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("stderr = %q, want already running message", stderr.String())
	}
}

func TestDoSupervisorStartDetectsSupervisorOnFallbackSocket(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		if cmd == "ping" {
			return "4242\n"
		}
		return ""
	})

	var stdout, stderr bytes.Buffer
	code := doSupervisorStart(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("stderr = %q, want already running message", stderr.String())
	}
}

func TestRunSupervisorRejectsSupervisorOnFallbackSocket(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		if cmd == "ping" {
			return "4242\n"
		}
		return ""
	})

	var stdout, stderr bytes.Buffer
	code := runSupervisor(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("runSupervisor code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "already running") {
		t.Fatalf("stderr = %q, want already running message", stderr.String())
	}
}

func TestRunSupervisorFailsWhenAPIPortUnavailable(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer lis.Close() //nolint:errcheck

	port := lis.Addr().(*net.TCPAddr).Port
	cfg := []byte("[supervisor]\nport = " + strconv.Itoa(port) + "\n")
	if err := os.WriteFile(supervisor.ConfigPath(), cfg, 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runSupervisor(&stdout, &stderr)
	if code != 1 {
		t.Fatalf("runSupervisor code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "api: listen") {
		t.Fatalf("stderr = %q, want API listen failure", stderr.String())
	}
}

func TestControllerStatusForSupervisorManagedCity(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	dir := t.TempDir()
	cityPath := filepath.Join(dir, "bright-lights")
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
	supervisorCityRunningHook = func(string) (bool, string, bool) { return true, "", true }
	defer func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	}()

	ctrl := controllerStatusForCity(cityPath)
	if !ctrl.Running || ctrl.PID != 4242 || ctrl.Mode != "supervisor" {
		t.Fatalf("controller status = %+v, want running supervisor PID", ctrl)
	}
}

func TestSupervisorCityAPIClientRequiresRunning(t *testing.T) {
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

	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4242 }
	supervisorCityRunningHook = func(string) (bool, string, bool) { return false, "", true }
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})

	if client := supervisorCityAPIClient(cityPath); client != nil {
		t.Fatalf("supervisorCityAPIClient(%q) = %#v, want nil for stopped city", cityPath, client)
	}
}

func TestPrepareCityForSupervisorEnsuresInitArtifacts(t *testing.T) {
	configureIsolatedRuntimeEnv(t)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := ensureCityScaffold(cityPath); err != nil {
		t.Fatal(err)
	}

	cfg := config.DefaultCity("bright-lights")
	content, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), content, 0o644); err != nil {
		t.Fatal(err)
	}

	initFormula := filepath.Join(cityPath, citylayout.FormulasRoot, "mol-do-work.formula.toml")
	if _, err := os.Stat(initFormula); !os.IsNotExist(err) {
		t.Fatalf("init formula should not exist before supervisor prep, err=%v", err)
	}

	var stderr bytes.Buffer
	if err := prepareCityForSupervisor(cityPath, "bright-lights", &cfg, &stderr, nil); err != nil {
		t.Fatalf("prepareCityForSupervisor() error: %v; stderr=%s", err, stderr.String())
	}

	if _, err := os.Stat(initFormula); err != nil {
		t.Fatalf("init formula missing after supervisor prep: %v", err)
	}
}

func TestCityRegistryReportsRunningOnlyAfterStartup(t *testing.T) {
	cs := &controllerState{}
	mc := &managedCity{
		cr:     &CityRuntime{cityName: "bright-lights", cs: cs},
		name:   "bright-lights",
		status: "adopting_sessions",
	}
	reg := newCityRegistry()
	reg.Add("/city", mc)

	cities := reg.ListCities()
	if len(cities) != 1 || cities[0].Running {
		t.Fatalf("ListCities before startup = %+v, want one stopped city", cities)
	}
	if cities[0].Status != "adopting_sessions" {
		t.Fatalf("ListCities before startup Status = %q, want adopting_sessions", cities[0].Status)
	}
	if got := reg.CityState("bright-lights"); got != nil {
		t.Fatalf("CityState before startup = %#v, want nil", got)
	}

	reg.UpdateCallback("/city", func(m *managedCity) {
		m.started = true
	})
	cities = reg.ListCities()
	if len(cities) != 1 || !cities[0].Running {
		t.Fatalf("ListCities after startup = %+v, want one running city", cities)
	}
	if cities[0].Status != "" {
		t.Fatalf("ListCities after startup Status = %q, want empty", cities[0].Status)
	}
	if got := reg.CityState("bright-lights"); got != cs {
		t.Fatalf("CityState after startup = %#v, want controller state", got)
	}
}

func TestControllerAliveNoSocket(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := controllerAlive(dir); got != 0 {
		t.Fatalf("controllerAlive = %d, want 0", got)
	}
}

func TestStartHiddenLegacyFlags(t *testing.T) {
	var stdout, stderr bytes.Buffer
	cmd := newStartCmd(&stdout, &stderr)

	for _, name := range []string{"foreground", "controller", "file", "no-strict"} {
		flag := cmd.Flags().Lookup(name)
		if flag == nil {
			t.Fatalf("missing %s flag", name)
		}
		if !flag.Hidden {
			t.Fatalf("%s flag should be hidden", name)
		}
	}

	if flag := cmd.Flags().Lookup("dry-run"); flag == nil || flag.Hidden {
		t.Fatal("dry-run flag should remain visible")
	}
}

func TestDoStartRequiresInitializedCity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doStart([]string{dir}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "not in a city directory") {
		t.Fatalf("stderr = %q, want city-directory error", stderr.String())
	}
	if !strings.Contains(stderr.String(), `gc init `+dir) {
		t.Fatalf("stderr = %q, want init guidance", stderr.String())
	}
}

func TestDoStartRejectsUnbootstrappedCityConfig(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := doStart([]string{dir}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "city runtime not bootstrapped") {
		t.Fatalf("stderr = %q, want bootstrap error", stderr.String())
	}
	if !strings.Contains(stderr.String(), `gc init `+dir) {
		t.Fatalf("stderr = %q, want init guidance", stderr.String())
	}
}

func TestDoStartForegroundRejectsSupervisorManagedCity(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "bright-lights")
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

	var stdout, stderr bytes.Buffer
	code := doStart([]string{cityPath}, true, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "registered with the supervisor") {
		t.Fatalf("stderr = %q, want supervisor registration error", stderr.String())
	}
}

func TestDoStartRejectsStandaloneOnlyFlagsUnderSupervisor(t *testing.T) {
	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"bright-lights\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldExtraConfigFiles := extraConfigFiles
	oldNoStrictMode := noStrictMode
	extraConfigFiles = []string{"override.toml"}
	noStrictMode = true
	t.Cleanup(func() {
		extraConfigFiles = oldExtraConfigFiles
		noStrictMode = oldNoStrictMode
	})

	var stdout, stderr bytes.Buffer
	code := doStart([]string{cityPath}, false, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("doStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "only apply to the legacy standalone controller") {
		t.Fatalf("stderr = %q, want standalone-flag rejection", stderr.String())
	}
}

func TestStopManagedCityForcesCleanupAfterTimeout(t *testing.T) {
	cityPath := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	closer := &closerSpy{}
	mc := &managedCity{
		name:   "bright-lights",
		cancel: func() {},
		done:   make(chan struct{}),
		closer: closer,
		cr: &CityRuntime{
			cfg: &config.City{
				Session: config.SessionConfig{StartupTimeout: "20ms"},
				Daemon: config.DaemonConfig{
					ShutdownTimeout:   "20ms",
					DriftDrainTimeout: "20ms",
				},
			},
			sp:     runtime.NewFake(),
			rec:    events.Discard,
			stdout: io.Discard,
			stderr: io.Discard,
		},
	}

	var stderr bytes.Buffer
	start := time.Now()
	stopManagedCity(mc, cityPath, &stderr)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("stopManagedCity took %s, want bounded timeout", elapsed)
	}
	if !strings.Contains(stderr.String(), "did not exit within") {
		t.Fatalf("stderr = %q, want forced-timeout warning", stderr.String())
	}
	if !closer.closed {
		t.Fatal("expected closer to be closed after forced cleanup")
	}

	ops := readOpLog(t, logFile)
	if len(ops) != 1 {
		t.Fatalf("expected bead provider stop, got %v", ops)
	}
	if !strings.HasPrefix(ops[0], "stop") {
		t.Fatalf("unexpected bead provider op: %v", ops)
	}
}

func TestStopManagedCityDoesNotUseStartupOrDriftTimeouts(t *testing.T) {
	cityPath := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "ops.log")
	script := writeSpyScript(t, logFile)
	t.Setenv("GC_BEADS", "exec:"+script)

	closer := &closerSpy{}
	mc := &managedCity{
		name:   "bright-lights",
		cancel: func() {},
		done:   make(chan struct{}),
		closer: closer,
		cr: &CityRuntime{
			cfg: &config.City{
				Session: config.SessionConfig{StartupTimeout: "3m"},
				Daemon: config.DaemonConfig{
					ShutdownTimeout:   "20ms",
					DriftDrainTimeout: "2m",
				},
			},
			sp:     runtime.NewFake(),
			rec:    events.Discard,
			stdout: io.Discard,
			stderr: io.Discard,
		},
	}

	var stderr bytes.Buffer
	start := time.Now()
	stopManagedCity(mc, cityPath, &stderr)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("stopManagedCity took %s, want shutdown-timeout bound", elapsed)
	}
	if !strings.Contains(stderr.String(), "20ms") {
		t.Fatalf("stderr = %q, want shutdown-timeout warning", stderr.String())
	}
	if !closer.closed {
		t.Fatal("expected closer to be closed after forced cleanup")
	}

	ops := readOpLog(t, logFile)
	if len(ops) != 1 {
		t.Fatalf("expected bead provider stop, got %v", ops)
	}
	if !strings.HasPrefix(ops[0], "stop") {
		t.Fatalf("unexpected bead provider op: %v", ops)
	}
}
