package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/supervisor"
)

func TestDoctorSkipsDoltChecksTreatsExecGcBeadsBdAsBdContract(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityDir))
	if doctorSkipsDoltChecks(cityDir) {
		t.Fatal("doctorSkipsDoltChecks() = true, want false for exec:gc-beads-bd")
	}
}

func TestCollectPackDirsEmpty(t *testing.T) {
	cfg := &config.City{}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 0 {
		t.Errorf("expected no dirs, got %v", dirs)
	}
}

func TestCollectPackDirsCityLevel(t *testing.T) {
	cfg := &config.City{
		PackDirs: []string{"/a", "/b"},
	}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d: %v", len(dirs), dirs)
	}
	if dirs[0] != "/a" || dirs[1] != "/b" {
		t.Errorf("dirs = %v, want [/a /b]", dirs)
	}
}

func TestCollectPackDirsRigLevel(t *testing.T) {
	cfg := &config.City{
		RigPackDirs: map[string][]string{
			"rig1": {"/x", "/y"},
			"rig2": {"/z"},
		},
	}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 3 {
		t.Fatalf("expected 3 dirs, got %d: %v", len(dirs), dirs)
	}
}

func TestCollectPackDirsDeduplicates(t *testing.T) {
	cfg := &config.City{
		PackDirs: []string{"/shared", "/a"},
		RigPackDirs: map[string][]string{
			"rig1": {"/shared", "/b"}, // /shared is a duplicate
		},
	}
	dirs := collectPackDirs(cfg)
	// /shared should appear only once.
	count := 0
	for _, d := range dirs {
		if d == "/shared" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("/shared appears %d times, want 1", count)
	}
	if len(dirs) != 3 {
		t.Fatalf("expected 3 unique dirs, got %d: %v", len(dirs), dirs)
	}
}

func TestCollectPackDirsMixed(t *testing.T) {
	cfg := &config.City{
		PackDirs: []string{"/city-topo"},
		RigPackDirs: map[string][]string{
			"rig1": {"/rig-topo"},
		},
	}
	dirs := collectPackDirs(cfg)
	if len(dirs) != 2 {
		t.Fatalf("expected 2 dirs, got %d: %v", len(dirs), dirs)
	}
}

func TestDoctorStoreFactoryUsesExplicitCityForRigOutsideCityTree(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	captureDir := t.TempDir()
	script := writeExecCaptureScript(t, captureDir)
	writeExecStoreCityConfig(t, cityDir, "metro-city", "ct", []config.Rig{{
		Name:   "frontend",
		Path:   rigDir,
		Prefix: "fe",
	}})
	t.Setenv("GC_BEADS", "exec:"+script)

	store, err := openStoreForCity(cityDir)(rigDir)
	if err != nil {
		t.Fatalf("openStoreForCity(rig): %v", err)
	}
	if _, err := store.Create(beads.Bead{Title: "rig"}); err != nil {
		t.Fatalf("Create: %v", err)
	}

	rigEnv := readExecCaptureEnv(t, filepath.Join(captureDir, "frontend.env"))
	if got := rigEnv["GC_CITY_PATH"]; got != cityDir {
		t.Fatalf("GC_CITY_PATH = %q, want %q", got, cityDir)
	}
	if got := rigEnv["GC_STORE_ROOT"]; got != rigDir {
		t.Fatalf("GC_STORE_ROOT = %q, want %q", got, rigDir)
	}
	if got := rigEnv["GC_BEADS_PREFIX"]; got != "fe" {
		t.Fatalf("GC_BEADS_PREFIX = %q, want fe", got)
	}
	if got := rigEnv["GC_RIG"]; got != "frontend" {
		t.Fatalf("GC_RIG = %q, want frontend", got)
	}
}

func TestDoctorStoreFactoryLegacyFileRigUsesSharedCityStoreWithoutCreatingRigState(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rigDir := filepath.Join(t.TempDir(), "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	legacyCityStore, err := openScopeLocalFileStore(cityDir)
	if err != nil {
		t.Fatalf("openScopeLocalFileStore(city): %v", err)
	}
	if _, err := legacyCityStore.Create(beads.Bead{Title: "legacy city bead", Type: "task"}); err != nil {
		t.Fatalf("legacy city Create: %v", err)
	}
	store, err := openStoreForCity(cityDir)(rigDir)
	if err != nil {
		t.Fatalf("openStoreForCity(rig): %v", err)
	}
	list, err := store.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("rig List: %v", err)
	}
	if len(list) != 1 || list[0].Title != "legacy city bead" {
		t.Fatalf("rig store should read legacy shared city data, got %#v", list)
	}
	if _, err := os.Stat(filepath.Join(rigDir, ".gc")); !os.IsNotExist(err) {
		t.Fatalf("doctor store factory should not create rig .gc state, stat err = %v", err)
	}
}

