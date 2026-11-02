package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/cityinit"
	"github.com/gastownhall/gascity/internal/fsys"
)

// leaveStaleStore puts a previous city's binding at dir/.gc/store, the shape
// a reused directory has after city.toml and the scaffold were removed.
func leaveStaleStore(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc", "store"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".gc", "store", infraMigratedMarkerName), []byte("infrastructure classes migrated to binding infra (x) 2026-08-08T00:09:31Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func setInitAdoptStore(t *testing.T, v bool) {
	t.Helper()
	prev := initAdoptStoreFlag
	t.Cleanup(func() { initAdoptStoreFlag = prev })
	initAdoptStoreFlag = v
}

// TestInitRefusesAnExistingStore is B15's other door: gc init into a
// directory that still holds a previous city's .gc/store would hand the new
// city that binding (S5 §7.2). Every init mode refuses it unless the operator
// says --adopt-store.
func TestInitRefusesAnExistingStore(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)

	t.Run("from", func(t *testing.T) {
		src := newRemoteStageSource(t)
		dst := filepath.Join(t.TempDir(), "reused")
		leaveStaleStore(t, dst)
		var stdout, stderr bytes.Buffer
		if code := doInitFromDirWithOptionsInternal(src, dst, "", &stdout, &stderr, true, true, hostedDoltInitOptions{}); code == 0 {
			t.Fatal("gc init --from adopted an existing .gc/store")
		}
		if !strings.Contains(stderr.String(), "--adopt-store") {
			t.Errorf("refusal %q does not name --adopt-store", stderr.String())
		}
		if _, err := os.Stat(filepath.Join(dst, "city.toml")); !os.IsNotExist(err) {
			t.Errorf("the refused init still copied the city: %v", err)
		}
	})
	t.Run("default", func(t *testing.T) {
		dst := filepath.Join(t.TempDir(), "reused")
		leaveStaleStore(t, dst)
		var stdout, stderr bytes.Buffer
		if code := doInit(fsys.OSFS{}, dst, defaultWizardConfig(), "", &stdout, &stderr, false); code == 0 {
			t.Fatal("gc init adopted an existing .gc/store")
		}
		if !strings.Contains(stderr.String(), "--adopt-store") {
			t.Errorf("refusal %q does not name --adopt-store", stderr.String())
		}
	})
	t.Run("adopt", func(t *testing.T) {
		setInitAdoptStore(t, true)
		src := newRemoteStageSource(t)
		dst := filepath.Join(t.TempDir(), "reused")
		leaveStaleStore(t, dst)
		var stdout, stderr bytes.Buffer
		if code := doInitFromDirWithOptionsInternal(src, dst, "", &stdout, &stderr, true, true, hostedDoltInitOptions{}); code != 0 {
			t.Fatalf("gc init --from --adopt-store = %d; stderr=%s", code, stderr.String())
		}
	})
}

// TestAPIInitSurfacesTheRefusalReason: the API's init reports why the
// scaffold refused (an existing .gc/store), not a bare exit code.
func TestAPIInitSurfacesTheRefusalReason(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	configureIsolatedRuntimeEnv(t)
	dst := filepath.Join(t.TempDir(), "reused")
	leaveStaleStore(t, dst)
	err := cityInitDoInit(context.Background(), cityinit.InitRequest{Dir: dst, ConfigName: "minimal", Provider: "claude"})
	if err == nil || !strings.Contains(err.Error(), ".gc/store") || !strings.Contains(err.Error(), "--adopt-store") {
		t.Fatalf("API init over an existing store = %v, want the refusal reason", err)
	}
}
