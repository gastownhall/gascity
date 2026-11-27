package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/userscope"
)

// disableInfraScopeForTests makes infrastructure spawns run unscoped without
// probing the host's systemd user manager. TestMain calls it.
func disableInfraScopeForTests() {
	infraScope = &userscope.Wrapper{Probe: func(string) error {
		return errors.New("systemd user scopes disabled in tests")
	}}
}

// fakeCgroupEnv models cgroup membership in the fakes: like a cgroup, it is
// inherited by every descendant, and the fake systemd-run moves its command
// into a new "scope" by overwriting it.
const fakeCgroupEnv = "FAKE_CGROUP_SCOPE"

// testSessionIdentityEnv is the session identity gc gives every agent
// session, as a test-local name: production code no longer reads it to
// decide infrastructure scoping, and the tests that still vary it do so to
// prove it no longer matters.
const testSessionIdentityEnv = "GC_SESSION_ID"

// fakeInfraScopeName is the fake scope the fake systemd-run moves commands to.
const fakeInfraScopeName = "fake-infra-scope"

// infraScopeHost describes the host facts the infrastructure scope rule
// reads.
type infraScopeHost struct {
	session bool   // the gc process carries GC_SESSION_ID
	linger  bool   // /var/lib/systemd/linger/<user> exists
	cgroup  string // the gc process's cgroup v2 path; empty means loginSessionCgroup
}

// Cgroups the fake host can place the gc process in. "<uid>" stands for the
// test process's uid.
const (
	// loginSessionCgroup is a login session's scope: a shell, or a tmux
	// server without systemd support started from an SSH login.
	loginSessionCgroup = "/user.slice/user-<uid>.slice/session-3.scope"
	// userManagerCgroup is a scope under the user manager, such as a
	// GC_AGENT_SLICE pane scope.
	userManagerCgroup = "/user.slice/user-<uid>.slice/user@<uid>.service/app.slice/run-r1.scope"
	// userServiceCgroup is a user service or timer unit under the user
	// manager, such as nudge-drainer.service: the gc process carries no
	// session identity, but a unit stop still tears the whole cgroup down
	// (issue #5910's non-session cases).
	userServiceCgroup = "/user.slice/user-<uid>.slice/user@<uid>.service/app.slice/nudge-drainer.service"
	// supervisorSliceServiceCgroup is the supervisor run as a system
	// service in a slice of its own; its agent panes share its cgroup.
	supervisorSliceServiceCgroup = "/gascity.slice/gascity-prod.slice/gascity-supervisor.service"
	// systemServiceCgroup is the supervisor run as a plain system service.
	systemServiceCgroup = "/system.slice/gascity-supervisor.service"
	// containerRootCgroup is the root of a container's cgroup namespace.
	containerRootCgroup = "/"
)

// installInfraScopeHost fakes the host for the infrastructure scope rule: a
// systemd-run first on PATH that appends its argv (one invocation per line)
// to the returned log, moves its command to fakeInfraScopeName and execs the
// command after "--" exactly like systemd-run's scope mode (the spawned PID
// is the command's PID); a linger directory; this process's cgroup file; and
// a fresh production wrapper so the one-per-process probe runs again.
func installInfraScopeHost(t *testing.T, h infraScopeHost) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fake requires POSIX sh")
	}
	u, err := user.Current()
	if err != nil {
		t.Skipf("current user: %v", err)
	}
	dir := t.TempDir()
	logPath := filepath.Join(dir, "systemd-run.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + logPath + "'\n" +
		"while [ $# -gt 0 ] && [ \"$1\" != \"--\" ]; do shift; done\n" +
		"shift\n" +
		"export " + fakeCgroupEnv + "=" + fakeInfraScopeName + "\n" +
		"exec \"$@\"\n"
	if err := os.WriteFile(filepath.Join(dir, "systemd-run"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake systemd-run: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(infraSliceEnv, "")
	if h.session {
		t.Setenv(testSessionIdentityEnv, "gc-test-session")
	} else {
		t.Setenv(testSessionIdentityEnv, "")
	}

	lingerDir := filepath.Join(dir, "linger")
	if err := os.MkdirAll(lingerDir, 0o755); err != nil {
		t.Fatalf("mkdir linger: %v", err)
	}
	if h.linger {
		if err := os.WriteFile(filepath.Join(lingerDir, u.Username), nil, 0o644); err != nil {
			t.Fatalf("write linger marker: %v", err)
		}
	}
	cgroup := h.cgroup
	if cgroup == "" {
		cgroup = loginSessionCgroup
	}
	cgroup = "0::" + strings.ReplaceAll(cgroup, "<uid>", strconv.Itoa(os.Getuid())) + "\n"
	cgroupPath := filepath.Join(dir, "cgroup")
	if err := os.WriteFile(cgroupPath, []byte(cgroup), 0o644); err != nil {
		t.Fatalf("write cgroup file: %v", err)
	}

	oldScope, oldLinger, oldCgroup := infraScope, infraLingerDir, infraProcCgroupPath
	infraScope, infraLingerDir, infraProcCgroupPath = newInfraScope(), lingerDir, cgroupPath
	t.Cleanup(func() {
		infraScope, infraLingerDir, infraProcCgroupPath = oldScope, oldLinger, oldCgroup
	})
	return logPath
}

// infraTestDescription is the scope description the infraArgv tests pass.
const infraTestDescription = "gascity infra: test"

// infraTestScopeArgv is argv as infraArgv scopes it under the default slice
// with infraTestDescription; unit is the name infraArgv returned.
func infraTestScopeArgv(unit string, argv []string) []string {
	return userscope.ScopeArgv(userscope.Scope{Slice: defaultInfraSlice, Unit: unit, Description: infraTestDescription}, argv)
}

// wrappableFromSession is a host on which a spawn from a session is scoped.
var wrappableFromSession = infraScopeHost{session: true, linger: true}

// infraPollUntil reports whether cond holds within timeout, checking every 10ms.
func infraPollUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.After(timeout)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for !cond() {
		select {
		case <-deadline:
			return cond()
		case <-tick.C:
		}
	}
	return true
}

// waitForSystemdRunInvocation polls the fake's log until an invocation
// containing want appears.
func waitForSystemdRunInvocation(t *testing.T, logPath, want string) {
	t.Helper()
	var data []byte
	found := infraPollUntil(10*time.Second, func() bool {
		data, _ = os.ReadFile(logPath)
		for _, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, want) {
				return true
			}
		}
		return false
	})
	if !found {
		t.Fatalf("no systemd-run invocation containing %q; log:\n%s", want, data)
	}
}

