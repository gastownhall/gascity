package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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

func startTestParentWatchdogFixture(t *testing.T) *testParentWatchdogFixture {
	t.Helper()
	dir := t.TempDir()
	// The command line reads "<dir>/dolt sql-server --config ...", which is what
	// the watchdog checks before it signals the pid.
	fakeDolt := filepath.Join(dir, "dolt")
	if err := os.WriteFile(fakeDolt, []byte("#!/bin/sh\ntrap 'exit 0' TERM\nwhile :; do sleep 0.1; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := &testParentWatchdogFixture{
		parent:     exec.Command("sleep", "300"),
		parentDone: make(chan struct{}),
		server:     exec.Command(fakeDolt, "sql-server", "--config", filepath.Join(dir, "dolt-config.yaml")),
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
// gc-beads-bd.sh for the fixture's server. It fails the test if the function
// does not return promptly with its output pipes released: a watchdog that
// kept them would hold the provider script's caller until the test parent
// exits.
func runTestParentWatchdog(t *testing.T, f *testParentWatchdogFixture, testMode bool) {
	t.Helper()
	scriptBytes, err := os.ReadFile(gcBinGuardScriptPath(t))
	if err != nil {
		t.Fatalf("read script: %v", err)
	}
	fnSrc := extractShellFunction(t, string(scriptBytes), "start_dolt_test_parent_watchdog")
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, managedDoltTestModeEnv+"=") || strings.HasPrefix(kv, managedDoltTestParentPIDEnv+"=") {
			continue
		}
		env = append(env, kv)
	}
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
// outlive the run.
func TestGCBeadsBDTestParentWatchdogStopsServerWhenParentExits(t *testing.T) {
	t.Parallel()
	f := startTestParentWatchdogFixture(t)
	runTestParentWatchdog(t, f, true)

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
}

// TestGCBeadsBDTestParentWatchdogOffOutsideTestMode keeps production servers
// untouched: without GC_MANAGED_DOLT_TEST_MODE=1 no watchdog starts.
func TestGCBeadsBDTestParentWatchdogOffOutsideTestMode(t *testing.T) {
	t.Parallel()
	f := startTestParentWatchdogFixture(t)
	runTestParentWatchdog(t, f, false)

	if err := f.parent.Process.Kill(); err != nil {
		t.Fatalf("kill parent: %v", err)
	}
	<-f.parentDone
	if waitClosed(f.serverDone, 2500*time.Millisecond) {
		t.Fatal("server stopped outside managed-Dolt test mode")
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
