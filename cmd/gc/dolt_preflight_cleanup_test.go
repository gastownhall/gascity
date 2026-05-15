package main

import (
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
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "lsof"), []byte("#!/bin/sh\nexec sleep 10\n"), 0o755); err != nil {
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
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("fileOpenedByAnyProcess() took %s, want bounded timeout", elapsed)
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
