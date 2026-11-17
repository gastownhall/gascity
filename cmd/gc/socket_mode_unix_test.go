//go:build !windows

package main

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// assertOwnerOnlySocket fails the test unless path is a socket with 0600
// permissions.
func assertOwnerOnlySocket(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s mode = %v, want a socket", path, info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("%s permissions = %#o, want 0600", path, got)
	}
}

// TestControllerSocketIsOwnerOnly pins that the controller command socket is
// bound owner-only rather than at the umask default.
func TestControllerSocketIsOwnerOnly(t *testing.T) {
	cityPath := shortSocketTempDir(t, "gc-ctl-mode-")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	wake := newLegacyWake(make(chan struct{}, 1), make(chan struct{}, 1))
	lis, err := startControllerSocket(cityPath, controllerHostingStandalone, cancel, nil, &atomic.Bool{}, nil, nil, wake)
	if err != nil {
		t.Fatalf("startControllerSocket: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	assertOwnerOnlySocket(t, controllerSocketPath(cityPath))
}

// TestSupervisorSocketIsOwnerOnly pins that the supervisor control socket is
// bound owner-only rather than at the umask default.
func TestSupervisorSocketIsOwnerOnly(t *testing.T) {
	sockPath := filepath.Join(shortTempDir(t, "gc-sup-mode-"), "supervisor.sock")

	requestShutdown := func(supervisorShutdownMode, shutdownTrigger) bool { return true }
	lis, err := startSupervisorSocket(sockPath, requestShutdown, make(chan reconcileRequest, 1), newShutdownState())
	if err != nil {
		t.Fatalf("startSupervisorSocket: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	assertOwnerOnlySocket(t, sockPath)
}
