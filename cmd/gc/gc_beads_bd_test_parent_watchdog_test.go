package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testParentWatchdogFixture is a stand-in test parent and a stand-in dolt
// sql-server, each a child of this test so it can watch them exit.
type testParentWatchdogFixture struct {
	parent     *exec.Cmd
	parentDone chan struct{}
	server     *exec.Cmd
	serverDone chan struct{}
}

// startTestParentWatchdogFixture starts the fixture with its server's files
// in a temp directory, under a subdirectory named dirName unless it is empty.
func startTestParentWatchdogFixture(t *testing.T, dirName string) *testParentWatchdogFixture {
	t.Helper()
	dir := t.TempDir()
	if dirName != "" {
		dir = filepath.Join(dir, dirName)
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// The command line reads "<dir>/dolt sql-server --config ...", like the
	// server the script launches.
	fakeDolt := filepath.Join(dir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\ntrap 'exit 0' TERM\nwhile :; do sleep 0.1; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "dolt-config.yaml")
	f := &testParentWatchdogFixture{
		parent:     exec.Command("sleep", "300"),
		parentDone: make(chan struct{}),
		server:     exec.Command(fakeDolt, "sql-server", "--config", config),
		serverDone: make(chan struct{}),
	}
	for _, c := range []struct {
		cmd  *exec.Cmd
		done chan struct{}
	}{{f.parent, f.parentDone}, {f.server, f.serverDone}} {
		c.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err := c.cmd.Start(); err != nil {
			t.Fatalf("start %s: %v", c.cmd.Path, err)
		}
		// Reap here: an unreaped child stays a zombie that kill -0 still finds.
		go func(cmd *exec.Cmd, done chan struct{}) {
			_ = cmd.Wait()
			close(done)
		}(c.cmd, c.done)
	}
	t.Cleanup(func() {
		_ = f.parent.Process.Kill()
		_ = f.server.Process.Kill()
		<-f.parentDone
		<-f.serverDone
	})
	return f
}

// runTestParentWatchdog runs start_dolt_test_parent_watchdog from
// gc-beads-bd.sh for the fixture's server pid, with each KEY=VALUE of
// extraEnv replacing KEY in the environment. It fails the test if the
// function does not return promptly with its output pipes released: a
// watchdog that kept them would hold the provider script's caller until the
// test parent exits.
func runTestParentWatchdog(t *testing.T, f *testParentWatchdogFixture, testMode bool, extraEnv ...string) {
	t.Helper()
	scriptBytes, err := os.ReadFile(gcBinGuardScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fnSrc := extractShellFunction(t, string(scriptBytes), "start_dolt_test_parent_watchdog")
	drop := []string{managedDoltTestModeEnv, managedDoltTestParentPIDEnv}
	for _, kv := range extraEnv {
		drop = append(drop, strings.SplitN(kv, "=", 2)[0])
	}
	var env []string
	for _, kv := range os.Environ() {
		if !slices.Contains(drop, strings.SplitN(kv, "=", 2)[0]) {
			env = append(env, kv)
		}
	}
	env = append(env, extraEnv...)
	if testMode {
		env = append(env,
			managedDoltTestModeEnv+"=1",
			managedDoltTestParentPIDEnv+"="+strconv.Itoa(f.parent.Process.Pid),
		)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", fnSrc+"\nstart_dolt_test_parent_watchdog \"$1\"\n", "watchdog", strconv.Itoa(f.server.Process.Pid))
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("start_dolt_test_parent_watchdog did not return within 10s (output pipe held?): %q", out)
	}
	if err != nil {
		t.Fatalf("start_dolt_test_parent_watchdog: %v: %q", err, out)
	}
}

func waitClosed(ch <-chan struct{}, d time.Duration) bool {
	select {
	case <-ch:
		return true
	case <-time.After(d):
		return false
	}
}

// TestGCBeadsBDTestParentWatchdogStopsServerWhenParentExits covers the dolt
// sql-server gc-beads-bd.sh starts itself when it has no gc helper (GC_BIN
// unset, as in every cmd/gc test binary). In managed-Dolt test mode the server
// must stop when the test parent exits, however it exits: a `go test -timeout`
// panic or a SIGKILL skips every in-process cleanup, and the server used to
// outlive the run. That holds for a server path with a non-ASCII byte under
// the C locale too (an unset LANG, as under launchd or env -i), where ps
// prints the server's command line with that byte escaped.
func TestGCBeadsBDTestParentWatchdogStopsServerWhenParentExits(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		dirName string
		env     []string
	}{
		{"ascii path", "", nil},
		{"non-ascii path under the C locale", "caf\u00e9", []string{"LC_ALL=C", "LANG="}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := startTestParentWatchdogFixture(t, tc.dirName)
			runTestParentWatchdog(t, f, true, tc.env...)

			if waitClosed(f.serverDone, 2500*time.Millisecond) {
				t.Fatal("server stopped while its test parent was still running")
			}
			if err := f.parent.Process.Kill(); err != nil {
				t.Fatalf("kill parent: %v", err)
			}
			<-f.parentDone
			if !waitClosed(f.serverDone, 10*time.Second) {
				t.Fatal("server still running 10s after its test parent exited")
			}
		})
	}
}

