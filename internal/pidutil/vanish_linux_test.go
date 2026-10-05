//go:build linux

package pidutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
)

// vanishAfter swaps readProcStat for one that answers the real
// /proc/<pid>/stat for the first n reads of pid and fs.ErrNotExist afterwards,
// modeling pid being reaped mid-probe: kill(0) has already succeeded, then its
// /proc entry disappears.
func vanishAfter(t *testing.T, pid, n int) {
	t.Helper()
	target := filepath.Join("/proc", strconv.Itoa(pid), "stat")
	var reads atomic.Int32
	orig := readProcStat
	readProcStat = func(path string) ([]byte, error) {
		if path == target && int(reads.Add(1)) > n {
			return nil, fmt.Errorf("open %s: %w", path, fs.ErrNotExist)
		}
		return orig(path)
	}
	t.Cleanup(func() { readProcStat = orig })
}

// failingPS puts a ps on PATH that finds nothing, as real ps does for a PID
// that no longer exists.
func failingPS(t *testing.T) {
	t.Helper()
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "ps"), []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(ps): %v", err)
	}
	t.Setenv("PATH", strings.Join([]string{binDir, os.Getenv("PATH")}, string(os.PathListSeparator)))
}

// A PID reaped between Alive's kill(0) probe and its /proc read used to fall
// through to the ps zombie check, which finds nothing for a vanished PID and so
// reported it alive. With /proc mounted, a missing entry means the process is
// gone.
func TestAliveReportsDeadWhenProcessVanishesAfterSignalProbe(t *testing.T) {
	failingPS(t)
	self := os.Getpid()
	vanishAfter(t, self, 0)
	if Alive(self) {
		t.Fatal("Alive = true for a PID whose /proc entry vanished after kill(0); a reaped process must read as dead")
	}
}

// A PID reaped between AliveWithStartTime's liveness check and its start-time
// read made the identity unreadable, and the conservative branch reported the
// vanished process alive. Re-checking liveness keeps the conservative answer
// for a live process while letting a vanished one read as dead.
func TestAliveWithStartTimeReportsDeadWhenProcessVanishesDuringIdentityRead(t *testing.T) {
	self := os.Getpid()
	token, err := StartTime(self)
	if err != nil {
		t.Fatalf("StartTime(self): %v", err)
	}
	failingPS(t)
	vanishAfter(t, self, 1)
	if AliveWithStartTime(self, token) {
		t.Fatal("AliveWithStartTime = true for a PID that vanished before its start time could be read")
	}
}

// permissionDenied makes Alive's kill(0) probe answer EPERM for pid, as it
// does for a live process owned by another user.
func permissionDenied(t *testing.T, pid int) {
	t.Helper()
	orig := probeSignal
	probeSignal = func(p int, sig syscall.Signal) error {
		if p == pid {
			return syscall.EPERM
		}
		return orig(p, sig)
	}
	t.Cleanup(func() { probeSignal = orig })
}

// EPERM from kill(0) proves the process exists; only its owner differs. With
// /proc mounted hidepid=1 or 2, another user's /proc/<pid> entry is hidden, so
// a missing entry after EPERM is not a reaped process and must not read as
// dead. Only a missing entry after a successful kill(0) means it vanished.
func TestAliveKeepsPermissionDeniedProcessWithHiddenProcEntry(t *testing.T) {
	failingPS(t)
	self := os.Getpid()
	permissionDenied(t, self)
	vanishAfter(t, self, 0)
	if !Alive(self) {
		t.Fatal("Alive = false for a PID that kill(0) answered with EPERM; a hidden /proc entry (hidepid) does not mean the process is gone")
	}
}
