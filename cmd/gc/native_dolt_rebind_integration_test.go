//go:build integration

package main

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestManagedBdRigProviderStoreRecoversAfterHardKillPortRebind proves that the
// cmd/gc provider-store wiring re-resolves a managed Dolt endpoint after the
// original process is killed and its port is made unavailable.
func TestManagedBdRigProviderStoreRecoversAfterHardKillPortRebind(t *testing.T) {
	cityPath, rigPath := setupManagedBdWaitTestCity(t)
	bdPath := waitTestRealBDPath(t)
	rawDir := filepath.Join(rigPath, "provider-rebind")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(rawDir): %v", err)
	}

	rawID := parseCreatedBeadID(t, runRawBDFromDir(t, bdPath, rawDir, "create", "--json", "provider rebind bead", "-t", "task"))
	providerResult, err := openStoreResultAtForCity(rigPath, cityPath)
	if err != nil {
		t.Fatalf("openStoreResultAtForCity(rig): %v", err)
	}
	if got, want := providerResult.Diagnostic.Store, beads.BeadsStoreNameNativeDoltStore; got != want {
		t.Fatalf("provider store = %q, want %q; diagnostic: %+v", got, want, providerResult.Diagnostic)
	}
	providerStore := providerResult.Store
	if got, err := providerStore.Get(rawID); err != nil {
		t.Fatalf("providerStore.Get(rawID) before rebind: %v", err)
	} else if got.ID != rawID {
		t.Fatalf("providerStore.Get(rawID).ID = %q, want %q", got.ID, rawID)
	}

	before, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile(before): %v", err)
	}
	if before.PID <= 0 || before.Port <= 0 {
		t.Fatalf("unexpected managed runtime before fault: %+v", before)
	}
	if err := syscall.Kill(before.PID, syscall.SIGKILL); err != nil {
		t.Fatalf("Kill(%d): %v", before.PID, err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for pidAlive(before.PID) && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
	}

	occupyManagedDoltPort(t, before.Port)

	t.Setenv("GC_DOLT_PORT", "9999")
	got, err := providerStore.Get(rawID)
	if err != nil {
		t.Fatalf("providerStore.Get(rawID) after rebind: %v", err)
	}
	if got.ID != rawID {
		t.Fatalf("providerStore.Get(rawID) after rebind ID = %q, want %q", got.ID, rawID)
	}

	after, err := readDoltRuntimeStateFile(managedDoltStatePath(cityPath))
	if err != nil {
		t.Fatalf("readDoltRuntimeStateFile(after): %v", err)
	}
	if !after.Running || after.Port <= 0 || after.Port == before.Port || after.PID <= 0 || !pidAlive(after.PID) {
		t.Fatalf("managed Dolt did not rebind for provider store; before=%+v after=%+v", before, after)
	}
}