// assertNoSystemdRun fails if the fake systemd-run ever ran. Call it after
// the spawned command was observed running: the fake logs before it execs
// its command, so a wrapped spawn would already have logged.
func assertNoSystemdRun(t *testing.T, logPath string) {
	t.Helper()
	if data, err := os.ReadFile(logPath); err == nil {
		t.Fatalf("systemd-run ran for a spawn that must stay unwrapped; log:\n%s", data)
	}
}

// waitForFile polls until path exists.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	if !infraPollUntil(10*time.Second, func() bool { _, err := os.Stat(path); return err == nil }) {
		t.Fatalf("%s was not written", path)
	}
}

// fakeDoltFinalImage is the program the marking fake dolt execs to stay up.
const fakeDoltFinalImage = "sleep"

// writeMarkingFakeDolt writes a fake dolt that records it is running, then
// stays up like a sql-server by exec'ing fakeDoltFinalImage. The marker is
// written before that exec, so the process may still be mid-exec when the
// marker appears; waitForFinalImage waits it out. It returns the bin dir and
// the marker path.
func writeMarkingFakeDolt(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "dolt.started")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = sql-server ] || { echo \"unexpected dolt args: $*\" >&2; exit 2; }\n" +
		": > '" + marker + "'\n" +
		"exec " + fakeDoltFinalImage + " 60\n"
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return dir, marker
}

