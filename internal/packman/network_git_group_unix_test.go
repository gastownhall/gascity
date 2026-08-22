//go:build !windows

package packman

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
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
// Returning on time is therefore not the property under test. The assertion is
// that the writing stopped, measured directly: the shim's child appends to a
// heartbeat file for as long as it lives, so a file that stops growing is the
// descendant's death and a file that keeps growing is the leak. That is also
// why this asserts on bytes rather than on the pid — a killed orphan is a
// zombie until init reaps it, so pid liveness is ambiguous for exactly as long
// as it takes to be misleading.
func TestDefaultRunNetworkGitKillsDescendants(t *testing.T) {
	wedged := wedgedGit(t)

	restore := networkGitTimeout
	networkGitTimeout = 300 * time.Millisecond
	t.Cleanup(func() { networkGitTimeout = restore })
	restoreWait := networkGitWaitDelay
	networkGitWaitDelay = time.Second
	t.Cleanup(func() { networkGitWaitDelay = restoreWait })

	if _, err := defaultRunNetworkGit("", wedged.URL, "", "clone", "--quiet", wedged.URL, t.TempDir()+"/dest"); err == nil {
		t.Fatal("cloning a wedged remote succeeded, want a timeout error")
	}

	// Non-empty proves the child ran at all; without this the stability check
	// below would pass vacuously against a shim that never started one.
	size := processgrouptest.WaitForFileSize(t, wedged.HeartbeatPath)
	// The child writes about once a second, so 1.5s of no growth cannot be a
	// gap between its writes. AssertFileSizeStable fails with "kept growing
	// after timeout cleanup" when the orphan survived, which is the leak.
	processgrouptest.AssertFileSizeStable(t, wedged.HeartbeatPath, size, 1500*time.Millisecond)
}
