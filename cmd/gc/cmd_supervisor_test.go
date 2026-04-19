package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/user"
	"path/filepath"
	goruntime "runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestSupervisorAliveIgnoresSharedXDGSocketForIsolatedGCHome(t *testing.T) {
	homeDir := shortTempDir(t, "home-")
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(runtimeDir, "gc", "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		if cmd == "ping" {
			return "4242\n"
		}
		return ""
	})

	gotPath, gotPID := runningSupervisorSocket()
	if gotPath != "" || gotPID != 0 {
		t.Fatalf("runningSupervisorSocket() = (%q, %d), want no shared XDG supervisor for isolated GC_HOME", gotPath, gotPID)
	}
	if pid := supervisorAlive(); pid != 0 {
		t.Fatalf("supervisorAlive() = %d, want 0 when only shared XDG socket exists", pid)
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

func TestReconcileRigIndexSuppressesDeprecatedOrderWarnings(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := setupCity(t, "quiet-supervisor")
	rigDir := filepath.Join(t.TempDir(), "quiet-supervisor-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := `[workspace]
name = "quiet-supervisor"
includes = ["missing-pack"]

[[agent]]
name = "mayor"

[[rigs]]
name = "quiet-supervisor-rig"
path = "` + rigDir + `"

[packs.missing-pack]
source = "https://example.com/missing.git"
ref = "main"
path = "packs/missing"
`
	writeRigAnywhereCityToml(t, cityPath, toml)
	writeRigAnywhereLegacyOrderPack(t, cityPath)

	reg := registryAt(t, gcHome)
	if err := reg.Register(cityPath, "quiet-supervisor"); err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	oldWriter := log.Writer()
	oldFlags := log.Flags()
	oldPrefix := log.Prefix()
	log.SetOutput(&logs)
	log.SetFlags(0)
	log.SetPrefix("")
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
		log.SetPrefix(oldPrefix)
	})

	var stderr bytes.Buffer
	reconcileRigIndex(reg, &stderr)
	if stderr.Len() != 0 {
		t.Fatalf("reconcileRigIndex stderr = %q, want empty", stderr.String())
	}
	if strings.Contains(logs.String(), "deprecated order path") {
		t.Fatalf("reconcileRigIndex emitted order migration warning:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "not found, skipping") {
		t.Fatalf("reconcileRigIndex suppressed non-order config diagnostics; logs:\n%s", logs.String())
	}
	entry, ok := reg.LookupRigByName("quiet-supervisor-rig")
	if !ok {
		t.Fatal("reconcileRigIndex did not register quiet-supervisor-rig")
	}
	assertSameTestPath(t, entry.DefaultCity, cityPath)
}

func TestRenderSupervisorLaunchdTemplate(t *testing.T) {
	data := &supervisorServiceData{
		GCPath:        "/usr/local/bin/gc",
		LogPath:       "/home/user/.gc/supervisor.log",
		GCHome:        "/home/user/.gc",
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
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
		"XDG_RUNTIME_DIR",
		"/tmp/gc-run",
		"<key>PATH</key>",
	} {
		if !strings.Contains(content, check) {
			t.Fatalf("launchd template missing %q", check)
		}
	}
}

func TestRenderSupervisorSystemdTemplate(t *testing.T) {
	data := &supervisorServiceData{
		GCPath:        "/usr/local/bin/gc",
		LogPath:       "/home/user/.gc/supervisor.log",
		GCHome:        "/home/user/.gc",
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
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
		`Environment=XDG_RUNTIME_DIR="/tmp/gc-run"`,
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
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	t.Setenv("PATH", "/usr/local/bin:/usr/bin:/bin")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/gc-run")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	if !slices.Contains(filepath.SplitList(data.Path), nvmBin) {
		t.Fatalf("buildSupervisorServiceData PATH %q missing nvm bin %q", data.Path, nvmBin)
	}
	if data.XDGRuntimeDir != "/tmp/gc-run" {
		t.Fatalf("buildSupervisorServiceData XDGRuntimeDir = %q, want /tmp/gc-run", data.XDGRuntimeDir)
	}
}

func TestReconcileRigIndexSuppressesMigrationWarnings(t *testing.T) {
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)

	cityPath := filepath.Join(t.TempDir(), "city")
	rigPath := filepath.Join(t.TempDir(), "rig")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(city): %v", err)
	}
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatalf("MkdirAll(rig): %v", err)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = "alpha"

[agents]
append_fragments = ["legacy.md"]

[[rigs]]
name = "alpha-rig"
path = %q
`, rigPath)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "alpha"); err != nil {
		t.Fatalf("Register(city): %v", err)
	}

	var stderr bytes.Buffer
	reconcileRigIndex(reg, &stderr)

	if strings.Contains(stderr.String(), "[agents] is a deprecated compatibility alias") {
		t.Fatalf("stderr = %q, want migration warnings suppressed during rig reconcile", stderr.String())
	}

	rigs, err := reg.ListRigs()
	if err != nil {
		t.Fatalf("ListRigs(): %v", err)
	}
	if len(rigs) != 1 {
		t.Fatalf("ListRigs() len = %d, want 1", len(rigs))
	}
	if rigs[0].Name != "alpha-rig" {
		t.Fatalf("rig name = %q, want %q", rigs[0].Name, "alpha-rig")
	}
	if rigs[0].DefaultCity == "" {
		t.Fatal("default city = empty, want reconciled default city")
	}
}