func TestBackfillRigIndexResolvesRelativeRigPaths(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[[rigs]]
name = "frontend"
path = "rigs/frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_HOME", t.TempDir())

	if err := backfillRigIndex(cityDir); err != nil {
		t.Fatalf("backfillRigIndex: %v", err)
	}

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	rigs, err := reg.ListRigs()
	if err != nil {
		t.Fatalf("ListRigs: %v", err)
	}
	if len(rigs) != 1 {
		t.Fatalf("len(rigs) = %d, want 1", len(rigs))
	}
	if rigs[0].Path != rigDir {
		t.Fatalf("rig path = %q, want %q", rigs[0].Path, rigDir)
	}
	if rigs[0].DefaultCity != cityDir {
		t.Fatalf("default city = %q, want %q", rigs[0].DefaultCity, cityDir)
	}
	envData, err := os.ReadFile(filepath.Join(rigDir, ".beads", ".env"))
	if err != nil {
		t.Fatalf("ReadFile(.env): %v", err)
	}
	if got := string(envData); !strings.Contains(got, "GT_ROOT="+cityDir+"\n") {
		t.Fatalf(".env = %q, want GT_ROOT", got)
	}
}

func TestDoctorSkipsSuspendedRigChecks(t *testing.T) {
	t.Parallel()
	activeDir := t.TempDir()
	suspendedDir := t.TempDir()

	rigs := []config.Rig{
		{Name: "active-rig", Path: activeDir},
		{Name: "suspended-rig", Path: suspendedDir, Suspended: true},
	}

	// Mirror the per-rig registration logic from doDoctor.
	d := &doctor.Doctor{}
	for _, rig := range rigs {
		if rig.Suspended {
			continue
		}
		d.Register(doctor.NewRigPathCheck(rig))
	}

	var buf bytes.Buffer
	ctx := &doctor.CheckContext{CityPath: t.TempDir()}
	d.Run(ctx, &buf, false)

	out := buf.String()
	if !strings.Contains(out, "active-rig") {
		t.Error("expected active-rig checks to be registered")
	}
	if strings.Contains(out, "suspended-rig") {
		t.Error("suspended-rig checks should not be registered")
	}
}

func TestDoltTopologyCheckReportsCanonicalCompatCityDrift(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[dolt]
host = "city.example.com"
port = 3307
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCanonicalScopeConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "hq",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}

	res := newDoltTopologyCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error", res.Status)
	}
	if !strings.Contains(res.Message, "deprecated city.toml [dolt] endpoint conflicts") {
		t.Fatalf("message = %q, want city drift", res.Message)
	}
	if res.FixHint == "" {
		t.Fatal("expected fix hint for topology drift")
	}
}

func TestDoltTopologyCheckReportsInheritedRigCompatDrift(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
dolt_host = "rig.example.com"
dolt_port = "3308"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCanonicalScopeConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "hq",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	resolveRigPaths(cityDir, cfg.Rigs)

	res := newDoltTopologyCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want error", res.Status)
	}
	if !strings.Contains(res.Message, `deprecated rig dolt_host/dolt_port conflict with inherited canonical endpoint for rig "frontend"`) {
		t.Fatalf("message = %q, want inherited rig drift", res.Message)
	}
}

func TestDoltTopologyCheckAllowsInheritedRigCompatMirrorForExternalCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[dolt]
host = "city.example.com"
port = 3307

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
dolt_host = "city.example.com"
dolt_port = "3307"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCanonicalScopeConfig(t, cityDir, contract.ConfigState{
		IssuePrefix:    "hq",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "city.example.com",
		DoltPort:       "3307",
	})
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "city.example.com",
		DoltPort:       "3307",
	})
	cfg, err := loadCityConfig(cityDir)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	resolveRigPaths(cityDir, cfg.Rigs)

	res := newDoltTopologyCheck(cityDir, cfg).Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want ok; message = %q", res.Status, res.Message)
	}
}