// writeMarkingFakeDoltDyingOnSecondTERM writes a fake dolt that records it
// is running and then stays up, exiting only on its second SIGTERM: the
// first (a unit stop's cgroup-wide signal) alone does not finish it, the
// second (the watchdog forwarding that stop) does. That makes the
// watchdog's forwarding observable and deterministic — the server cannot
// die before the watchdog has logged and forwarded — without depending on
// the SIGKILL escalation's grace period. The shell stays the process image,
// and the marker appears only after the kernel finished exec'ing it, so no
// waitForFinalImage dance is needed.
func writeMarkingFakeDoltDyingOnSecondTERM(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "dolt.started")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = sql-server ] || { echo \"unexpected dolt args: $*\" >&2; exit 2; }\n" +
		": > '" + marker + "'\n" +
		"terms=0\n" +
		"trap 'terms=$((terms+1)); [ \"$terms\" -ge 2 ] && exit 0' TERM\n" +
		"while :; do sleep 5; done\n"
	if err := os.WriteFile(filepath.Join(dir, "dolt"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake dolt: %v", err)
	}
	return dir, marker
}

const infraScopePrefix = "--user --scope --slice=gascity-infra.slice --collect --quiet "

// infraUnitPattern is the transient scope unit name gc gives a scoped spawn.
var infraUnitPattern = regexp.MustCompile(`^gascity-infra-[0-9a-f]{16}\.scope$`)

// assertScopedInvocation checks that the scoped spawn named its scope unit,
// recorded as unit, and the description journald keeps after collection.
func assertScopedInvocation(t *testing.T, logPath, unit, description string) {
	t.Helper()
	if !infraUnitPattern.MatchString(unit) {
		t.Fatalf("recorded infra scope unit = %q, want %s", unit, infraUnitPattern)
	}
	data, _ := os.ReadFile(logPath)
	want := infraScopePrefix + "--unit=" + unit + " --description=" + description + " -- "
	if !strings.Contains(string(data), want) {
		t.Fatalf("no systemd-run invocation containing %q; log:\n%s", want, data)
	}
}

func killInfraScopeTestPID(t *testing.T, pid int) {
	t.Helper()
	t.Cleanup(func() {
		if pid > 0 {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	})
}

// startDirectManagedDolt starts the managed Dolt server without the scope
// watchdog, as a gc/bd call that autostarts it does.
func startDirectManagedDolt(t *testing.T) (managedDoltStartedProcess, string) {
	t.Helper()
	withManagedDoltTestMode(t, false)
	t.Setenv(managedDoltTestModeEnv, "")
	t.Setenv(managedDoltScopeWatchdogEnv, "0")
	binDir, marker := writeMarkingFakeDolt(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	configPath := filepath.Join(dir, "dolt-config.yaml")
	logFile, err := os.Create(filepath.Join(dir, "dolt.log"))
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	started, err := startManagedDoltSQLServer("", configPath, logFile.Name(), logFile)
	if err != nil {
		t.Fatalf("startManagedDoltSQLServer: %v", err)
	}
	killInfraScopeTestPID(t, started.PID)
	waitForFile(t, marker)
	return started, configPath
}

func TestManagedDoltSpawnWrappedInInfraScope(t *testing.T) {
	for _, tc := range []struct {
		name string
		host infraScopeHost
	}{
		{name: "from a session", host: wrappableFromSession},
		// A gc process without a session identity under user@<uid>.service
		// (a user or bot service, a timer unit) is scoped too: its unit stop
		// would otherwise SIGTERM the store (issue #5910).
		{name: "outside a session, from a user service", host: infraScopeHost{linger: true, cgroup: userServiceCgroup}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := installInfraScopeHost(t, tc.host)
			started, configPath := startDirectManagedDolt(t)
			waitForSystemdRunInvocation(t, logPath, " -- dolt sql-server --config "+configPath)
			assertScopedInvocation(t, logPath, started.InfraScope, "gascity infra: managed dolt")
		})
	}
	// Each of these stays in its caller's cgroup.
	for _, tc := range []struct {
		name     string
		host     infraScopeHost
		sliceEnv string
	}{
		{name: "from a session with GC_INFRA_SLICE=off", host: wrappableFromSession, sliceEnv: "off"},
		{name: "from a session in a system service", host: infraScopeHost{session: true, linger: true, cgroup: supervisorSliceServiceCgroup}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := installInfraScopeHost(t, tc.host)
			t.Setenv(infraSliceEnv, tc.sliceEnv)
			started, _ := startDirectManagedDolt(t)
			assertNoSystemdRun(t, logPath)
			if started.InfraScope != "" {
				t.Fatalf("unwrapped spawn recorded infra scope %q, want none", started.InfraScope)
			}
		})
	}
}

// TestManagedDoltStateRecordsInfraScope: the scope a managed Dolt server was
// started in is recorded with its PID in the provider state, so gc doctor can
// point at the scope's journal records after --collect removed the unit.
func TestManagedDoltStateRecordsInfraScope(t *testing.T) {
	const unit = "gascity-infra-0123456789abcdef.scope"
	cityPath := installStartManagedDoltLoopStubs(t, startManagedDoltLoopStubs{
		startFn: func(cityPath, _, _ string, _ *os.File) (managedDoltStartedProcess, error) {
			return managedDoltStartedProcess{CityPath: cityPath, PID: 4242, InfraScope: unit}, nil
		},
		waitReadyFn: func(_, _, _, _ string, _ int, _ time.Duration, _ bool) (managedDoltWaitReadyReport, error) {
			return managedDoltWaitReadyReport{Ready: true, PIDAlive: true}, nil
		},
		logSuffixFn:     func(string, int64) (string, error) { return "", nil },
		portAvailableFn: func(string, int) bool { return true },
	})
	if _, err := startManagedDoltProcessWithOptions(cityPath, "127.0.0.1", "17778", "root", "warning", -1, time.Second, false); err != nil {
		t.Fatalf("startManagedDoltProcessWithOptions: %v", err)
	}
	layout, err := resolveManagedDoltRuntimeLayout(cityPath)
	if err != nil {
		t.Fatalf("layout: %v", err)
	}
	state, err := readDoltRuntimeStateFile(layout.StateFile)
	if err != nil {
		t.Fatalf("read provider state: %v", err)
	}
	if state.PID != 4242 || state.InfraScope != unit {
		t.Fatalf("provider state pid=%d infra_scope=%q, want 4242 and %q", state.PID, state.InfraScope, unit)
	}
}

// The bd provider script the supervisor launches runs `gc dolt-state
// start-managed` with the supervisor's environment: BEADS_ACTOR (one of the
// session-scoped keys) is set, GC_SESSION_ID is not. Where the server lands
// follows the supervisor's own cgroup: under the user manager (a supervisor
// user unit) the move suits the caller and the spawn is scoped — stopping
// that unit must not take the store with it — while in a system service the
// operator placed the tree deliberately and the spawn stays put.
func TestSupervisorLaunchedProviderSpawnScopedUnderUserManager(t *testing.T) {
	logPath := installInfraScopeHost(t, infraScopeHost{linger: true, cgroup: userServiceCgroup})
	t.Setenv("BEADS_ACTOR", "gc-supervisor")
	started, configPath := startDirectManagedDolt(t)
	waitForSystemdRunInvocation(t, logPath, " -- dolt sql-server --config "+configPath)
	assertScopedInvocation(t, logPath, started.InfraScope, "gascity infra: managed dolt")
}

func TestSupervisorLaunchedProviderSpawnStaysUnwrappedInSystemService(t *testing.T) {
	logPath := installInfraScopeHost(t, infraScopeHost{linger: true, cgroup: supervisorSliceServiceCgroup})
	t.Setenv("BEADS_ACTOR", "gc-supervisor")
	startDirectManagedDolt(t)
	assertNoSystemdRun(t, logPath)
}

func TestScopeWatchdogSpawnWrappedInInfraScope(t *testing.T) {
	cityPath := t.TempDir()
	start := func(t *testing.T) (string, managedDoltStartedProcess) {
		t.Helper()
		binDir, marker := writeMarkingFakeDolt(t)
		dir := t.TempDir()
		configPath := filepath.Join(dir, "dolt-config.yaml")
		if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		logFilePath := filepath.Join(dir, "dolt.log")
		logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			t.Fatalf("open log file: %v", err)
		}
		t.Cleanup(func() { _ = logFile.Close() })
		parentEnv := removeEnvKey(os.Environ(), "PATH")
		parentEnv = append(parentEnv, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

		started, err := startManagedDoltSQLServerWithScopeWatchdogEnv(cityPath, configPath, logFilePath, logFile, parentEnv)
		if err != nil {
			t.Fatalf("start managed dolt with scope watchdog: %v", err)
		}
		killInfraScopeTestPID(t, started.PID)
		killInfraScopeTestPID(t, started.WatchdogPID)
		waitForFile(t, marker)
		return configPath, started
	}

	for _, tc := range []struct {
		name string
		host infraScopeHost
	}{
		{name: "from a session", host: wrappableFromSession},
		// A gc process without a session identity under user@<uid>.service
		// (a user or bot service, a timer unit) scopes the watchdog too: its
		// unit stop would otherwise SIGTERM the store through the watchdog
		// (issue #5910).
		{name: "outside a session, from a user service", host: infraScopeHost{linger: true, cgroup: userServiceCgroup}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := installInfraScopeHost(t, tc.host)
			configPath, started := start(t)
			exe, err := managedDoltWatchdogExecutable()
			if err != nil {
				t.Fatalf("watchdog executable: %v", err)
			}
			waitForSystemdRunInvocation(t, logPath, " -- "+exe+" "+managedDoltScopeWatchdogArg+" "+configPath)
			assertScopedInvocation(t, logPath, started.InfraScope, "gascity infra: managed dolt for city "+cityPath)
			// The dolt server is the watchdog's child and inherits its scope;
			// it is not wrapped a second time. (The other line is the probe.)
			data, _ := os.ReadFile(logPath)
			if n := strings.Count(string(data), " "+managedDoltScopeWatchdogArg+" "); n != 1 {
				t.Fatalf("watchdog wrapped %d times, want 1; log:\n%s", n, data)
			}
			if strings.Contains(string(data), "-- dolt ") {
				t.Fatalf("watchdog's dolt child was wrapped again; log:\n%s", data)
			}
		})
	}
	for _, tc := range []struct {
		name     string
		host     infraScopeHost
		sliceEnv string
	}{
		{name: "from a session with GC_INFRA_SLICE=off", host: wrappableFromSession, sliceEnv: "off"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			logPath := installInfraScopeHost(t, tc.host)
			t.Setenv(infraSliceEnv, tc.sliceEnv)
			_, started := start(t)
			assertNoSystemdRun(t, logPath)
			if started.InfraScope != "" {
				t.Fatalf("unwrapped watchdog recorded infra scope %q, want none", started.InfraScope)
			}
		})
	}
}