func TestEmitSupervisorLoadCityConfigWarningsOncePerCity(t *testing.T) {
	var stderr bytes.Buffer
	prov := &config.Provenance{
		Warnings: []string{
			`/city/pack.toml: [agents] is a deprecated compatibility alias for [agent_defaults]; rewrite the table name to [agent_defaults]`,
			`/city/pack.toml: [agents] is a deprecated compatibility alias for [agent_defaults]; rewrite the table name to [agent_defaults]`,
		},
	}
	cityPath := filepath.Join(t.TempDir(), "city")
	otherCityPath := filepath.Join(t.TempDir(), "other-city")

	emitSupervisorLoadCityConfigWarnings(&stderr, cityPath, prov)
	emitSupervisorLoadCityConfigWarnings(&stderr, cityPath, prov)
	emitSupervisorLoadCityConfigWarnings(&stderr, otherCityPath, prov)

	const want = "[agents] is a deprecated compatibility alias for [agent_defaults]"
	if got := strings.Count(stderr.String(), want); got != 2 {
		t.Fatalf("warning count = %d, want 2 (once per city); stderr=%q", got, stderr.String())
	}
}

func TestBuildSupervisorServiceDataOmitsXDGRuntimeDirForIsolatedGCHome(t *testing.T) {
	homeDir := t.TempDir()
	gcHome := filepath.Join(t.TempDir(), "isolated-home")
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/gc-run")

	data, err := buildSupervisorServiceData()
	if err != nil {
		t.Fatalf("buildSupervisorServiceData: %v", err)
	}
	if data.GCHome != gcHome {
		t.Fatalf("buildSupervisorServiceData GCHome = %q, want %q", data.GCHome, gcHome)
	}
	if data.XDGRuntimeDir != "" {
		t.Fatalf("buildSupervisorServiceData XDGRuntimeDir = %q, want empty for isolated GC_HOME", data.XDGRuntimeDir)
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

func TestInstallSupervisorSystemdRestartsWhenUnitChangesAndServiceActive(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        "/tmp/gc-home",
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}
	path := supervisorSystemdServicePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("old unit\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(service string) bool {
		return service == "gascity-supervisor.service"
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	joined := strings.Join(calls, "\n")
	for _, want := range []string{
		"--user daemon-reload",
		"--user enable gascity-supervisor.service",
		"--user restart gascity-supervisor.service",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("systemctl calls = %v, want %q", calls, want)
		}
	}
	if strings.Contains(joined, "--user start gascity-supervisor.service") {
		t.Fatalf("systemctl calls = %v, should restart instead of start when unit changes under an active service", calls)
	}
}

func TestInstallSupervisorSystemdStartsInactiveService(t *testing.T) {
	if goruntime.GOOS != "linux" {
		t.Skip("systemd path only applies on linux")
	}
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)

	data := &supervisorServiceData{
		GCPath:        "/tmp/gc-new",
		LogPath:       "/tmp/gc-home/supervisor.log",
		GCHome:        "/tmp/gc-home",
		XDGRuntimeDir: "/tmp/gc-run",
		Path:          "/usr/local/bin:/usr/bin:/bin",
	}

	oldRun := supervisorSystemctlRun
	oldActive := supervisorSystemctlActive
	var calls []string
	supervisorSystemctlRun = func(args ...string) error {
		calls = append(calls, strings.Join(args, " "))
		return nil
	}
	supervisorSystemctlActive = func(_ string) bool {
		return false
	}
	t.Cleanup(func() {
		supervisorSystemctlRun = oldRun
		supervisorSystemctlActive = oldActive
	})

	var stdout, stderr bytes.Buffer
	if code := installSupervisorSystemd(data, &stdout, &stderr); code != 0 {
		t.Fatalf("installSupervisorSystemd code = %d, want 0; stderr=%q", code, stderr.String())
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "--user start gascity-supervisor.service") {
		t.Fatalf("systemctl calls = %v, want start for inactive service", calls)
	}
	if strings.Contains(joined, "--user restart gascity-supervisor.service") {
		t.Fatalf("systemctl calls = %v, should not restart inactive service", calls)
	}
}

