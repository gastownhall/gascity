package main

// RED tests for the dolt-local-only-remote doctor check registration in
// cmd_doctor.go (ga-673qo6.1).
//
// These tests must fail to compile until the builder implements:
//   - cmd/gc/cmd_doctor.go: newDoctorDoltLocalOnlyCheck var + registration
//     after newDoctorDoltBackupCheck at the managed-bdstore gate.
//   - internal/doctor/checks_dolt_local_only.go: DoltLocalOnlyRemoteCheck
//     and NewDoltLocalOnlyRemoteCheck (see checks_dolt_local_only_test.go).

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
)

const cityTomlWithManagedRigs = `[workspace]
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
name = "suspended"
path = "suspended"
prefix = "su"
suspended = true
`

// TestDoDoctorRegistersLocalOnlyRemoteCheckForActiveManagedRigs verifies that
// the dolt-local-only-remote check is registered for each active managed-dolt
// rig, mirroring the gate used for newDoctorDoltBackupCheck.
func TestDoDoctorRegistersLocalOnlyRemoteCheckForActiveManagedRigs(t *testing.T) {
	clearInheritedBeadsEnv(t)

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTomlWithManagedRigs), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"managed", "filebacked", "suspended"} {
		if err := os.MkdirAll(filepath.Join(cityDir, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"managed", "suspended"} {
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
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_DOLT_DATA_DIR", doltDataDir)
	oldCityFlag := cityFlag
	cityFlag = cityDir
	t.Cleanup(func() { cityFlag = oldCityFlag })

	oldCityCheck := newDoctorDoltServerCheck
	oldRigCheck := newDoctorRigDoltServerCheck
	oldBackupCheck := newDoctorDoltBackupCheck
	oldLocalOnlyCheck := newDoctorDoltLocalOnlyCheck
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}
	newDoctorDoltBackupCheck = doctor.NewDoltBackupCheck
	registered := map[string]string{}
	newDoctorDoltLocalOnlyCheck = func(cityPath string, rig config.Rig, dataDir string) *doctor.DoltLocalOnlyRemoteCheck {
		registered[rig.Name] = dataDir
		return doctor.NewDoltLocalOnlyRemoteCheck(cityPath, rig, dataDir)
	}
	t.Cleanup(func() {
		newDoctorDoltServerCheck = oldCityCheck
		newDoctorRigDoltServerCheck = oldRigCheck
		newDoctorDoltBackupCheck = oldBackupCheck
		newDoctorDoltLocalOnlyCheck = oldLocalOnlyCheck
	})

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, false, false, false, &stdout, &stderr)

	if len(registered) != 1 {
		t.Fatalf("registered dolt-local-only checks = %#v, want exactly the active managed rig", registered)
	}
	if got := registered["managed"]; got != doltDataDir {
		t.Fatalf("managed rig data dir = %q, want %q", got, doltDataDir)
	}
	if _, ok := registered["filebacked"]; ok {
		t.Fatalf("file-backed rig should not register dolt-local-only check: %#v", registered)
	}
	if _, ok := registered["suspended"]; ok {
		t.Fatalf("suspended rig should not register dolt-local-only check: %#v", registered)
	}
}

// TestDoDoctorSkipsLocalOnlyCheckWhenGCDoltSkip verifies that the
// dolt-local-only-remote check is not registered when GC_DOLT=skip, matching
// the behavior of the existing dolt-backup check.
func TestDoDoctorSkipsLocalOnlyCheckWhenGCDoltSkip(t *testing.T) {
	clearInheritedBeadsEnv(t)

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "managed")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
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
	t.Setenv("GC_CITY_PATH", cityDir)
	t.Setenv("GC_DOLT", "skip")
	oldCityFlag := cityFlag
	cityFlag = cityDir
	t.Cleanup(func() { cityFlag = oldCityFlag })

	oldCityCheck := newDoctorDoltServerCheck
	oldRigCheck := newDoctorRigDoltServerCheck
	oldBackupCheck := newDoctorDoltBackupCheck
	oldLocalOnlyCheck := newDoctorDoltLocalOnlyCheck
	registered := 0
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}
	newDoctorDoltBackupCheck = doctor.NewDoltBackupCheck
	newDoctorDoltLocalOnlyCheck = func(cityPath string, rig config.Rig, dataDir string) *doctor.DoltLocalOnlyRemoteCheck {
		registered++
		return doctor.NewDoltLocalOnlyRemoteCheck(cityPath, rig, dataDir)
	}
	t.Cleanup(func() {
		newDoctorDoltServerCheck = oldCityCheck
		newDoctorRigDoltServerCheck = oldRigCheck
		newDoctorDoltBackupCheck = oldBackupCheck
		newDoctorDoltLocalOnlyCheck = oldLocalOnlyCheck
	})

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, false, false, false, &stdout, &stderr)

	if registered != 0 {
		t.Fatalf("registered %d dolt-local-only checks, want 0 when GC_DOLT=skip", registered)
	}
}
