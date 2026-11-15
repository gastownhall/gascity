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

// reapSignalAfterFirst makes probeSignal answer ESRCH for pid after the first
// probe, modeling a parent that reaps the PID once Alive has already observed it.
func reapSignalAfterFirst(t *testing.T, pid int) {
	t.Helper()
	orig := probeSignal
	var calls atomic.Int32
	probeSignal = func(p int, sig syscall.Signal) error {
		if p == pid && int(calls.Add(1)) > 1 {
			return syscall.ESRCH
		}
		return orig(p, sig)
	}
	t.Cleanup(func() { probeSignal = orig })
}

// A PID reaped between Alive's kill(0) probe and its /proc read used to fall
// through to the ps zombie check, which finds nothing for a vanished PID and so
// reported it alive. The follow-up probe sees the PID gone.
func TestAliveReportsDeadWhenProcessVanishesAfterSignalProbe(t *testing.T) {
	failingPS(t)
	self := os.Getpid()
	vanishAfter(t, self, 0)
	reapSignalAfterFirst(t, self)
	if Alive(self) {
		t.Fatal("Alive = true for a PID whose /proc entry vanished after kill(0); a reaped process must read as dead")
	}
}

// A PID reaped after /proc/<pid>/stat was opened but before it was read does
// not fail with ENOENT: the open already succeeded, so the kernel answers the
// read with ESRCH. That error is not fs.ErrNotExist, so it must be settled by
// the follow-up probe rather than by the missing-entry check.
func TestAliveReportsDeadWhenStatReadRacesReap(t *testing.T) {
	failingPS(t)
	self := os.Getpid()
	target := filepath.Join("/proc", strconv.Itoa(self), "stat")
	orig := readProcStat
	readProcStat = func(path string) ([]byte, error) {
		if path == target {
			return nil, &fs.PathError{Op: "read", Path: path, Err: syscall.ESRCH}
		}
		return orig(path)
	}
	t.Cleanup(func() { readProcStat = orig })
	reapSignalAfterFirst(t, self)
	if Alive(self) {
		t.Fatal("Alive = true for a PID reaped between opening and reading its /proc stat; a reaped process must read as dead")
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
	reapSignalAfterFirst(t, self)
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