func TestDoSupervisorStartRejectsHomeOverride(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("platform supervisor home override guard only applies on linux/darwin")
	}
	lookup, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lookup.HomeDir) == "" {
		t.Skip("user lookup home unavailable")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := doSupervisorStart(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorStart code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Keep HOME unchanged and use GC_HOME for isolated runs") {
		t.Fatalf("stderr = %q, want HOME override guidance", stderr.String())
	}
	if !strings.Contains(stderr.String(), lookup.HomeDir) {
		t.Fatalf("stderr = %q, want current home %q", stderr.String(), lookup.HomeDir)
	}
}

func TestDoSupervisorInstallRejectsHomeOverride(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("platform supervisor home override guard only applies on linux/darwin")
	}
	lookup, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lookup.HomeDir) == "" {
		t.Skip("user lookup home unavailable")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := doSupervisorInstall(&stdout, &stderr); code != 1 {
		t.Fatalf("doSupervisorInstall code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Keep HOME unchanged and use GC_HOME for isolated runs") {
		t.Fatalf("stderr = %q, want HOME override guidance", stderr.String())
	}
	if !strings.Contains(stderr.String(), lookup.HomeDir) {
		t.Fatalf("stderr = %q, want current home %q", stderr.String(), lookup.HomeDir)
	}
}

func TestEnsureSupervisorRunningRejectsHomeOverride(t *testing.T) {
	if goruntime.GOOS != "linux" && goruntime.GOOS != "darwin" {
		t.Skip("platform supervisor home override guard only applies on linux/darwin")
	}
	lookup, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err != nil || strings.TrimSpace(lookup.HomeDir) == "" {
		t.Skip("user lookup home unavailable")
	}
	t.Setenv("HOME", t.TempDir())

	var stdout, stderr bytes.Buffer
	if code := ensureSupervisorRunning(&stdout, &stderr); code != 1 {
		t.Fatalf("ensureSupervisorRunning code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "Keep HOME unchanged and use GC_HOME for isolated runs") {
		t.Fatalf("stderr = %q, want HOME override guidance", stderr.String())
	}
	if !strings.Contains(stderr.String(), lookup.HomeDir) {
		t.Fatalf("stderr = %q, want current home %q", stderr.String(), lookup.HomeDir)
	}
}

func TestWaitForSupervisorReadyUsesHookedTimeout(t *testing.T) {
	oldAlive := supervisorAliveHook
	oldTimeout := supervisorReadyTimeout
	oldPoll := supervisorReadyPollInterval
	calls := 0
	supervisorAliveHook = func() int {
		calls++
		if calls < 4 {
			return 0
		}
		return 4242
	}
	supervisorReadyTimeout = 25 * time.Millisecond
	supervisorReadyPollInterval = time.Millisecond
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorReadyTimeout = oldTimeout
		supervisorReadyPollInterval = oldPoll
	})

	var stderr bytes.Buffer
	if code := waitForSupervisorReady(&stderr); code != 0 {
		t.Fatalf("waitForSupervisorReady code = %d, want 0; stderr=%q", code, stderr.String())
	}
	if calls < 4 {
		t.Fatalf("supervisorAliveHook called %d times, want at least 4", calls)
	}
}

