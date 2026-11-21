//go:build windows

package main

import (
	"errors"
	"testing"
	"time"
)

// TestStartManagedDoltProcessRefusedOnWindows drives the real gate rather than
// a stub, so it fails if the Windows implementation ever starts reporting
// support it does not have.
//
// This is the case the gate was added for: Windows cannot prove which process
// owns a PID, a data directory or a port, so it cannot stop a managed Dolt
// server or even report on one. Starting one anyway produced a server that
// nothing could subsequently reach.
func TestStartManagedDoltProcessRefusedOnWindows(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_PACK_STATE_DIR", cityPath)

	_, err := startManagedDoltProcessWithOptions(cityPath, "127.0.0.1", "17796", "root", "warning", -1, time.Second, false)
	if !errors.Is(err, errManagedDoltIdentityUnavailable) {
		t.Fatalf("starting managed Dolt on Windows = %v, want errManagedDoltIdentityUnavailable", err)
	}

	// The other two verbs already refused; the point of the change is that all
	// three now agree rather than start standing alone.
	if _, err := stopManagedDoltProcessWithOptions(cityPath, "17796", false); !errors.Is(err, errManagedDoltIdentityUnavailable) {
		t.Fatalf("stopping managed Dolt on Windows = %v, want errManagedDoltIdentityUnavailable", err)
	}
	if _, err := inspectManagedDoltProcess(cityPath, "17796"); !errors.Is(err, errManagedDoltIdentityUnavailable) {
		t.Fatalf("inspecting managed Dolt on Windows = %v, want errManagedDoltIdentityUnavailable", err)
	}
}