// The supervisor is never moved into an infrastructure scope, not even when
// forked from a session: every session it starts would follow it into that
// one unbounded slice.
func TestBareSupervisorForkNotWrappedInInfraScope(t *testing.T) {
	fork := func(t *testing.T) {
		t.Helper()
		pinRealHome(t)
		t.Setenv("GC_HOME", t.TempDir())
		t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
		t.Setenv(supervisorSystemdUnitEnv, "")
		t.Setenv(supervisorSystemdScopeEnv, "")
		previousAlive := supervisorAliveHook
		supervisorAliveHook = func() int { return 4242 }
		t.Cleanup(func() { supervisorAliveHook = previousAlive })

		// The forked child is this test binary; the direct-child spy makes
		// it snapshot its environment and exit instead of running the suite.
		// It returns once the child ran.
		captureProductMetricsDirectChildEnv(t, func() error {
			var stdout, stderr bytes.Buffer
			if code := startBareSupervisor(&stdout, &stderr, false, false); code != 0 {
				t.Fatalf("startBareSupervisor = %d; stderr=%q", code, stderr.String())
			}
			return nil
		})
	}

	t.Run("from a session", func(t *testing.T) {
		logPath := installInfraScopeHost(t, wrappableFromSession)
		fork(t)
		assertNoSystemdRun(t, logPath)
	})
	t.Run("outside a session", func(t *testing.T) {
		logPath := installInfraScopeHost(t, infraScopeHost{linger: true, cgroup: userManagerCgroup})
		fork(t)
		assertNoSystemdRun(t, logPath)
	})
}

// The drift respawn replaces the supervisor; like the bare fork it is never
// scoped.
func TestDriftSupervisorRespawnNotWrappedInInfraScope(t *testing.T) {
	respawn := func(t *testing.T) {
		t.Helper()
		t.Setenv("GC_HOME", t.TempDir())
		marker := filepath.Join(t.TempDir(), "respawned")
		if err := spawnDetachedSupervisor("/bin/sh", "-c", ": > "+marker); err != nil {
			t.Fatalf("spawnDetachedSupervisor: %v", err)
		}
		waitForFile(t, marker)
	}

	t.Run("from a session", func(t *testing.T) {
		logPath := installInfraScopeHost(t, wrappableFromSession)
		respawn(t)
		assertNoSystemdRun(t, logPath)
	})
	t.Run("outside a session", func(t *testing.T) {
		logPath := installInfraScopeHost(t, infraScopeHost{linger: true, cgroup: userManagerCgroup})
		respawn(t)
		assertNoSystemdRun(t, logPath)
	})
}

