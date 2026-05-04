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

func TestRemoveStaleManagedDoltLocksWithoutLsofUsesAvailableState(t *testing.T) {
	skipSlowCmdGCTest(t, "runs managed-dolt preflight cleanup against filesystem locks; run make test-cmd-gc-process for full coverage")
	dataDir := t.TempDir()
	lockFile := filepath.Join(dataDir, "hq", ".dolt", "noms", "LOCK")
	if err := os.MkdirAll(filepath.Dir(lockFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockFile, []byte("live\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Join(t.TempDir(), "missing-bin"))
	_, procChecked := fileOpenedByAnyProcessFromProc(lockFile)
	if err := removeStaleManagedDoltLocks(dataDir); err != nil {
		t.Fatalf("removeStaleManagedDoltLocks() error = %v", err)
	}
	if _, err := os.Stat(lockFile); procChecked {
		if !os.IsNotExist(err) {
			t.Fatalf("LOCK stat err = %v, want stale lock removed when proc state is available", err)
		}
	} else if err != nil {
		t.Fatalf("LOCK stat err = %v, want preserved when open-file state is unknown", err)
	}
}

func TestQuarantinePhantomManagedDoltDatabasesQuarantinesRetiredReplacementDB(t *testing.T) {
	dataDir := t.TempDir()
	activeManifest := filepath.Join(dataDir, "ga", ".dolt", "noms", "manifest")
	if err := os.MkdirAll(filepath.Dir(activeManifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(activeManifest, []byte("active\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	retiredManifest := filepath.Join(dataDir, "ga.replaced-20260428T100722Z", ".dolt", "noms", "manifest")
	if err := os.MkdirAll(filepath.Dir(retiredManifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retiredManifest, []byte("retired\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	replacementLikeManifest := filepath.Join(dataDir, "ga.replaced-pending", ".dolt", "noms", "manifest")
	if err := os.MkdirAll(filepath.Dir(replacementLikeManifest), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacementLikeManifest, []byte("active\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 4, 29, 16, 20, 0, 0, time.UTC)
	if err := quarantinePhantomManagedDoltDatabases(dataDir, now); err != nil {
		t.Fatalf("quarantinePhantomManagedDoltDatabases: %v", err)
	}

	if _, err := os.Stat(activeManifest); err != nil {
		t.Fatalf("active manifest stat: %v", err)
	}
	if _, err := os.Stat(replacementLikeManifest); err != nil {
		t.Fatalf("replacement-like active manifest stat: %v", err)
	}
	if _, err := os.Stat(retiredManifest); !os.IsNotExist(err) {
		t.Fatalf("retired manifest stat err = %v, want moved out of data dir", err)
	}
	quarantined := filepath.Join(dataDir, ".quarantine", "20260429T162000-ga.replaced-20260428T100722Z", ".dolt", "noms", "manifest")
	if _, err := os.Stat(quarantined); err != nil {
		t.Fatalf("quarantined manifest stat: %v", err)
	}
}

func TestRetiredManagedDoltDatabaseNameRequiresTimestampSuffix(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{name: "ga.replaced-20260428T100722Z", want: true},
		{name: "ga.replaced-20260428T100722Z.bak", want: false},
		{name: "ga.replaced-20260428T100722", want: false},
		{name: "ga.replaced-pending", want: false},
		{name: "replaced-20260428T100722Z", want: false},
		{name: ".replaced-20260428T100722Z", want: false},
		{name: "ga", want: false},
	}
	for _, tt := range tests {
		if got := retiredManagedDoltDatabaseName(tt.name); got != tt.want {
			t.Fatalf("retiredManagedDoltDatabaseName(%q) = %v, want %v", tt.name, got, tt.want)
		}
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
