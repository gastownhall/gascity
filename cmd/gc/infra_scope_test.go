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

// fakeInfraScopeName is the fake scope the fake systemd-run moves commands to.
const fakeInfraScopeName = "fake-infra-scope"

// infraScopeHost describes the host facts the infrastructure scope rule
// reads.
type infraScopeHost struct {
	session      bool // the gc process carries GC_SESSION_ID
	linger       bool // /var/lib/systemd/linger/<user> exists
	underUserMgr bool // /proc/self/cgroup is under user@<uid>.service
}

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
		t.Setenv(infraSessionIdentityEnv, "gc-test-session")
	} else {
		t.Setenv(infraSessionIdentityEnv, "")
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
	uid := strconv.Itoa(os.Getuid())
	cgroup := "0::/user.slice/user-" + uid + ".slice/session-3.scope\n"
	if h.underUserMgr {
		cgroup = "0::/user.slice/user-" + uid + ".slice/user@" + uid + ".service/app.slice/run-r1.scope\n"
	}
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

// writeMarkingFakeDolt writes a fake dolt that records it is running, then
// stays up like a sql-server. It returns the bin dir and the marker path.
func writeMarkingFakeDolt(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	marker := filepath.Join(dir, "dolt.started")
	script := "#!/bin/sh\n" +
		"[ \"$1\" = sql-server ] || { echo \"unexpected dolt args: $*\" >&2; exit 2; }\n" +
		": > '" + marker + "'\n" +
		"exec sleep 60\n"
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
	t.Run("from a session", func(t *testing.T) {
		logPath := installInfraScopeHost(t, wrappableFromSession)
		started, configPath := startDirectManagedDolt(t)
		waitForSystemdRunInvocation(t, logPath, " -- dolt sql-server --config "+configPath)
		assertScopedInvocation(t, logPath, started.InfraScope, "gascity infra: managed dolt")
	})
	t.Run("outside a session", func(t *testing.T) {
		logPath := installInfraScopeHost(t, infraScopeHost{linger: true, underUserMgr: true})
		started, _ := startDirectManagedDolt(t)
		assertNoSystemdRun(t, logPath)
		if started.InfraScope != "" {
			t.Fatalf("unwrapped spawn recorded infra scope %q, want none", started.InfraScope)
		}
	})
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
// session-scoped keys) is set, GC_SESSION_ID is not. Its Dolt stays where
// the supervisor's tree runs.
func TestSupervisorLaunchedProviderSpawnStaysUnwrapped(t *testing.T) {
	logPath := installInfraScopeHost(t, infraScopeHost{linger: true, underUserMgr: true})
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

	t.Run("from a session", func(t *testing.T) {
		logPath := installInfraScopeHost(t, wrappableFromSession)
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
	t.Run("outside a session", func(t *testing.T) {
		logPath := installInfraScopeHost(t, infraScopeHost{linger: true, underUserMgr: true})
		_, started := start(t)
		assertNoSystemdRun(t, logPath)
		if started.InfraScope != "" {
			t.Fatalf("unwrapped watchdog recorded infra scope %q, want none", started.InfraScope)
		}
	})
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
		logPath := installInfraScopeHost(t, infraScopeHost{linger: true, underUserMgr: true})
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
		logPath := installInfraScopeHost(t, infraScopeHost{linger: true, underUserMgr: true})
		respawn(t)
		assertNoSystemdRun(t, logPath)
	})
}

// TestInfraArgvWrapsOnlyFromASessionWhenScopingKeepsLifetime is the rule's
// truth table: a spawn is scoped only from inside an agent session, and only
// when a user-manager scope lives as long as the caller's own cgroup
// (lingering, or the caller already under user@<uid>.service).
func TestInfraArgvWrapsOnlyFromASessionWhenScopingKeepsLifetime(t *testing.T) {
	argv := []string{"dolt", "sql-server"}
	for _, session := range []bool{false, true} {
		for _, linger := range []bool{false, true} {
			for _, underUserMgr := range []bool{false, true} {
				h := infraScopeHost{session: session, linger: linger, underUserMgr: underUserMgr}
				name := fmt.Sprintf("session=%t/linger=%t/under_user_manager=%t", session, linger, underUserMgr)
				t.Run(name, func(t *testing.T) {
					logPath := installInfraScopeHost(t, h)
					got, unit := infraArgv(infraTestDescription, argv)
					want := argv
					if session && (linger || underUserMgr) {
						want = infraTestScopeArgv(unit, argv)
					}
					if !slices.Equal(got, want) {
						t.Fatalf("infraArgv = %q, want %q", got, want)
					}
					if !session {
						// Outside a session the host is never probed.
						assertNoSystemdRun(t, logPath)
					}
				})
			}
		}
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

func TestCgroupUnderMatchesWholeComponent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cgroup")
	for _, tc := range []struct {
		content string
		want    bool
	}{
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/x.scope\n", true},
		{"12:pids:/user.slice\n1:name=systemd:/user.slice/user-1000.slice/user@1000.service/init.scope\n", true},
		{"0::/user.slice/user-1000.slice/session-2.scope\n", false},
		{"0::/user.slice/user-10000.slice/user@10000.service/x.scope\n", false},
		{"0::/system.slice/gascity-supervisor.service\n", false},
	} {
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatal(err)
		}
		got, err := cgroupUnder(path, "user@1000.service")
		if err != nil || got != tc.want {
			t.Errorf("cgroupUnder(%q) = %t, %v; want %t", tc.content, got, err, tc.want)
		}
	}
}

// killFakeCgroup kills every process in the fake cgroup scope, the way a
// runtime's cgroup-wide stop of an agent session does.
func killFakeCgroup(t *testing.T, scope string) {
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
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
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

// withInfraUsernameLookupFailing makes the current-user lookup fail, as it
// does for a uid with no passwd entry (containers, sandboxes).
func withInfraUsernameLookupFailing(t *testing.T) {
	t.Helper()
	old := infraCurrentUsername
	infraCurrentUsername = func() (string, error) { return "", errors.New("user: unknown userid") }
	t.Cleanup(func() { infraCurrentUsername = old })
}

// TestInfraScopeKeepsLifetimeUnderUserManagerWithoutPasswdEntry: being under
// user@<uid>.service suffices on its own, so a failed username lookup must
// not keep the spawn unwrapped.
func TestInfraScopeKeepsLifetimeUnderUserManagerWithoutPasswdEntry(t *testing.T) {
	installInfraScopeHost(t, infraScopeHost{session: true, underUserMgr: true})
	withInfraUsernameLookupFailing(t)
	if err := infraScopeKeepsLifetime(); err != nil {
		t.Fatalf("infraScopeKeepsLifetime = %v, want nil under user@<uid>.service", err)
	}
	argv := []string{"dolt", "sql-server"}
	got, unit := infraArgv(infraTestDescription, argv)
	if want := infraTestScopeArgv(unit, argv); !slices.Equal(got, want) {
		t.Fatalf("infraArgv = %q, want %q", got, want)
	}
}

// TestInfraScopeKeepsLifetimeTreatsFailedLookupAsNotLingering: outside the
// user manager, a uid whose username cannot be resolved is not lingering.
func TestInfraScopeKeepsLifetimeTreatsFailedLookupAsNotLingering(t *testing.T) {
	installInfraScopeHost(t, infraScopeHost{session: true, linger: true})
	withInfraUsernameLookupFailing(t)
	err := infraScopeKeepsLifetime()
	if err == nil {
		t.Fatal("infraScopeKeepsLifetime = nil, want a not-lingering error outside user@<uid>.service")
	}
	if !strings.Contains(err.Error(), "does not linger") {
		t.Fatalf("infraScopeKeepsLifetime = %v, want a not-lingering error", err)
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

// TestProviderLifecycleEnvCarriesSessionIdentity: an agent's gc autostarts
// the store through the provider script (gc-beads-bd.sh start → gc
// dolt-state start-managed). That inner gc scopes the Dolt server only if
// the provider env passes GC_SESSION_ID through, and must not invent one.
func TestProviderLifecycleEnvCarriesSessionIdentity(t *testing.T) {
	for _, session := range []bool{true, false} {
		t.Run(fmt.Sprintf("session=%t", session), func(t *testing.T) {
			cityPath := t.TempDir()
			if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[beads]\nprovider = \"bd\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			want := ""
			if session {
				want = "gc-test-session"
			}
			// An empty value is no session identity to infraArgv.
			t.Setenv(infraSessionIdentityEnv, want)
			env := runtimeEnvEntriesToMap(mustProviderLifecycleProcessEnv(t, cityPath, "exec:"+gcBeadsBdScriptPath(cityPath)))
			if got := env[infraSessionIdentityEnv]; got != want {
				t.Fatalf("provider env %s = %q, want %q", infraSessionIdentityEnv, got, want)
			}
		})
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
