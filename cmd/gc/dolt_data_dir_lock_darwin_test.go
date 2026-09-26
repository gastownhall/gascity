//go:build darwin

package main

import (
	"errors"
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
