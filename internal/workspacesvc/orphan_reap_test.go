package workspacesvc

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// spawnOrphanForTest spawns argv detached through an intermediate shell so
// the child re-parents to init (ppid 1), simulating a service process that
// survived a supervisor hard exit. extraEnv entries are appended to the
// orphan's environment. Skips the test on hosts where a child-subreaper
// intercepts re-parenting, since the production filter requires ppid 1.
func spawnOrphanForTest(t *testing.T, argv []string, extraEnv []string) int {
	t.Helper()
	args := append([]string{"-c", `"$@" >/dev/null 2>&1 & echo $!`, "gc-orphan-spawner"}, argv...)
	cmd := exec.Command("sh", args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("spawn orphan: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("parse orphan pid from %q: %v", out, err)
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	deadline := time.Now().Add(2 * time.Second)
	for {
		ppid, err := processParentPIDForTest(pid)
		if err != nil {
			t.Fatalf("orphan %d exited before re-parenting: %v", pid, err)
		}
		if ppid == 1 {
			return pid
		}
		if time.Now().After(deadline) {
			t.Skipf("orphan %d re-parented to %d, not init; host has a child subreaper", pid, ppid)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func processParentPIDForTest(pid int) (int, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	fields := strings.Fields(string(data[strings.LastIndexByte(string(data), ')')+1:]))
	if len(fields) < 2 {
		return 0, fmt.Errorf("malformed stat for pid %d", pid)
	}
	return strconv.Atoi(fields[1])
}

func processAliveForTest(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

func waitProcessGoneForTest(t *testing.T, pid int, timeout time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !processAliveForTest(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !processAliveForTest(pid)
}

func TestReapOrphanedServiceProcessesKillsPPID1CommandMatch(t *testing.T) {
	marker := fmt.Sprintf("86340.0%d", os.Getpid())
	command := []string{"sleep", marker}
	pid := spawnOrphanForTest(t, command, []string{"GC_SERVICE_NAME=orphan-reap-test"})

	reapOrphanedServiceProcesses("orphan-reap-test", command)

	if !waitProcessGoneForTest(t, pid, 3*time.Second) {
		t.Fatalf("orphan %d still alive after reap", pid)
	}
}

func TestReapOrphanedServiceProcessesSkipsNonMatches(t *testing.T) {
	marker := fmt.Sprintf("86341.0%d", os.Getpid())
	command := []string{"sleep", marker}

	// Same argv, but the environ marker names a different service: another
	// service's orphan must not be reaped by this service's sweep.
	otherService := spawnOrphanForTest(t, command, []string{"GC_SERVICE_NAME=some-other-service"})
	// Same argv, no GC_SERVICE_NAME marker at all: not provably a gc
	// service spawn, so it must survive.
	unmarked := spawnOrphanForTest(t, command, nil)
	// Matching argv and marker but still parented to this test process:
	// a live supervised child, not an orphan.
	supervised := exec.Command(command[0], command[1:]...)
	supervised.Env = append(os.Environ(), "GC_SERVICE_NAME=orphan-reap-test")
	if err := supervised.Start(); err != nil {
		t.Fatalf("start supervised child: %v", err)
	}
	t.Cleanup(func() {
		_ = supervised.Process.Kill()
		_ = supervised.Wait()
	})
	// Orphan with the right marker but a different argv.
	differentArgv := spawnOrphanForTest(t, []string{"sleep", marker, "0"}, []string{"GC_SERVICE_NAME=orphan-reap-test"})

	reapOrphanedServiceProcesses("orphan-reap-test", command)

	// Give any wrongly-issued SIGTERM time to land before asserting.
	time.Sleep(200 * time.Millisecond)
	for name, pid := range map[string]int{
		"other-service orphan":  otherService,
		"unmarked process":      unmarked,
		"supervised child":      supervised.Process.Pid,
		"different-argv orphan": differentArgv,
	} {
		if !processAliveForTest(pid) {
			t.Errorf("%s (pid %d) was killed; reap matched too broadly", name, pid)
		}
	}
}

// TestProxyProcessStartReapsOrphanedDuplicates verifies the spawn-path
// wiring: before a proxy_process child is spawned, survivors of a previous
// supervisor hard exit running the same service command are terminated so
// duplicates never accumulate (ga-mukg0s; ~39 observed in production).
func TestProxyProcessStartReapsOrphanedDuplicates(t *testing.T) {
	t.Setenv("GC_SERVICE_HELPER", "1")
	setHelperPassthrough(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("Executable: %v", err)
	}
	command := []string{exe, "-test.run=^TestProxyProcessHelper$", "--"}

	// The orphan is a literal leftover service instance: same binary, same
	// argv, GC_SERVICE_NAME marker in its environment, serving on a stale
	// socket, re-parented to init.
	orphanDir, err := os.MkdirTemp("", "gcorph")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(orphanDir) })
	orphanSocket := orphanDir + "/o.sock"
	orphanPID := spawnOrphanForTest(t, command, []string{
		"GC_SERVICE_HELPER=1",
		"GC_SERVICE_NAME=bridge",
		"GC_SERVICE_SOCKET=" + orphanSocket,
	})
	// Wait until the orphan is serving so it provably lingers on its own.
	dialDeadline := time.Now().Add(5 * time.Second)
	for {
		if conn, err := net.DialTimeout("unix", orphanSocket, 100*time.Millisecond); err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(dialDeadline) {
			t.Fatalf("orphan helper (pid %d) never started serving", orphanPID)
		}
		time.Sleep(20 * time.Millisecond)
	}

	rt := &testRuntime{
		cityPath: t.TempDir(),
		cityName: "test-city",
		cfg: &config.City{
			Services: []config.Service{{
				Name: "bridge",
				Kind: "proxy_process",
				Process: config.ServiceProcessConfig{
					Command:    command,
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
	defer mgr.Close() //nolint:errcheck // best-effort cleanup

	if !waitProcessGoneForTest(t, orphanPID, 5*time.Second) {
		t.Fatalf("orphaned duplicate (pid %d) still alive after service start", orphanPID)
	}
	status, ok := mgr.Get("bridge")
	if !ok {
		t.Fatal("service status missing")
	}
	if status.LocalState != "ready" {
		t.Fatalf("LocalState = %q, want ready (reason=%q)", status.LocalState, status.Reason)
	}
}
