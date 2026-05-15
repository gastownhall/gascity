package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestStaleManagedDoltSocketPathsExcludesMysqlSock(t *testing.T) {
	tmpSock, err := os.CreateTemp("/tmp", "dolt-preflight-cleanup-*.sock")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(tmpSock.Name()) })
	if err := tmpSock.Close(); err != nil {
		t.Fatal(err)
	}

	paths := staleManagedDoltSocketPaths()
	for _, path := range paths {
		if path == "/tmp/mysql.sock" {
			t.Fatalf("staleManagedDoltSocketPaths unexpectedly includes mysql.sock: %+v", paths)
		}
	}
	found := false
	for _, path := range paths {
		if path == tmpSock.Name() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("staleManagedDoltSocketPaths() = %+v, want %q", paths, tmpSock.Name())
	}
	for _, path := range paths {
		if strings.HasPrefix(path, filepath.Join("/tmp", "mysql.sock")) {
			t.Fatalf("unexpected mysql-path prefix in %+v", paths)
		}
	}
}

func TestFileOpenedByAnyProcessWithoutLsofReturnsClosedOrUnknown(t *testing.T) {
	path := filepath.Join(t.TempDir(), "LOCK")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(t.TempDir(), "missing-bin"))
	open, err := fileOpenedByAnyProcess(path)
	if err != nil && !errors.Is(err, errManagedDoltOpenStateUnknown) {
		t.Fatalf("fileOpenedByAnyProcess() error = %v, want nil or errManagedDoltOpenStateUnknown", err)
	}
	if open {
		t.Fatal("fileOpenedByAnyProcess() = true, want false when lsof is unavailable")
	}
}

func TestFileOpenedByAnyProcessBoundsLsof(t *testing.T) {
	path := filepath.Join(t.TempDir(), "LOCK")
	if err := os.WriteFile(path, []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	withManagedDoltProcPaths(t, filepath.Join(t.TempDir(), "missing-proc"), filepath.Join(t.TempDir(), "missing-unix"))
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "lsof"), []byte("#!/bin/sh\ntouch \"$1.ran\"\nexec sleep 10\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(lsof): %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	start := time.Now()
	open, err := fileOpenedByAnyProcess(path)
	if err != nil && !errors.Is(err, errManagedDoltOpenStateUnknown) {
		t.Fatalf("fileOpenedByAnyProcess() error = %v, want nil or errManagedDoltOpenStateUnknown", err)
	}
	if open {
		t.Fatal("fileOpenedByAnyProcess() = true, want false when lsof times out")
	}
	if _, err := os.Stat(path + ".ran"); err != nil {
		t.Fatalf("fake lsof did not run: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("fileOpenedByAnyProcess() took %s, want bounded timeout", elapsed)
	}
}

func TestFileOpenedByAnyProcessFromProcHonorsCancelledContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "LOCK")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	open, checked := fileOpenedByAnyProcessFromProc(ctx, path)
	if open || checked {
		t.Fatalf("fileOpenedByAnyProcessFromProc(canceled) = (%v, %v), want false, false", open, checked)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("fileOpenedByAnyProcessFromProc(canceled) took %s, want immediate cancellation", elapsed)
	}
}

func TestUnixSocketInodesForPathHonorsCancelledContext(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "dolt.sock")
	unixTable := filepath.Join(t.TempDir(), "unix")
	if err := os.WriteFile(unixTable, []byte("Num RefCount Protocol Flags Type St Inode Path\n0000000000000000: 00000002 00000000 00010000 0001 01 12345 "+socketPath+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(unix table): %v", err)
	}
	withManagedDoltProcPaths(t, t.TempDir(), unixTable)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	inodes, checked := unixSocketInodesForPath(ctx, socketPath)
	if checked || len(inodes) != 0 {
		t.Fatalf("unixSocketInodesForPath(canceled) = (%v, %v), want nil/empty, false", inodes, checked)
	}
}

func TestRemoveStaleManagedDoltSocketsWithoutLsofKeepsSocket(t *testing.T) {
	socketPath := filepath.Join("/tmp", "dolt-preflight-cleanup-live-test.sock")
	_ = os.Remove(socketPath)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("net.Listen(unix): %v", err)
	}
	defer func() {
		_ = listener.Close()
		_ = os.Remove(socketPath)
	}()
	t.Setenv("PATH", filepath.Join(t.TempDir(), "missing-bin"))
	if err := removeStaleManagedDoltSockets(); err != nil {
		t.Fatalf("removeStaleManagedDoltSockets() error = %v", err)
	}
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("socket stat err = %v, want preserved when lsof unavailable", err)
	}
}

func withManagedDoltProcPaths(t *testing.T, procDir, unixSocketTable string) {
	t.Helper()
	oldProcDir := managedDoltProcDir
	oldUnixSocketTable := managedDoltUnixSocketTable
	managedDoltProcDir = procDir
	managedDoltUnixSocketTable = unixSocketTable
	t.Cleanup(func() {
		managedDoltProcDir = oldProcDir
		managedDoltUnixSocketTable = oldUnixSocketTable
	})
}
