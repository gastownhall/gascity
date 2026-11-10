//go:build linux

package workspacesvc

// The contract under test is Pdeathsig: the kernel signals a child when its
// parent dies, which is what makes a proxy_process grandchild fall over when
// the test watchdog hard-exits its parent. Pdeathsig is a Linux facility with
// no portable equivalent — a Windows tree is confined by a Job Object, which
// internal/processgroup covers directly — so this pair is built only there.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestProxyProcessHardExitHarness is re-exec'd as a subprocess by
// TestProxyProcessSurvivesHardParentExit. It starts a real proxy_process
// child, writes that child's pid to GC_HARD_EXIT_PIDFILE, then calls
// os.Exit directly with zero cleanup — reproducing exactly what the Go
// test watchdog does on a -timeout kill, deliberately skipping every
// defer and t.Cleanup in the process (including Manager.Close).
func TestProxyProcessHardExitHarness(t *testing.T) {
	if os.Getenv("GC_HARD_EXIT_HARNESS") != "1" {
		t.Skip("harness process")
	}
	setHelperPassthrough(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	cityDir := os.Getenv("GC_HARD_EXIT_CITYDIR")
	if cityDir == "" {
		t.Fatal("GC_HARD_EXIT_CITYDIR not set")
	}
	pidFile := os.Getenv("GC_HARD_EXIT_PIDFILE")
	if pidFile == "" {
		t.Fatal("GC_HARD_EXIT_PIDFILE not set")
	}

	rt := &testRuntime{
		cityPath: cityDir,
		cityName: "test-city",
		cfg: &config.City{
			Services: []config.Service{{
				Name: "bridge",
				Kind: "proxy_process",
				Process: config.ServiceProcessConfig{
					Command:    []string{exe, "-test.run=^TestProxyProcessHelper$", "--"},
					HealthPath: "/healthz",
				},
			}},
		},
		sp:    runtime.NewFake(),
		store: beads.NewMemStore(),
	}
	mgr := NewManager(rt)
	if err := mgr.Reload(); err != nil {
		t.Fatalf("Reload: %v", err)
	}

	pid := proxyProcessInstancePID(t, mgr, "bridge")
	if pid == 0 {
		t.Fatal("started grandchild has pid 0")
	}
	if err := os.WriteFile(pidFile, []byte(strconv.Itoa(pid)), 0o600); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}

	os.Exit(1)
}

// TestProxyProcessSurvivesHardParentExit is the RED test for ga-9br097's
// Family A acceptance criterion: a proxy_process child spawned by start()
// must not survive its parent's hard exit (the Go -timeout watchdog's
// os.Exit path, which runs no defer/t.Cleanup anywhere in the process).
// It re-execs this test binary as a harness (TestProxyProcessHardExitHarness)
// that starts a real child and then os.Exit(1)s with zero cleanup, then
// asserts the grandchild is gone. start() does not set Pdeathsig today, so
// this must fail.
func TestProxyProcessSurvivesHardParentExit(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	stateDir := t.TempDir()
	pidFile := filepath.Join(stateDir, "grandchild.pid")

	cmd := exec.Command(exe, "-test.run=^TestProxyProcessHardExitHarness$", "--")
	cmd.Env = append(os.Environ(),
		"GC_HARD_EXIT_HARNESS=1",
		"GC_SERVICE_HELPER=1",
		"GC_HARD_EXIT_CITYDIR="+stateDir,
		"GC_HARD_EXIT_PIDFILE="+pidFile,
	)
	out, runErr := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if runErr != nil && !errors.As(runErr, &exitErr) {
		t.Fatalf("run harness: %v\n%s", runErr, out)
	}

	pidBytes, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("harness did not report a grandchild pid (harness output below):\n%s\nerr: %v", out, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(pidBytes)))
	if err != nil {
		t.Fatalf("parse pidfile %q: %v", pidBytes, err)
	}

	// Pdeathsig delivery is asynchronous; poll for death rather than
	// asserting immediately.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL) // don't leak this test's own reproduction
	t.Fatalf("grandchild pid %d still alive 5s after harness hard-exited with no cleanup", pid)
}
