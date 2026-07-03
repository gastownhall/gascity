package main

import (
	"bytes"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/supervisor"
)

// stubSupervisorAlive installs test hooks that make controllerStatusForCity
// report a running supervisor-managed city. Caller must register the city
// in the supervisor registry first.
func stubSupervisorAlive(t *testing.T) {
	t.Helper()
	oldAlive := supervisorAliveHook
	oldRunning := supervisorCityRunningHook
	supervisorAliveHook = func() int { return 4321 }
	supervisorCityRunningHook = func(string) (bool, string, bool) { return true, "", true }
	t.Cleanup(func() {
		supervisorAliveHook = oldAlive
		supervisorCityRunningHook = oldRunning
	})
}

func TestCityStatusSnapshotOmitsStoreHealthWhenControllerStopped(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "city"}}
	snapshot := collectCityStatusSnapshot(runtime.NewFake(), cfg, "/tmp/city", nil, io.Discard)
	if snapshot.Summary.StoreHealth != nil {
		t.Fatalf("StoreHealth = %+v, want nil when controller stopped", snapshot.Summary.StoreHealth)
	}
}

func registerCityForSnapshot(t *testing.T) string {
	t.Helper()
	t.Setenv("GC_HOME", filepath.Join(t.TempDir(), "gc-home"))
	cityPath := filepath.Join(t.TempDir(), "bright-lights")
	if err := supervisor.NewRegistry(supervisor.RegistryPath()).Register(cityPath, "bright-lights"); err != nil {
		t.Fatalf("register city: %v", err)
	}
	return cityPath
}

func TestCityStatusSnapshotIncludesStoreHealthWhenControllerRunning(t *testing.T) {
	cityPath := registerCityForSnapshot(t)
	stubSupervisorAlive(t)

	store := beads.NewMemStore()
	for i := 0; i < 3; i++ {
		if _, err := store.Create(beads.Bead{Title: "b"}); err != nil {
			t.Fatalf("Create: %v", err)
		}
	}

	cfg := &config.City{Workspace: config.Workspace{Name: "bright-lights"}}
	snapshot := collectCityStatusSnapshot(runtime.NewFake(), cfg, cityPath, store, io.Discard)

	h := snapshot.Summary.StoreHealth
	if h == nil {
		t.Fatal("StoreHealth = nil, want populated")
	}
	if h.LiveRows != 3 {
		t.Errorf("LiveRows = %d, want 3", h.LiveRows)
	}
	if h.ThresholdMB != 1.0 {
		t.Errorf("ThresholdMB = %v, want 1.0", h.ThresholdMB)
	}
	if h.Warning {
		t.Errorf("Warning = true, want false for empty store dir")
	}
	if !strings.HasSuffix(h.Path, filepath.Join(".beads", "dolt")) {
		t.Errorf("Path = %q, want .beads/dolt suffix", h.Path)
	}
}

func TestCityStatusJSONIncludesStoreHealthWhenSupervisorAlive(t *testing.T) {
	cityPath := registerCityForSnapshot(t)
	stubSupervisorAlive(t)

	store := beads.NewMemStore()
	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: store}, nil
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })

	cfg := &config.City{Workspace: config.Workspace{Name: "bright-lights"}}
	var stdout, stderr bytes.Buffer
	code := doCityStatusJSON(runtime.NewFake(), cfg, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr: %s", code, stderr.String())
	}

	var got StatusJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("Unmarshal: %v; stdout: %s", err, stdout.String())
	}
	if got.Summary.StoreHealth == nil {
		t.Fatalf("Summary.StoreHealth = nil, want populated; stdout: %s", stdout.String())
	}
	if got.Summary.StoreHealth.ThresholdMB != 1.0 {
		t.Errorf("ThresholdMB = %v, want 1.0", got.Summary.StoreHealth.ThresholdMB)
	}
}

func TestCityStatusJSONOmitsStoreHealthWhenSupervisorDown(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "bright-lights"}}
	var stdout, stderr bytes.Buffer
	code := doCityStatusJSON(runtime.NewFake(), cfg, "/tmp/no-city", &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr: %s", code, stderr.String())
	}
	if strings.Contains(stdout.String(), "store_health") {
		t.Fatalf("stdout contains store_health when supervisor down:\n%s", stdout.String())
	}
}

func TestCityStatusTextIncludesStoreHealthBlockWhenSupervisorAlive(t *testing.T) {
	cityPath := registerCityForSnapshot(t)
	stubSupervisorAlive(t)

	store := beads.NewMemStore()
	oldOpen := openCityStoreAtForStatus
	openCityStoreAtForStatus = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: store}, nil
	}
	t.Cleanup(func() { openCityStoreAtForStatus = oldOpen })

	cfg := &config.City{Workspace: config.Workspace{Name: "bright-lights"}}
	sp := runtime.NewFake()
	dops := newFakeDrainOps()
	var stdout, stderr bytes.Buffer
	code := doCityStatus(sp, dops, cfg, cityPath, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("code = %d, stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Store health:") {
		t.Fatalf("stdout missing Store health block:\n%s", stdout.String())
	}
}

func TestCityStatusSnapshotWarnsOnHighRatio(t *testing.T) {
	cityPath := registerCityForSnapshot(t)
	stubSupervisorAlive(t)

	// 221 rows is enough to exceed the 1 MB/row threshold against a
	// simulated 11.2 GB on disk. Since WalkSize reads the real FS and
	// an empty tempdir won't hit the threshold, we exercise the math
	// via storeHealthFromInputs directly instead.
	rows := 221
	const diskBytes = int64(11_200_000_000)
	h := storeHealthFromInputs(cityPath, diskBytes, rows)
	if !h.Warning {
		t.Fatalf("Warning = false, want true for %d bytes / %d rows", diskBytes, rows)
	}
	// Sanity: below-threshold case.
	h = storeHealthFromInputs(cityPath, 50_000_000, rows)
	if h.Warning {
		t.Fatalf("Warning = true, want false for 50 MB / %d rows", rows)
	}
}
