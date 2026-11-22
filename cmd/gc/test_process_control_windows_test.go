//go:build windows

package main

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// doltLeakDetectionAvailable reports whether this platform can enumerate dolt
// sql-server processes well enough for the leak guard to make a finding.
//
// It cannot here. Attributing a PID to `dolt sql-server` needs its command
// line, which on Windows means reading the target's PEB across the process
// boundary; pidutil reports that as unsupported rather than guessing, and the
// /proc and ps fallbacks do not exist. Reading "/proc" would be worse than
// useless: it resolves to C:\proc, which any local user can create and fill
// with forged entries.
//
// What keeps this from hiding a real leak is that there is nothing to leak.
// managedDoltNativeIdentitySupported fails closed on Windows, so cmd/gc never
// starts a managed dolt server here in the first place. The guard says so out
// loud rather than reporting a sweep it did not perform.
func doltLeakDetectionAvailable() bool {
	return false
}

// sysProcAttrDetaches reports whether attr asks the OS to start the child
// outside this process's console control group, so a Ctrl+C aimed at the
// console is not inherited by it. CREATE_NEW_PROCESS_GROUP is the Windows
// spelling of what Setpgid does on Unix.
func sysProcAttrDetaches(attr *syscall.SysProcAttr) bool {
	return attr.CreationFlags&syscall.CREATE_NEW_PROCESS_GROUP != 0
}

// reraiseTestSignal ends an interrupted shard. Windows has no signal to
// re-raise, and signal.Notify has already suppressed the default Ctrl+C
// termination by the time this runs, so the process must exit here or the
// run would carry on past the interrupt it just swept for.
func reraiseTestSignal(_ os.Signal) {
	os.Exit(1)
}

// stopManagedDoltTestPID ends a managed dolt server the test city owns.
//
// There is no cooperative phase: the Unix path opens with SIGTERM, and
// Windows has no equivalent that can be addressed to an arbitrary PID, so
// this goes straight to termination rather than reporting a request that was
// never delivered. The confirmation is the same, and it is the part that
// matters -- the process must be observably gone before this returns.
func stopManagedDoltTestPID(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 || !managedStopPIDAlive(pid) {
		return
	}
	if err := killProcess(pid, syscall.SIGKILL); err != nil {
		if !managedStopPIDAlive(pid) {
			return
		}
		t.Fatalf("terminate dolt test pid %d: %v", pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for managedStopPIDAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if managedStopPIDAlive(pid) {
		t.Fatalf("dolt test pid %d still alive after termination", pid)
	}
}