// TestInfraArgvWrapsOnlyFromASessionWhenScopingKeepsLifetime is the rule's
// truth table. A spawn is scoped only when a user-manager scope suits the
// caller's cgroup, and never with GC_INFRA_SLICE=off: always under
// user@<uid>.service; from a login session's scope when the user lingers;
// from anywhere else (a system service or slice, a container's root) only
// when GC_INFRA_SLICE names a slice and the user lingers. A spawn that
// stays unscoped never probes the host: the probe itself creates a scope.
func TestInfraArgvWrapsOnlyFromASessionWhenScopingKeepsLifetime(t *testing.T) {
	argv := []string{"dolt", "sql-server"}
	cgroups := []struct {
		name   string
		cgroup string
	}{
		{"login_session", loginSessionCgroup},
		{"user_manager", userManagerCgroup},
		{"supervisor_slice_service", supervisorSliceServiceCgroup},
		{"system_service", systemServiceCgroup},
		{"container_root", containerRootCgroup},
	}
	for _, session := range []bool{false, true} {
		for _, linger := range []bool{false, true} {
			for _, cg := range cgroups {
				for _, sliceEnv := range []string{"", "custom-infra.slice", "off"} {
					h := infraScopeHost{session: session, linger: linger, cgroup: cg.cgroup}
					name := fmt.Sprintf("session=%t/linger=%t/cgroup=%s/GC_INFRA_SLICE=%q", session, linger, cg.name, sliceEnv)
					t.Run(name, func(t *testing.T) {
						logPath := installInfraScopeHost(t, h)
						t.Setenv(infraSliceEnv, sliceEnv)
						got, unit := infraArgv(infraTestDescription, argv)

						var scoped bool
						switch {
						case sliceEnv == "off":
						case cg.cgroup == userManagerCgroup:
							scoped = true
						case cg.cgroup == loginSessionCgroup:
							scoped = linger
						default:
							scoped = linger && sliceEnv != ""
						}
						want := argv
						if scoped {
							slice := defaultInfraSlice
							if sliceEnv != "" {
								slice = sliceEnv
							}
							want = userscope.ScopeArgv(userscope.Scope{Slice: slice, Unit: unit, Description: infraTestDescription}, argv)
						}
						if !slices.Equal(got, want) {
							t.Fatalf("infraArgv = %q, want %q", got, want)
						}
						if !scoped {
							assertNoSystemdRun(t, logPath)
						}
					})
				}
			}
		}
	}
}

// TestInfraSliceResolvesNameAndOffSwitch: GC_INFRA_SLICE names the slice, and
// its off values (those of the GC_INJECT_* switches) turn scoping off.
func TestInfraSliceResolvesNameAndOffSwitch(t *testing.T) {
	for _, tc := range []struct {
		value     string
		wantSlice string
		wantNamed bool
	}{
		{"", defaultInfraSlice, false},
		{"  ", defaultInfraSlice, false},
		{"custom-infra.slice", "custom-infra.slice", true},
		{" custom-infra.slice\n", "custom-infra.slice", true},
		// Naming the default slice still opts in callers outside the user's
		// login sessions and user manager.
		{defaultInfraSlice, defaultInfraSlice, true},
		{"off", "", false},
		{"OFF", "", false},
		{" Off ", "", false},
		{"0", "", false},
		{"false", "", false},
		{"FALSE", "", false},
	} {
		t.Run(fmt.Sprintf("%q", tc.value), func(t *testing.T) {
			t.Setenv(infraSliceEnv, tc.value)
			slice, named := infraSlice()
			if slice != tc.wantSlice || named != tc.wantNamed {
				t.Fatalf("infraSlice() = %q, %t; want %q, %t", slice, named, tc.wantSlice, tc.wantNamed)
			}
		})
	}
}

func TestInfraArgvWithoutUserManagerLeavesSpawnUnwrapped(t *testing.T) {
	installInfraScopeHost(t, wrappableFromSession)
	infraScope = &userscope.Wrapper{Probe: func(string) error { return os.ErrNotExist }}
	argv := []string{"dolt", "sql-server"}
	if got, unit := infraArgv(infraTestDescription, argv); !slices.Equal(got, argv) || unit != "" {
		t.Fatalf("infraArgv = %q, %q; want unwrapped %q and no unit", got, unit, argv)
	}
}

func TestReadInfraCallerClassifiesCgroups(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cgroup")
	for _, tc := range []struct {
		content string
		want    infraCaller
	}{
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/x.scope\n", infraCallerUserManager},
		{"12:pids:/user.slice\n1:name=systemd:/user.slice/user-1000.slice/user@1000.service/init.scope\n", infraCallerUserManager},
		{"0::/user.slice/user-1000.slice/session-2.scope\n", infraCallerLoginSession},
		{"0::/user.slice/user-1000.slice/session-c1.scope\n", infraCallerLoginSession},
		{"12:pids:/user.slice\n1:name=systemd:/user.slice/user-1000.slice/session-2.scope\n", infraCallerLoginSession},
		// Another user's units, matched by whole component.
		{"0::/user.slice/user-10000.slice/user@10000.service/x.scope\n", infraCallerElsewhere},
		{"0::/user.slice/user-10000.slice/session-2.scope\n", infraCallerElsewhere},
		// A session scope counts only directly under the user's slice.
		{"0::/system.slice/session-2.scope\n", infraCallerElsewhere},
		{"0::/user.slice/user-1000.slice\n", infraCallerElsewhere},
		{"0::/user.slice/user-1000.slice/user-runtime-dir@1000.service\n", infraCallerElsewhere},
		{"0::/system.slice/gascity-supervisor.service\n", infraCallerElsewhere},
		{"0::/gascity.slice/gascity-prod.slice/gascity-supervisor.service\n", infraCallerElsewhere},
		{"0::/\n", infraCallerElsewhere},
		{"", infraCallerElsewhere},
	} {
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := readInfraCaller(path, 1000)
		if err != nil || got != tc.want {
			t.Errorf("readInfraCaller(%q) = %d, %v; want %d", tc.content, got, err, tc.want)
		}
	}
	if got, err := readInfraCaller(filepath.Join(t.TempDir(), "missing"), 1000); err == nil || got != infraCallerElsewhere {
		t.Errorf("readInfraCaller(missing file) = %d, %v; want %d and the read error", got, err, infraCallerElsewhere)
	}
}