func TestWaitForSupervisorReadySucceedsWhenAlreadyReadyEvenWithZeroTimeout(t *testing.T) {
	oldAlive := supervisorAliveHook
	oldTimeout := supervisorReadyTimeout
	oldPoll := supervisorReadyPollInterval
	supervisorAliveHook = func() int { return 4242 }
	supervisorReadyTimeout = 0
	supervisorReadyPollInterval = time.Millisecond
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorReadyTimeout = oldTimeout
		supervisorReadyPollInterval = oldPoll
	})

	var stderr bytes.Buffer
	if code := waitForSupervisorReady(&stderr); code != 0 {
		t.Fatalf("waitForSupervisorReady code = %d, want 0; stderr=%q", code, stderr.String())
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

func TestDeleteManagedCityIfCurrentKeepsReplacementCity(t *testing.T) {
	oldCity := &managedCity{name: "bright-lights"}
	newCity := &managedCity{name: "bright-lights"}
	cities := map[string]*managedCity{"/city": newCity}

	if deleted := deleteManagedCityIfCurrent(cities, "/city", oldCity); deleted {
		t.Fatal("deleteManagedCityIfCurrent returned true for stale city pointer")
	}
	if got := cities["/city"]; got != newCity {
		t.Fatalf("city at /city = %#v, want replacement city %#v", got, newCity)
	}
}

func TestDeleteManagedCityIfCurrentRemovesMatchingCity(t *testing.T) {
	current := &managedCity{name: "bright-lights"}
	cities := map[string]*managedCity{"/city": current}

	if deleted := deleteManagedCityIfCurrent(cities, "/city", current); !deleted {
		t.Fatal("deleteManagedCityIfCurrent returned false, want true")
	}
	if _, ok := cities["/city"]; ok {
		t.Fatalf("cities still contains /city after delete: %#v", cities["/city"])
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
	if !strings.Contains(stderr.String(), `gc init `+dir) && !strings.Contains(stderr.String(), `gc init `+canonicalTestPath(dir)) {
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
	err := stopManagedCity(mc, cityPath, &stderr)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("stopManagedCity took %s, want bounded timeout", elapsed)
	}
	if err == nil {
		t.Fatal("stopManagedCity err = nil, want non-nil because city never exited")
	}
	if !strings.Contains(err.Error(), "did not exit") {
		t.Fatalf("stopManagedCity err = %q, want 'did not exit' detail", err.Error())
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
	err := stopManagedCity(mc, cityPath, &stderr)
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("stopManagedCity took %s, want shutdown-timeout bound", elapsed)
	}
	if err == nil {
		t.Fatal("stopManagedCity err = nil, want non-nil because city never exited")
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

// TestStopSupervisorWithWaitBlocksUntilSocketStops exercises the --wait
// path of `gc supervisor stop`. The fake socket answers "ping" with a PID
// (so supervisorAliveAtPath keeps returning alive) for ~200ms after the
// "stop" request, then closes the listener. stopSupervisorWithWait must
// block across that window and return success.
func TestStopSupervisorWithWaitBlocksUntilSocketStops(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() {
		lis.Close()         //nolint:errcheck
		os.Remove(sockPath) //nolint:errcheck
	})

	stopDelay := 200 * time.Millisecond
	// stopRequested/stopAt are touched by the "stop" handler goroutine and
	// read concurrently by every "ping" handler goroutine. Guard with a
	// mutex so `go test -race` doesn't flag this fake server.
	var (
		mu            sync.Mutex
		stopRequested bool
		stopAt        time.Time
	)
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
				cmd := strings.TrimSpace(string(buf[:n]))
				switch cmd {
				case "ping":
					mu.Lock()
					finished := stopRequested && time.Now().After(stopAt)
					mu.Unlock()
					if finished {
						// Stop answering ping so the waiter sees us as gone.
						return
					}
					io.WriteString(conn, "4242\n") //nolint:errcheck
				case "stop":
					mu.Lock()
					stopRequested = true
					stopAt = time.Now().Add(stopDelay)
					mu.Unlock()
					io.WriteString(conn, "ok\n") //nolint:errcheck
					// New protocol: --wait clients also read a final
					// status line. Emit done:ok after the stop delay so
					// this test exercises the happy path of the new
					// protocol in addition to the socket-close fallback.
					time.Sleep(stopDelay)
					io.WriteString(conn, "done:ok\n") //nolint:errcheck
				}
			}(conn)
		}
	}()

	start := time.Now()
	var stdout, stderr bytes.Buffer
	code := stopSupervisorWithWait(&stdout, &stderr, true, 5*time.Second)
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if elapsed < stopDelay {
		t.Fatalf("returned after %s, expected at least %s (must have waited for socket to stop answering)", elapsed, stopDelay)
	}
	if !strings.Contains(stdout.String(), "Supervisor stopped.") {
		t.Fatalf("stdout = %q, want final confirmation message", stdout.String())
	}
}

