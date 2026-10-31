//go:build unix

package main

import (
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"
)

// doltLeakDetectionAvailable reports whether this platform can enumerate dolt
// sql-server processes well enough for the leak guard to make a finding.
func doltLeakDetectionAvailable() bool {
	return true
}

// sysProcAttrDetaches reports whether attr asks the OS to start the child
// outside this process's signal-delivery group, so a Ctrl+C aimed at the
// terminal is not inherited by it.
func sysProcAttrDetaches(attr *syscall.SysProcAttr) bool {
	return attr.Setpgid
}

// reraiseTestSignal restores the default disposition for s and delivers it to
// this process, so a shard interrupted or timed out mid-run still exits with
// the status the signal names once the leak guard has swept.
func reraiseTestSignal(s os.Signal) {
	ss, ok := s.(syscall.Signal)
	if !ok {
		return
	}
	signal.Reset(ss)
	_ = syscall.Kill(os.Getpid(), ss)
}

// stopManagedDoltTestPID ends a managed dolt server the test city owns: ask it
// to stop, wait, then force it and confirm. A signal that reports ESRCH means
// the process is already gone, which is the outcome being sought.
func stopManagedDoltTestPID(t *testing.T, pid int) {
	t.Helper()
	if pid <= 0 || !managedStopPIDAlive(pid) {
		return
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && err != syscall.ESRCH {
		t.Fatalf("signal dolt test pid %d with SIGTERM: %v", pid, err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for managedStopPIDAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !managedStopPIDAlive(pid) {
		return
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
		t.Fatalf("signal dolt test pid %d with SIGKILL: %v", pid, err)
	}
	deadline = time.Now().Add(time.Second)
	for managedStopPIDAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if managedStopPIDAlive(pid) {
		t.Fatalf("dolt test pid %d still alive after SIGKILL", pid)
	}
}