// TestInfraScopeSuitsCallerElsewhereNeedsANamedSlice: from a cgroup outside
// the user's login sessions and user manager an unreadable cgroup file or an
// unset GC_INFRA_SLICE keeps the spawn in place even when the user lingers,
// and a named slice opts it in.
func TestInfraScopeSuitsCallerElsewhereNeedsANamedSlice(t *testing.T) {
	t.Run("unset GC_INFRA_SLICE", func(t *testing.T) {
		installInfraScopeHost(t, infraScopeHost{session: true, linger: true, cgroup: supervisorSliceServiceCgroup})
		err := infraScopeSuitsCaller()
		if err == nil || !strings.Contains(err.Error(), infraSliceEnv+" names no slice") {
			t.Fatalf("infraScopeSuitsCaller = %v, want a names-no-slice error", err)
		}
	})
	t.Run("unreadable cgroup file", func(t *testing.T) {
		installInfraScopeHost(t, infraScopeHost{session: true, linger: true})
		infraProcCgroupPath = filepath.Join(t.TempDir(), "missing")
		err := infraScopeSuitsCaller()
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("infraScopeSuitsCaller = %v, want the cgroup read error", err)
		}
	})
	t.Run("named slice", func(t *testing.T) {
		installInfraScopeHost(t, infraScopeHost{session: true, linger: true, cgroup: supervisorSliceServiceCgroup})
		t.Setenv(infraSliceEnv, "custom-infra.slice")
		if err := infraScopeSuitsCaller(); err != nil {
			t.Fatalf("infraScopeSuitsCaller = %v, want nil with a named slice and lingering", err)
		}
	})
}

// waitForFinalImage polls until pid runs the program named name and its
// environment is in place. A /proc/<pid>/environ read that overlaps the
// process's exec returns no bytes and no error, so killFakeCgroup, which reads
// each environment once, would pass over a live process of the scope. The
// kernel shows a cmdline only once exec has laid out the new image's argv,
// and stores the end of the environment last: once both are visible the
// environment stays readable for as long as the process runs.
func waitForFinalImage(t *testing.T, pid int, name string) {
	t.Helper()
	proc := filepath.Join("/proc", strconv.Itoa(pid))
	settled := infraPollUntil(10*time.Second, func() bool {
		cmdline, err := os.ReadFile(filepath.Join(proc, "cmdline"))
		if err != nil || !bytes.HasPrefix(cmdline, []byte(name+"\x00")) {
			return false
		}
		environ, err := os.ReadFile(filepath.Join(proc, "environ"))
		return err == nil && len(environ) > 0
	})
	if !settled {
		cmdline, _ := os.ReadFile(filepath.Join(proc, "cmdline"))
		t.Fatalf("pid %d did not settle into %s with its environment; cmdline %q", pid, name, cmdline)
	}
}

// signalFakeCgroup signals every process in the fake cgroup scope, the way a
// cgroup-wide stop does: a runtime killing an agent's scope whole (SIGKILL),
// or a unit stop under KillMode=control-group (SIGTERM). It reads each
// process's environment once: a process still exec'ing is passed over, so
// wait for the target's final image first (waitForFinalImage).
func signalFakeCgroup(t *testing.T, scope string, sig syscall.Signal) {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Skipf("no /proc: %v", err)
	}
	want := []byte(fakeCgroupEnv + "=" + scope + "\x00")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		environ, err := os.ReadFile(filepath.Join("/proc", e.Name(), "environ"))
		if err != nil || !bytes.Contains(environ, want) {
			continue
		}
		_ = syscall.Kill(pid, sig)
	}
}

// killFakeCgroup kills every process in the fake cgroup scope, the way a
// runtime's cgroup-wide stop of an agent session does.
func killFakeCgroup(t *testing.T, scope string) {
	t.Helper()
	signalFakeCgroup(t, scope, syscall.SIGKILL)
}

func processAlive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	// A killed child not yet reaped is a zombie: not alive.
	stat, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return false
	}
	fields := strings.Fields(string(stat[bytes.LastIndexByte(stat, ')')+1:]))
	return len(fields) > 0 && fields[0] != "Z"
}

// TestAgentScopeKillSparesAgentStartedManagedDolt reproduces the failure: an
// agent's gc/bd call autostarts the managed Dolt server, then the runtime
// stops the agent by killing its cgroup. Before infrastructure scoping the
// server is in the agent's cgroup and dies with it; scoped, it survives.
func TestAgentScopeKillSparesAgentStartedManagedDolt(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake cgroup kill reads /proc")
	}
	for _, tc := range []struct {
		name      string
		unscoped  bool
		wantAlive bool
	}{
		{name: "without infrastructure scoping the store dies with the agent", unscoped: true, wantAlive: false},
		{name: "scoped from the session the store survives the agent", unscoped: false, wantAlive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installInfraScopeHost(t, wrappableFromSession)
			if tc.unscoped {
				disableInfraScopeForTests()
			}
			agentScope := fmt.Sprintf("agent-scope-%d-%d", os.Getpid(), time.Now().UnixNano())
			t.Setenv(fakeCgroupEnv, agentScope)

			started, _ := startDirectManagedDolt(t)
			// The fake marks itself running before it execs its final image;
			// a kill that scans it mid-exec misses it.
			waitForFinalImage(t, started.PID, fakeDoltFinalImage)
			killFakeCgroup(t, agentScope)

			// A process the kill reached is gone within moments; one it
			// spared is still running after the window.
			window := 10 * time.Second
			if tc.wantAlive {
				window = 500 * time.Millisecond
			}
			died := infraPollUntil(window, func() bool { return !processAlive(started.PID) })
			alive := !died
			if alive != tc.wantAlive {
				t.Fatalf("managed Dolt alive after the agent's cgroup kill = %t, want %t", alive, tc.wantAlive)
			}
		})
	}
}

