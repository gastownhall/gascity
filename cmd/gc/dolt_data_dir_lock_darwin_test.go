//go:build darwin

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"testing"
	"time"
)

func TestManagedDoltSIGKILLLockGateAllowsTargetOwnedLockOnDarwin(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	holdFlock(t, lockPath)

	if err := waitManagedDoltSIGKILLLockGate(os.Getpid(), dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond); err != nil {
		t.Fatalf("target-owned lock refused SIGKILL escalation on Darwin: %v", err)
	}
}

func TestParseManagedDoltLsofHolderPIDs(t *testing.T) {
	got, err := parseManagedDoltLsofHolderPIDs([]byte("p42\x00f5\x00n/tmp/LOCK\x00\np7\x00f9\x00n/tmp/LOCK\x00\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := []int{7, 42}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("holder pids = %v, want %v", got, want)
	}
}

func TestManagedDoltSIGKILLLockGateRefusesForeignLsofHolderOnDarwin(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	holdFlock(t, lockPath)
	old := managedDoltLsofLockHolders
	managedDoltLsofLockHolders = func(string) ([]byte, error) { return []byte("p4242\x00\n"), nil }
	t.Cleanup(func() { managedDoltLsofLockHolders = old })

	if err := waitManagedDoltSIGKILLLockGate(os.Getpid(), dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond); err == nil {
		t.Fatal("foreign lsof holder unexpectedly allowed SIGKILL escalation")
	}
}

func TestManagedDoltSIGKILLLockGateFailsClosedWhenLsofFailsOnDarwin(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	holdFlock(t, lockPath)
	old := managedDoltLsofLockHolders
	managedDoltLsofLockHolders = func(string) ([]byte, error) { return nil, errors.New("lsof unavailable") }
	t.Cleanup(func() { managedDoltLsofLockHolders = old })

	if err := waitManagedDoltSIGKILLLockGate(os.Getpid(), dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond); err == nil {
		t.Fatal("lsof failure unexpectedly allowed SIGKILL escalation")
	}
}

func TestManagedDoltSIGKILLLockGateRefusesTargetPlusForeignLsofHolderOnDarwin(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	holdFlock(t, lockPath)
	old := managedDoltLsofLockHolders
	out := []byte(fmt.Sprintf("p%d\x00\np4242\x00\n", os.Getpid()))
	managedDoltLsofLockHolders = func(string) ([]byte, error) { return out, nil }
	t.Cleanup(func() { managedDoltLsofLockHolders = old })

	if err := waitManagedDoltSIGKILLLockGate(os.Getpid(), dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond); err == nil {
		t.Fatal("target plus foreign lsof holder unexpectedly allowed SIGKILL escalation")
	}
}

func TestManagedDoltSIGKILLLockGateRefusesEmptyLsofOutputOnDarwin(t *testing.T) {
	dataDir, lockPath := makeDoltDataDirWithLock(t)
	holdFlock(t, lockPath)
	old := managedDoltLsofLockHolders
	managedDoltLsofLockHolders = func(string) ([]byte, error) { return nil, nil }
	t.Cleanup(func() { managedDoltLsofLockHolders = old })

	if err := waitManagedDoltSIGKILLLockGate(os.Getpid(), dataDir, func(int) bool { return true }, time.Second, 0, time.Millisecond); err == nil {
		t.Fatal("empty lsof output unexpectedly allowed SIGKILL escalation")
	}
}

func TestParseManagedDoltLsofHolderPIDsRejectsInvalidPID(t *testing.T) {
	for _, field := range []string{"p0", "pabc"} {
		if pids, err := parseManagedDoltLsofHolderPIDs([]byte(field + "\x00\n")); err == nil {
			t.Errorf("parse %q = %v, want error", field, pids)
		}
	}
}
