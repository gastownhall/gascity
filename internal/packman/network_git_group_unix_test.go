//go:build !windows

package packman

import (
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestDefaultRunNetworkGitKillsDescendants pins that the deadline reaches git's
// children, not just git.
//
// The first version of this bound relied on cmd.WaitDelay, whose contract is to
// close the parent's ends of the I/O pipes and kill the command's own process.
// It does not signal descendants. So the call returned on time while
// git-remote-http and index-pack stayed alive — still writing into the cache
// directory whose write lock this call had just released. The next process to
// take that lock can RemoveAll a tree a live orphan is repopulating, which is
// the same corruption the lock exists to prevent, arriving by a different door.
//
// Returning on time is therefore not sufficient. The tree has to be dead.
func TestDefaultRunNetworkGitKillsDescendants(t *testing.T) {
	remote, childPIDPath := wedgedGit(t)

	restore := networkGitTimeout
	networkGitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { networkGitTimeout = restore })
	restoreWait := networkGitWaitDelay
	networkGitWaitDelay = time.Second
	t.Cleanup(func() { networkGitWaitDelay = restoreWait })

	if _, err := defaultRunNetworkGit("", remote, "", "clone", "--quiet", remote, t.TempDir()+"/dest"); err == nil {
		t.Fatal("cloning a wedged remote succeeded, want a timeout error")
	}

	pid := readChildPID(t, childPIDPath)
	// The group kill is delivered as the deadline fires, but reaping is not
	// instant; poll briefly rather than assert on a single sample.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if err := syscall.Kill(pid, 0); err != nil {
			return // ESRCH: the descendant is gone, which is the point.
		}
		if time.Now().After(deadline) {
			// Do not leave a live orphan behind for the rest of the suite.
			_ = syscall.Kill(pid, syscall.SIGKILL)
			t.Fatalf("git's child (pid %d) survived the deadline: the bound kills git but orphans its helpers, which keep writing into the repo cache after the lock is released", pid)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func readChildPID(t *testing.T, path string) int {
	t.Helper()
	// The shim writes the pid just after backgrounding, so it may not have
	// landed the instant the bound returns.
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err == nil {
			if pid, convErr := strconv.Atoi(strings.TrimSpace(string(raw))); convErr == nil && pid > 0 {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("git shim never recorded its child pid at %s: %v", path, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}
