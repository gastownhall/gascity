//go:build !windows

package packman

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/processgroup/processgrouptest"
	"github.com/gastownhall/gascity/internal/testutil"
)

// TestDefaultRunNetworkGitKillsDescendants pins that context cancellation
// reaches git's children, not just git. TestDefaultRunNetworkGitIsBounded owns
// the real deadline; this test waits for an actual descendant before canceling.
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

	restoreWait := networkGitWaitDelay
	networkGitWaitDelay = time.Second
	t.Cleanup(func() { networkGitWaitDelay = restoreWait })

	ctx, cancel := context.WithCancel(context.Background())
	finished := make(chan struct{})
	var runErr error
	dest := t.TempDir() + "/dest"
	go func() {
		defer close(finished)
		_, runErr = runNetworkGitWithContextFactory(func() (context.Context, context.CancelFunc) {
			return ctx, cancel
		}, "", wedged.URL, "", "clone", "--quiet", wedged.URL, dest)
	}()
	// Register after global restoration callbacks so even a readiness failure
	// cancels and drains this invocation before its knobs can be restored.
	t.Cleanup(func() {
		cancel()
		processgrouptest.KillFromPIDFile(t, wedged.PIDPath)
		select {
		case <-finished:
		case <-time.After(testutil.ExecRaceTimeout):
			t.Error("network git did not stop during fixture cleanup")
		}
	})

	// The parent must publish the child PID before cancellation, including on
	// the negative-control path where only the parent is killed. The child,
	// not the parent, must then prove it is writing before we stop the group.
	processgrouptest.WaitForFileSize(t, wedged.PIDPath)
	processgrouptest.WaitForFileSize(t, wedged.HeartbeatPath)
	cancel()
	select {
	case <-finished:
	case <-time.After(testutil.ExecRaceTimeout):
		t.Fatal("network git did not return after cancellation")
	}
	if !errors.Is(runErr, errNetworkGitTimeout) {
		t.Fatalf("cloning a wedged remote returned %v, want a cancellation timeout", runErr)
	}

	size := processgrouptest.WaitForFileSize(t, wedged.HeartbeatPath)
	// The window has to be a comfortable multiple of the shim's 50ms write
	// cadence, because the failure mode of getting it wrong is silent: a live
	// orphan that happens to be descheduled for one window reads as a dead one
	// and the test goes green having stopped guarding. 300ms is 6x, and is what
	// the other users of this helper pair with the same cadence.
	processgrouptest.AssertFileSizeStable(t, wedged.HeartbeatPath, size, 300*time.Millisecond)
}