// startCallerUnitDirectManagedDolt starts the managed Dolt server directly
// (no scope watchdog) from the caller unit's environment, as a gc call from
// a non-session user unit that autostarts it. It returns the started
// process, the fake dolt's running marker and the dolt log path.
func startCallerUnitDirectManagedDolt(t *testing.T) (managedDoltStartedProcess, string, string) {
	t.Helper()
	withManagedDoltTestMode(t, false)
	t.Setenv(managedDoltTestModeEnv, "")
	t.Setenv(managedDoltScopeWatchdogEnv, "0")
	binDir, marker := writeMarkingFakeDolt(t)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dolt-config.yaml")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	logFile, err := os.Create(filepath.Join(dir, "dolt.log"))
	if err != nil {
		t.Fatalf("create log: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	started, err := startManagedDoltSQLServer("", configPath, logFile.Name(), logFile)
	if err != nil {
		t.Fatalf("startManagedDoltSQLServer: %v", err)
	}
	killInfraScopeTestPID(t, started.PID)
	return started, marker, logFile.Name()
}

// startCallerUnitWatchdogManagedDolt starts the managed Dolt server under
// the scope watchdog from the caller unit's environment. The fake dolt dies
// only on its second SIGTERM (writeMarkingFakeDoltDyingOnSecondTERM), so a
// death proves the watchdog forwarded the stop. It returns the started
// process, the fake dolt's running marker and the dolt log path.
func startCallerUnitWatchdogManagedDolt(t *testing.T) (managedDoltStartedProcess, string, string) {
	t.Helper()
	cityPath := t.TempDir()
	binDir, marker := writeMarkingFakeDoltDyingOnSecondTERM(t)
	dir := t.TempDir()
	configPath := filepath.Join(dir, "dolt-config.yaml")
	if err := os.WriteFile(configPath, []byte("log_level: debug\n"), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	logFilePath := filepath.Join(dir, "dolt.log")
	logFile, err := os.OpenFile(logFilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open log file: %v", err)
	}
	t.Cleanup(func() { _ = logFile.Close() })
	parentEnv := removeEnvKey(os.Environ(), "PATH")
	parentEnv = append(parentEnv, "PATH="+binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	started, err := startManagedDoltSQLServerWithScopeWatchdogEnv(cityPath, configPath, logFilePath, logFile, parentEnv)
	if err != nil {
		t.Fatalf("start managed dolt with scope watchdog: %v", err)
	}
	killInfraScopeTestPID(t, started.PID)
	killInfraScopeTestPID(t, started.WatchdogPID)
	return started, marker, logFilePath
}

// TestCallerUnitStopSparesManagedDoltStartedOutsideASession reproduces
// issue #5910's non-session cases: a gc command run from a transient user
// unit that is not an agent session (a one-shot service such as
// nudge-drainer.service, a bot service) autostarts the managed Dolt server.
// Stopping that unit SIGTERMs every process in its cgroup
// (KillMode=control-group). Scoped into a scope of its own, the server (and
// its watchdog) survive the stop; without infrastructure scoping the same
// sequence kills the store, and the watchdog logs the report's signature —
// "received terminated" — as it forwards the unit stop into the server.
func TestCallerUnitStopSparesManagedDoltStartedOutsideASession(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("fake cgroup signal reads /proc")
	}
	for _, tc := range []struct {
		name      string
		watchdog  bool
		unscoped  bool
		wantAlive bool
	}{
		{name: "direct without infrastructure scoping the store dies with the unit", unscoped: true, wantAlive: false},
		{name: "direct scoped the store survives the unit stop", unscoped: false, wantAlive: true},
		{name: "scope watchdog without infrastructure scoping the store dies with the unit", watchdog: true, unscoped: true, wantAlive: false},
		{name: "scope watchdog scoped the store survives the unit stop", watchdog: true, unscoped: false, wantAlive: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installInfraScopeHost(t, infraScopeHost{linger: false, cgroup: userServiceCgroup})
			if tc.unscoped {
				disableInfraScopeForTests()
			}
			callerUnit := fmt.Sprintf("caller-unit-%d-%d", os.Getpid(), time.Now().UnixNano())
			t.Setenv(fakeCgroupEnv, callerUnit)

			var started managedDoltStartedProcess
			var marker, logFilePath string
			if tc.watchdog {
				started, marker, logFilePath = startCallerUnitWatchdogManagedDolt(t)
			} else {
				started, marker, logFilePath = startCallerUnitDirectManagedDolt(t)
			}
			waitForFile(t, marker)
			if !tc.watchdog {
				// The direct fake marks itself running before it execs its
				// final image; a signal scan over it mid-exec misses it.
				waitForFinalImage(t, started.PID, fakeDoltFinalImage)
			}
			signalFakeCgroup(t, callerUnit, syscall.SIGTERM)

			// A process the unit stop reached is gone within moments; one it
			// spared is still running after the window.
			window := 10 * time.Second
			if tc.wantAlive {
				window = 500 * time.Millisecond
			}
			died := infraPollUntil(window, func() bool { return !processAlive(started.PID) })
			if alive := !died; alive != tc.wantAlive {
				data, _ := os.ReadFile(logFilePath)
				t.Fatalf("managed Dolt alive after the caller unit's SIGTERM = %t, want %t; log:\n%s", alive, tc.wantAlive, data)
			}
			if tc.watchdog && tc.wantAlive && !processAlive(started.WatchdogPID) {
				data, _ := os.ReadFile(logFilePath)
				t.Fatalf("watchdog pid %d died with the caller unit while the store was scoped; log:\n%s", started.WatchdogPID, data)
			}
			if tc.watchdog && !tc.wantAlive {
				data, _ := os.ReadFile(logFilePath)
				if !strings.Contains(string(data), "gc scope watchdog: received terminated") {
					t.Fatalf("watchdog log missing the forwarded unit stop; log:\n%s", data)
				}
			}
		})
	}
}

// withInfraUsernameLookupFailing makes the current-user lookup fail, as it
// does for a uid with no passwd entry (containers, sandboxes).
func withInfraUsernameLookupFailing(t *testing.T) {
	t.Helper()
	old := infraCurrentUsername
	infraCurrentUsername = func() (string, error) { return "", errors.New("user: unknown userid") }
	t.Cleanup(func() { infraCurrentUsername = old })
}

// TestInfraScopeSuitsCallerUnderUserManagerWithoutPasswdEntry: being under
// user@<uid>.service suffices on its own, so a failed username lookup must
// not keep the spawn unwrapped.
func TestInfraScopeSuitsCallerUnderUserManagerWithoutPasswdEntry(t *testing.T) {
	installInfraScopeHost(t, infraScopeHost{session: true, cgroup: userManagerCgroup})
	withInfraUsernameLookupFailing(t)
	if err := infraScopeSuitsCaller(); err != nil {
		t.Fatalf("infraScopeSuitsCaller = %v, want nil under user@<uid>.service", err)
	}
	argv := []string{"dolt", "sql-server"}
	got, unit := infraArgv(infraTestDescription, argv)
	if want := infraTestScopeArgv(unit, argv); !slices.Equal(got, want) {
		t.Fatalf("infraArgv = %q, want %q", got, want)
	}
}

// TestInfraScopeSuitsCallerTreatsFailedLookupAsNotLingering: in a login
// session's scope, a uid whose username cannot be resolved is not lingering.
func TestInfraScopeSuitsCallerTreatsFailedLookupAsNotLingering(t *testing.T) {
	installInfraScopeHost(t, infraScopeHost{session: true, linger: true})
	withInfraUsernameLookupFailing(t)
	err := infraScopeSuitsCaller()
	if err == nil {
		t.Fatal("infraScopeSuitsCaller = nil, want a not-lingering error outside user@<uid>.service")
	}
	if !strings.Contains(err.Error(), "does not linger") {
		t.Fatalf("infraScopeSuitsCaller = %v, want a not-lingering error", err)
	}
	argv := []string{"dolt", "sql-server"}
	if got, unit := infraArgv(infraTestDescription, argv); !slices.Equal(got, argv) || unit != "" {
		t.Fatalf("infraArgv = %q, %q; want unwrapped %q and no unit", got, unit, argv)
	}
}

// TestInfraCommandKeepsImmediateErrorForMissingExecutable: wrapping would
// make exec resolve systemd-run, hiding a missing executable until a
// readiness timeout. A name that does not resolve stays unwrapped so Start
// fails at once.
func TestInfraCommandKeepsImmediateErrorForMissingExecutable(t *testing.T) {
	logPath := installInfraScopeHost(t, wrappableFromSession)
	cmd, unit := infraCommand(infraTestDescription, "gc-infra-scope-test-no-such-binary", "sql-server")
	if unit != "" {
		t.Fatalf("unresolvable executable recorded infra scope %q, want none", unit)
	}
	if want := []string{"gc-infra-scope-test-no-such-binary", "sql-server"}; !slices.Equal(cmd.Args, want) {
		t.Fatalf("infraCommand args = %q, want unwrapped %q", cmd.Args, want)
	}
	err := cmd.Start()
	if !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("Start = %v, want exec.ErrNotFound", err)
	}
	assertNoSystemdRun(t, logPath)
}

// TestInfraCommandWrapsResolvableExecutable pins the other side: a name on
// PATH is wrapped from a session.
func TestInfraCommandWrapsResolvableExecutable(t *testing.T) {
	installInfraScopeHost(t, wrappableFromSession)
	cmd, unit := infraCommand(infraTestDescription, "sh", "-c", "true")
	if want := infraTestScopeArgv(unit, []string{"sh", "-c", "true"}); !slices.Equal(cmd.Args, want) {
		t.Fatalf("infraCommand args = %q, want %q", cmd.Args, want)
	}
}

// TestInfraArgvHonorsInfraSliceEnv: GC_INFRA_SLICE names the slice scopes are
// created under.
func TestInfraArgvHonorsInfraSliceEnv(t *testing.T) {
	installInfraScopeHost(t, wrappableFromSession)
	t.Setenv(infraSliceEnv, "custom-infra.slice")
	argv := []string{"dolt", "sql-server"}
	got, unit := infraArgv(infraTestDescription, argv)
	want := userscope.ScopeArgv(userscope.Scope{Slice: "custom-infra.slice", Unit: unit, Description: infraTestDescription}, argv)
	if !slices.Equal(got, want) {
		t.Fatalf("infraArgv = %q, want %q", got, want)
	}
}

// TestInfraUnitNamesAreUniquePerSpawn: a fixed unit name would collide with
// a previous spawn's scope that is still active (or not yet collected).
func TestInfraUnitNamesAreUniquePerSpawn(t *testing.T) {
	installInfraScopeHost(t, wrappableFromSession)
	_, first := infraArgv(infraTestDescription, []string{"dolt"})
	_, second := infraArgv(infraTestDescription, []string{"dolt"})
	for _, unit := range []string{first, second} {
		if !infraUnitPattern.MatchString(unit) {
			t.Fatalf("unit %q does not match %s", unit, infraUnitPattern)
		}
	}
	if first == second {
		t.Fatalf("two spawns got the same scope unit %q", first)
	}
}
