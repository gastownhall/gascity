package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

func prependDoctorJSONStubBinaries(t *testing.T, names ...string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDoctorJSONSuccessIsParseableJSONOnly(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeBuiltinImportsFixture(t, cityDir, "core")
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "site.toml"), []byte("workspace_name = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	prependDoctorJSONStubBinaries(t, "tmux", "git", "jq", "pgrep", "lsof")

	var stdout, stderr bytes.Buffer
	code := run([]string{"--city", cityDir, "doctor", "--json"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc doctor --json = %d; stderr=%s stdout=%s", code, stderr.String(), stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want empty", stderr.String())
	}
	if strings.Contains(stdout.String(), "✓") || strings.Contains(stdout.String(), "warnings") {
		t.Fatalf("stdout contains human doctor output: %q", stdout.String())
	}

	var payload struct {
		Passed  int `json:"passed"`
		Failed  int `json:"failed"`
		Results []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
		} `json:"results"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if payload.Passed == 0 || payload.Failed != 0 || len(payload.Results) == 0 {
		t.Fatalf("payload summary/results = %+v", payload)
	}
}

func TestDoctorSkipsDoltChecksTreatsExecGcBeadsBdAsBdContract(t *testing.T) {
	cityDir := t.TempDir()
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityDir))
	if doctorSkipsDoltChecks(cityDir) {
		t.Fatal("doctorSkipsDoltChecks() = true, want false for exec:gc-beads-bd")
	}
}

func TestDoctorSkipsDoltChecksDetectsBdRigUnderFileBackedCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if doctorSkipsDoltChecks(cityDir) {
		t.Fatal("doctorSkipsDoltChecks() = true, want false for bd-backed rig")
	}
}

func TestManagedDoltOpsCheckSkipKeepsCityManagedWorkspaceEnabled(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{Rigs: nil}
	if managedDoltOpsCheckSkip(cityDir, cfg, nil) {
		t.Fatal("managedDoltOpsCheckSkip() = true, want false for city-managed workspace without rigs")
	}
}

func TestManagedDoltOpsCheckSkipOnConfigError(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if !managedDoltOpsCheckSkip(cityDir, nil, os.ErrInvalid) {
		t.Fatal("managedDoltOpsCheckSkip() = false, want true when city config failed to load")
	}
}

func TestManagedDoltOpsCheckUsesDoctorApplicabilityOnConfigError(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if managedDoltOpsCheckSkip(cityDir, nil, os.ErrInvalid) {
		t.Fatal("managedDoltOpsCheckSkip() = true, want false when broken city still has managed bd metadata")
	}
	if !doctor.ManagedLocalDoltChecksApplicableForConfig(cityDir, nil, os.ErrInvalid) {
		t.Fatal("doctor applicability = false, want true for same broken managed city")
	}
}

func TestManagedDoltOpsCheckDiscoversRigMetadataOnConfigError(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if managedDoltOpsCheckSkip(cityDir, nil, os.ErrInvalid) {
		t.Fatal("managedDoltOpsCheckSkip() = true, want false when broken city still has managed rig metadata")
	}
}

func TestDoDoctorRunsCityDoltCheckForInheritedBdRigUnderFileBackedCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "config.yaml"), contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "fe",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", cityDir)

	oldCityCheck := newDoctorDoltServerCheck
	oldRigCheck := newDoctorRigDoltServerCheck
	var citySkip, rigSkip *bool
	newDoctorDoltServerCheck = func(cityPath string, skip bool) *doctor.DoltServerCheck {
		citySkip = &skip
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, skip bool) *doctor.RigDoltServerCheck {
		rigSkip = &skip
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}
	t.Cleanup(func() {
		newDoctorDoltServerCheck = oldCityCheck
		newDoctorRigDoltServerCheck = oldRigCheck
	})

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, false, false, false, 0, &stdout, &stderr)

	if citySkip == nil || *citySkip {
		t.Fatalf("city dolt check skip = %v, want false when a bd-backed rig inherits the city endpoint", citySkip)
	}
	if rigSkip == nil || *rigSkip {
		t.Fatalf("rig dolt check skip = %v, want false for bd-backed rig", rigSkip)
	}
}

func TestBuildDoctorChecksRegistersDoltChecksOnlyForActiveManagedRigs(t *testing.T) {
	clearInheritedBeadsEnv(t)

	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "managed"
path = "managed"
prefix = "ma"

[[rigs]]
name = "filebacked"
path = "filebacked"
prefix = "fi"

[[rigs]]
name = "sleeping"
path = "sleeping"
prefix = "sl"
suspended = true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"managed", "filebacked", "sleeping"} {
		if err := os.MkdirAll(filepath.Join(cityDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"managed", "sleeping"} {
		rigDir := filepath.Join(cityDir, name)
		if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"), contract.MetadataState{
			Database:     "dolt",
			Backend:      "dolt",
			DoltMode:     "server",
			DoltDatabase: name,
		}); err != nil {
			t.Fatal(err)
		}
	}
	doltDataDir := filepath.Join(cityDir, "runtime-dolt")
	t.Setenv("GC_DOLT_DATA_DIR", doltDataDir)

	oldBackupCheck := newDoctorDoltBackupCheck
	oldLocalOnlyCheck := newDoctorDoltLocalOnlyCheck
	registeredBackup := map[string]string{}
	registeredLocalOnly := map[string]string{}
	newDoctorDoltBackupCheck = func(cityPath string, rig config.Rig, dataDir string) *doctor.DoltBackupCheck {
		registeredBackup[rig.Name] = dataDir
		return doctor.NewDoltBackupCheck(cityPath, rig, dataDir)
	}
	newDoctorDoltLocalOnlyCheck = func(cityPath string, rig config.Rig, dataDir string) *doctor.DoltLocalOnlyRemoteCheck {
		registeredLocalOnly[rig.Name] = dataDir
		return doctor.NewDoltLocalOnlyRemoteCheck(cityPath, rig, dataDir)
	}
	t.Cleanup(func() {
		newDoctorDoltBackupCheck = oldBackupCheck
		newDoctorDoltLocalOnlyCheck = oldLocalOnlyCheck
	})

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "managed", Path: "managed", Prefix: "ma"},
			{Name: "filebacked", Path: "filebacked", Prefix: "fi"},
			{Name: "sleeping", Path: "sleeping", Prefix: "sl", Suspended: true},
		},
	}
	buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    true,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})

	if len(registeredBackup) != 1 {
		t.Fatalf("registered dolt-backup checks = %#v, want only active managed rig", registeredBackup)
	}
	if got := registeredBackup["managed"]; got != doltDataDir {
		t.Fatalf("managed rig data dir = %q, want runtime layout data dir %q", got, doltDataDir)
	}
	if _, ok := registeredBackup["filebacked"]; ok {
		t.Fatalf("file-backed rig should not register dolt-backup check: %#v", registeredBackup)
	}
	if _, ok := registeredBackup["sleeping"]; ok {
		t.Fatalf("suspended rig should not register dolt-backup check: %#v", registeredBackup)
	}
	if len(registeredLocalOnly) != 1 {
		t.Fatalf("registered dolt-local-only checks = %#v, want only active managed rig", registeredLocalOnly)
	}
	if got := registeredLocalOnly["managed"]; got != doltDataDir {
		t.Fatalf("managed rig local-only data dir = %q, want runtime layout data dir %q", got, doltDataDir)
	}
	if _, ok := registeredLocalOnly["filebacked"]; ok {
		t.Fatalf("file-backed rig should not register dolt-local-only check: %#v", registeredLocalOnly)
	}
	if _, ok := registeredLocalOnly["sleeping"]; ok {
		t.Fatalf("suspended rig should not register dolt-local-only check: %#v", registeredLocalOnly)
	}
}

func TestBuildDoctorChecksSkipsRigDoltChecks(t *testing.T) {
	clearInheritedBeadsEnv(t)

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "managed")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "managed"
path = "managed"
prefix = "ma"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "managed",
	}); err != nil {
		t.Fatal(err)
	}

	oldRigCheck := newDoctorRigDoltServerCheck
	var rigSkip *bool
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, skip bool) *doctor.RigDoltServerCheck {
		rigSkip = &skip
		return doctor.NewRigDoltServerCheck(cityPath, rig, skip)
	}
	t.Cleanup(func() {
		newDoctorRigDoltServerCheck = oldRigCheck
	})

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "managed", Path: "managed", Prefix: "ma"}},
	}
	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    true,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
		SkipRigDoltChecks:    true,
	})

	if rigSkip == nil {
		t.Error("rig dolt-server check was not registered")
	} else if !*rigSkip {
		t.Error("rig dolt-server check skip = false, want true")
	}
	names := doctorCheckNames(checks)
	for _, name := range []string{"rig:managed:dolt-backup", "rig:managed:dolt-local-only-remote"} {
		if doctorCheckIndex(names, name) >= 0 {
			t.Errorf("check %q registered with SkipRigDoltChecks=true; names=%v", name, names)
		}
	}
}

func TestDoDoctorRunsDoltTopologyForBdRigUnderFileBackedCity(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[[rigs]]
name = "frontend"
path = "frontend"
prefix = "fe"
dolt_host = "rig.example.com"
dolt_port = "3308"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "fe",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "fe",
	}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", cityDir)

	oldCityCheck := newDoctorDoltServerCheck
	oldRigCheck := newDoctorRigDoltServerCheck
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}
	t.Cleanup(func() {
		newDoctorDoltServerCheck = oldCityCheck
		newDoctorRigDoltServerCheck = oldRigCheck
	})

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, false, false, false, 0, &stdout, &stderr)

	if !strings.Contains(stdout.String(), "canonical/compat Dolt drift") {
		t.Fatalf("doctor output missing Dolt topology drift:\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}
}

func TestDoDoctorRegistersStaleLocalPackDirCheck(t *testing.T) {
	skipSlowCmdGCTest(t, "starts real Dolt lifecycle")
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "packs", "actual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[packs.actual]
source = "https://github.com/gastownhall/gc-actual-packs"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_DOLT", "skip")
	cleanupManagedDoltTestCity(t, cityDir)

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, false, false, 0, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "stale-local-pack-dirs") {
		t.Fatalf("doctor output missing stale-local-pack-dirs check:\n%s", out)
	}
	if !strings.Contains(out, "delete `packs/actual/` (it's stale); edits go via PR on gc-actual-packs") {
		t.Fatalf("doctor output missing stale pack action:\n%s", out)
	}
}

func TestDoDoctorRegistersStaleLocalPackDirCheckForRemoteImport(t *testing.T) {
	cityDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	source := "https://github.com/gastownhall/gc-actual-packs"
	commit := writeDoctorRemotePackFixture(t, homeDir, source)

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "packs", "actual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[session]
provider = "fake"

[imports.actual]
source = "https://github.com/gastownhall/gc-actual-packs"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoctorPackLock(t, cityDir, source, commit)

	out := runDoctorForStaleLocalPackDirTest(t, cityDir)
	if !strings.Contains(out, "stale-local-pack-dirs") {
		t.Fatalf("doctor output missing stale-local-pack-dirs check:\n%s", out)
	}
	if !strings.Contains(out, "packs/actual exists while [imports.actual] points at https://github.com/gastownhall/gc-actual-packs") {
		t.Fatalf("doctor output missing remote import stale pack detail:\n%s", out)
	}
}

func TestDoDoctorRegistersStaleLocalPackDirCheckForRigRemoteImport(t *testing.T) {
	cityDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	t.Setenv("GC_HOME", filepath.Join(homeDir, ".gc"))
	source := "https://github.com/gastownhall/gc-actual-packs"
	commit := writeDoctorRemotePackFixture(t, homeDir, source)

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "rig"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "packs", "actual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[session]
provider = "fake"

[[rigs]]
name = "demo-rig"
path = "rig"

[rigs.imports.actual]
source = "https://github.com/gastownhall/gc-actual-packs"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoctorPackLock(t, cityDir, source, commit)

	out := runDoctorForStaleLocalPackDirTest(t, cityDir)
	if !strings.Contains(out, "stale-local-pack-dirs") {
		t.Fatalf("doctor output missing stale-local-pack-dirs check:\n%s", out)
	}
	if !strings.Contains(out, "packs/actual exists while [rigs.demo-rig.imports.actual] points at https://github.com/gastownhall/gc-actual-packs") {
		t.Fatalf("doctor output missing rig remote import stale pack detail:\n%s", out)
	}
}

func TestDoDoctorRegistersStaleLocalPackDirCheckForDefaultRigRemoteImport(t *testing.T) {
	skipSlowCmdGCTest(t, "starts real Dolt lifecycle")
	cityDir := t.TempDir()
	homeDir := t.TempDir()
	t.Setenv("HOME", homeDir)
	source := "https://github.com/gastownhall/gc-actual-packs"
	commit := writeDoctorRemotePackFixture(t, homeDir, source)

	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, "packs", "actual"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"

[session]
provider = "fake"

[defaults.rig.imports.actual]
source = "https://github.com/gastownhall/gc-actual-packs"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "pack.toml"), []byte(`[pack]
name = "demo"
schema = 2
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoctorPackLock(t, cityDir, source, commit)

	out := runDoctorForStaleLocalPackDirTest(t, cityDir)
	if !strings.Contains(out, "stale-local-pack-dirs") {
		t.Fatalf("doctor output missing stale-local-pack-dirs check:\n%s", out)
	}
	if !strings.Contains(out, "packs/actual exists while [defaults.rig.imports.actual] points at https://github.com/gastownhall/gc-actual-packs") {
		t.Fatalf("doctor output missing default rig remote import stale pack detail:\n%s", out)
	}
}

func writeDoctorRemotePackFixture(t *testing.T, homeDir, source string) string {
	t.Helper()

	repoDir := filepath.Join(t.TempDir(), "remote-pack")
	if err := os.MkdirAll(repoDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoDir, "pack.toml"), []byte(`[pack]
name = "actual"
schema = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGitImport(t, repoDir, "init")
	mustGitImport(t, repoDir, "add", ".")
	mustGitImport(t, repoDir, "commit", "-m", "initial")
	commit := gitOutputImport(t, repoDir, "rev-parse", "HEAD")
	cacheDir := filepath.Join(homeDir, ".gc", "cache", "repos", config.RepoCacheKey(source, commit))
	if err := os.MkdirAll(filepath.Dir(cacheDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(repoDir, cacheDir); err != nil {
		t.Fatal(err)
	}
	return commit
}

func writeDoctorPackLock(t *testing.T, cityDir, source, commit string) {
	t.Helper()

	if err := os.WriteFile(filepath.Join(cityDir, "packs.lock"), []byte(`schema = 1

[packs."`+source+`"]
version = "1.0.0"
commit = "`+commit+`"
fetched = "2026-05-20T00:00:00Z"
`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func runDoctorForStaleLocalPackDirTest(t *testing.T, cityDir string) string {
	t.Helper()

	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_DOLT", "skip")
	cleanupManagedDoltTestCity(t, cityDir)

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, false, false, 0, &stdout, &stderr)
	return stdout.String() + stderr.String()
}

func TestDoDoctorReportsLegacyBDSplitStore(t *testing.T) {
	cityDir := t.TempDir()
	writeMinimalCityToml(t, cityDir)
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"server","dolt_database":"hq"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{
		filepath.Join(cityDir, ".beads", "dolt", "hq", ".dolt"),
		filepath.Join(cityDir, ".beads", "embeddeddolt", "legacy", ".dolt"),
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("GC_BEADS", "file")
	origCityFlag := cityFlag
	cityFlag = cityDir
	t.Cleanup(func() { cityFlag = origCityFlag })

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, false, false, false, 0, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "bd-split-store") {
		t.Fatalf("doctor output missing bd-split-store check:\n%s", out)
	}
	if !strings.Contains(out, "legacy split store") {
		t.Fatalf("doctor output missing split-store warning:\n%s", out)
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

func TestDoctorSkipsSuspendedRigChecks(t *testing.T) {
	t.Parallel()
	activeDir := t.TempDir()
	suspendedDir := t.TempDir()

	rigs := []config.Rig{
		{Name: "active-rig", Path: activeDir},
		{Name: "suspended-rig", Path: suspendedDir, SuspendedOnStart: true},
	}

	// Mirror the per-rig registration logic from doDoctor.
	d := &doctor.Doctor{}
	for _, rig := range rigs {
		if suspensionstate.EffectiveRigSuspended(suspensionstate.State{}, rig.Name, rig.SuspendedOnStart) {
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

// TestWriteDoctorJSONProjectsTimedOut pins the machine-readable timeout
// signal: a timed-out check projects timed_out=true so automation can tell an
// abandoned check (outcome unknown) from an ordinary advisory failure, while
// omitempty keeps output byte-identical for every non-timed-out result.
func TestWriteDoctorJSONProjectsTimedOut(t *testing.T) {
	report := &doctor.Report{
		Failed: 2,
		Results: []*doctor.CheckResult{
			{Name: "wedged", Status: doctor.StatusError, Severity: doctor.SeverityAdvisory, Message: "timed out", TimedOut: true},
			{Name: "advisory", Status: doctor.StatusError, Severity: doctor.SeverityAdvisory, Message: "ran and found an advisory issue"},
		},
	}
	var buf bytes.Buffer
	if err := writeDoctorJSON(&buf, report); err != nil {
		t.Fatalf("writeDoctorJSON: %v", err)
	}
	var decoded doctorJSONReport
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode doctor JSON: %v; out=%q", err, buf.String())
	}
	if len(decoded.Results) != 2 {
		t.Fatalf("Results = %d, want 2", len(decoded.Results))
	}
	if !decoded.Results[0].TimedOut {
		t.Fatalf("timed-out check projected TimedOut=false, want true")
	}
	if decoded.Results[1].TimedOut {
		t.Fatalf("ordinary advisory check projected TimedOut=true, want false")
	}
	// omitempty: only the abandoned check carries the key, so existing --json
	// consumers of non-timed-out results see unchanged output.
	if n := strings.Count(buf.String(), "timed_out"); n != 1 {
		t.Fatalf("timed_out appears %d times, want exactly 1 (only the abandoned check); out=%s", n, buf.String())
	}
}