// TestStopSupervisorWithoutWaitReturnsAfterAck confirms the default
// (non-wait) path returns as soon as the supervisor ACKs the stop. The
// fake socket keeps answering "ping" indefinitely; without --wait,
// stopSupervisor must not block on the ping result.
func TestStopSupervisorWithoutWaitReturnsAfterAck(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		switch cmd {
		case "ping":
			return "4242\n"
		case "stop":
			return "ok\n"
		}
		return ""
	})

	start := time.Now()
	var stdout, stderr bytes.Buffer
	code := stopSupervisor(&stdout, &stderr)
	elapsed := time.Since(start)

	if code != 0 {
		t.Fatalf("stopSupervisor code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if elapsed > 2*time.Second {
		t.Fatalf("returned after %s, expected fast return (no wait) — waited anyway?", elapsed)
	}
	if !strings.Contains(stdout.String(), "Supervisor stopping...") {
		t.Fatalf("stdout = %q, want 'Supervisor stopping...' message", stdout.String())
	}
	if strings.Contains(stdout.String(), "Supervisor stopped.") {
		t.Fatalf("stdout unexpectedly contains 'Supervisor stopped.' — wait flag was false")
	}
}

// TestStopSupervisorWithWaitPropagatesDoneErr exercises the new
// post-shutdown status protocol: the server sends "ok\n" to ack the
// stop request, then "done:err:<detail>\n" when shutdown finished with
// errors (e.g., a managed city failed to quiesce). --wait must surface
// the error to stderr and exit non-zero so test cleanup sees the flake
// instead of believing shutdown was clean.
func TestStopSupervisorWithWaitPropagatesDoneErr(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen: %v", err)
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
				r := bufio.NewReader(conn)
				line, err := r.ReadString('\n')
				if err != nil {
					return
				}
				switch strings.TrimSpace(line) {
				case "ping":
					io.WriteString(conn, "4242\n") //nolint:errcheck
				case "stop":
					io.WriteString(conn, "ok\n")                                             //nolint:errcheck
					io.WriteString(conn, "done:err:city \"alpha\" did not exit within 5s\n") //nolint:errcheck
				}
			}(conn)
		}
	}()

	var stdout, stderr bytes.Buffer
	code := stopSupervisorWithWait(&stdout, &stderr, true, 2*time.Second)

	if code != 1 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 1 (propagated done:err); stderr=%q", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "alpha") || !strings.Contains(stderr.String(), "did not exit") {
		t.Fatalf("stderr = %q, want it to include the server's done:err detail", stderr.String())
	}
	if strings.Contains(stdout.String(), "Supervisor stopped.") {
		t.Fatalf("stdout unexpectedly contains 'Supervisor stopped.' — shutdown reported errors")
	}
}

// TestStopSupervisorWithWaitTimesOutWhenSocketKeepsAnswering guards the
// wait-timeout path. The fake socket keeps answering ping forever; --wait
// with a tiny timeout must return non-zero and mention the timeout.
func TestStopSupervisorWithWaitTimesOutWhenSocketKeepsAnswering(t *testing.T) {
	gcHome := shortTempDir(t, "gc-home-")
	runtimeDir := shortTempDir(t, "gc-run-")
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)

	sockPath := filepath.Join(gcHome, "supervisor.sock")
	startTestSupervisorSocket(t, sockPath, func(cmd string) string {
		switch cmd {
		case "ping":
			return "4242\n"
		case "stop":
			return "ok\n"
		}
		return ""
	})

	var stdout, stderr bytes.Buffer
	code := stopSupervisorWithWait(&stdout, &stderr, true, 300*time.Millisecond)

	if code != 1 {
		t.Fatalf("stopSupervisorWithWait code = %d, want 1 (timeout)", code)
	}
	if !strings.Contains(stderr.String(), "timed out") {
		t.Fatalf("stderr = %q, want timeout message", stderr.String())
	}
}