// TestGCBeadsBDTestParentWatchdogOffOutsideTestMode keeps production servers
// untouched: without GC_MANAGED_DOLT_TEST_MODE=1 no watchdog starts.
func TestGCBeadsBDTestParentWatchdogOffOutsideTestMode(t *testing.T) {
	t.Parallel()
	f := startTestParentWatchdogFixture(t, "")
	runTestParentWatchdog(t, f, false)

	if err := f.parent.Process.Kill(); err != nil {
		t.Fatalf("kill parent: %v", err)
	}
	<-f.parentDone
	if waitClosed(f.serverDone, 2500*time.Millisecond) {
		t.Fatal("server stopped outside managed-Dolt test mode")
	}
}

// TestGCBeadsBDTestParentWatchdogSparesReusedPID covers pid reuse: the
// server the watchdog was armed for dies while its test parent runs, and its
// pid goes to another dolt sql-server (a parallel test's, or the host's own
// city server) before the next poll. When the test parent exits, the watchdog
// must leave that server alone, because it was started after the one the
// watchdog was armed for. A stub ps stands in for the reuse: once armed, it
// reports another start time for the server's pid.
func TestGCBeadsBDTestParentWatchdogSparesReusedPID(t *testing.T) {
	t.Parallel()
	realPS, err := exec.LookPath("ps")
	if err != nil {
		t.Skipf("ps not found: %v", err)
	}
	stubDir := t.TempDir()
	reused := filepath.Join(stubDir, "reused-lstart")
	stub := "#!/bin/sh\n" +
		"case \" $* \" in *\" lstart= \"*) if [ -f '" + reused + "' ]; then cat '" + reused + "'; exit 0; fi ;; esac\n" +
		"exec '" + realPS + "' \"$@\"\n"
	if err := os.WriteFile(filepath.Join(stubDir, "ps"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}

	f := startTestParentWatchdogFixture(t, "")
	runTestParentWatchdog(t, f, true, "PATH="+stubDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if err := os.WriteFile(reused, []byte("Thu Jan  1 00:00:00 1970\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := f.parent.Process.Kill(); err != nil {
		t.Fatalf("kill parent: %v", err)
	}
	<-f.parentDone
	if waitClosed(f.serverDone, 4*time.Second) {
		t.Fatal("watchdog stopped a dolt sql-server started after the one it was armed for")
	}
}

// TestGCBeadsBDShellStartArmsTestParentWatchdog pins the wiring: the shell
// launch of dolt sql-server hands its pid to the watchdog.
func TestGCBeadsBDShellStartArmsTestParentWatchdog(t *testing.T) {
	t.Parallel()
	scriptBytes, err := os.ReadFile(gcBinGuardScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	launch := regexp.MustCompile(`(?m)^\s*nohup sh -c '[^']*exec dolt sql-server[^\n]*&\n\s*local server_pid=\$!\n\s*start_dolt_test_parent_watchdog "\$server_pid"\n`)
	if !launch.Match(scriptBytes) {
		t.Fatal(`gc-beads-bd.sh: the shell dolt sql-server launch is not followed by start_dolt_test_parent_watchdog "$server_pid"`)
	}
}
