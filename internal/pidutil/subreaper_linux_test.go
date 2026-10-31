//go:build linux

package pidutil

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// subreaperHelperEnv selects the role a re-executed test binary plays in
// TestDetectSubreapersFindsAnAncestorSubreaper.
const subreaperHelperEnv = "GC_PIDUTIL_SUBREAPER_ROLE"

// runTestAs re-executes this test binary to run only t's test, in role.
func runTestAs(t *testing.T, role string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+t.Name()+"$", "-test.count=1", "-test.v")
	cmd.Env = append(bazeltest.HelperProcessEnv(os.Environ()), subreaperHelperEnv+"="+role)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s role failed: %v\n%s", role, err, out)
	}
	if !strings.Contains(string(out), "--- PASS: "+t.Name()) {
		t.Fatalf("%s role did not run the test:\n%s", role, out)
	}
}

// An orphan is adopted by the nearest subreaper ancestor of its dead parent,
// which need not be pid 1 or a process named systemd: under Bazel's local
// process-wrapper, a container init or any PR_SET_CHILD_SUBREAPER supervisor,
// it is that process. A helper parent marks itself a subreaper and runs the
// scanner below it, the way process-wrapper runs a test action; the scanner
// orphans a grandchild and must recognize the helper as its adopter.
func TestDetectSubreapersFindsAnAncestorSubreaper(t *testing.T) {
	switch os.Getenv(subreaperHelperEnv) {
	case "":
		runTestAs(t, "subreaper")
	case "subreaper":
		if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
			t.Fatalf("PR_SET_CHILD_SUBREAPER: %v", err)
		}
		runTestAs(t, "scanner")
	case "scanner":
		orphanPID := startOrphan(t)
		helper := os.Getppid()
		ppid, err := ParentPID(orphanPID)
		if err != nil {
			t.Fatalf("reading parent of orphan %d: %v", orphanPID, err)
		}
		if ppid != helper {
			t.Fatalf("orphan %d has parent %d (%s), want the helper subreaper %d", orphanPID, ppid, Comm(ppid), helper)
		}
		if Comm(helper) == "systemd" {
			t.Fatalf("helper subreaper %d is named systemd; the test no longer models a non-systemd subreaper", helper)
		}
		subreapers := DetectSubreapers(os.Getpid())
		if !subreapers.Contains(helper) {
			t.Fatalf("DetectSubreapers(self) = %v, want it to contain the helper subreaper %d", subreapers, helper)
		}
		if subreapers.Contains(os.Getpid()) {
			t.Fatalf("DetectSubreapers(self) = %v contains the caller itself", subreapers)
		}
		if !IsReparentedOrphan(ppid, subreapers) {
			t.Fatalf("IsReparentedOrphan(%d, %v) = false for an orphan adopted by the helper subreaper", ppid, subreapers)
		}
	default:
		t.Fatalf("unknown %s=%q", subreaperHelperEnv, os.Getenv(subreaperHelperEnv))
	}
}

// startOrphan starts a sleep under a shell that exits at once, so the sleep
// is orphaned, and returns the sleep's pid. The kernel reparents the sleep as
// the shell exits, before the shell can be reaped, so the sleep already has
// its adopter when this returns. The sleep is killed at cleanup.
func startOrphan(t *testing.T) int {
	t.Helper()
	out, err := exec.Command("sh", "-c", "sleep 60 </dev/null >/dev/null 2>&1 & echo $!").Output()
	if err != nil {
		t.Fatalf("starting orphan: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil || pid <= 1 {
		t.Fatalf("orphan pid %q: %v", out, err)
	}
	t.Cleanup(func() { _ = unix.Kill(pid, unix.SIGKILL) })
	return pid
}
