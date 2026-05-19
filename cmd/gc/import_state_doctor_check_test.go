package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/packman"
)

func TestImportStateDoctorCheckReportsOK(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevCheck := checkInstalledImports
	t.Cleanup(func() { checkInstalledImports = prevCheck })
	checkInstalledImports = func(_ string, imports map[string]config.Import) (*packman.CheckReport, error) {
		if _, ok := imports["pack:tools"]; !ok {
			t.Fatalf("imports = %#v, want pack:tools", imports)
		}
		return &packman.CheckReport{CheckedSources: 1}, nil
	}

	result := newImportStateDoctorCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "1 remote import(s) installed") {
		t.Fatalf("message = %q", result.Message)
	}
}

func TestImportStateDoctorCheckReportsSyncHint(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevCheck := checkInstalledImports
	t.Cleanup(func() { checkInstalledImports = prevCheck })
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		return &packman.CheckReport{
			CheckedSources: 1,
			Issues: []packman.CheckIssue{{
				Severity:   packman.CheckSeverityError,
				Code:       "missing-cache",
				ImportName: "pack:tools",
				Source:     "https://example.com/tools.git",
				Commit:     "abc123",
				Path:       filepath.Join(cityDir, ".gc", "cache", "repos", "abc"),
				Message:    "locked import is missing from the local repo cache",
				RepairHint: `run "gc pack sync"`,
			}},
		}, nil
	}

	check := newImportStateDoctorCheck(cityDir)
	result := check.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if !check.CanFix() || !strings.Contains(result.FixHint, `gc doctor --fix`) || !strings.Contains(result.FixHint, `gc pack sync`) {
		t.Fatalf("result = %#v, want fixable doctor/sync hint", result)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "missing-cache") {
		t.Fatalf("details = %#v", result.Details)
	}
}

func TestImportStateDoctorCheckFixRunsPackSyncPath(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevSync := syncImports
	prevInstall := installLockedImports
	prevCheck := checkInstalledImports
	t.Cleanup(func() {
		syncImports = prevSync
		installLockedImports = prevInstall
		checkInstalledImports = prevCheck
	})
	synced := false
	installed := false
	lock := &packman.Lockfile{
		Schema: packman.LockfileSchema,
		Packs: map[string]packman.LockedPack{
			"https://example.com/tools.git": {Version: "1.1.0", Commit: "new"},
		},
	}
	syncImports = func(cityRoot string, imports map[string]config.Import, mode packman.InstallMode) (*packman.Lockfile, error) {
		if cityRoot != cityDir {
			t.Fatalf("sync cityRoot = %q, want %q", cityRoot, cityDir)
		}
		if _, ok := imports["pack:tools"]; !ok {
			t.Fatalf("sync imports = %#v, want pack:tools", imports)
		}
		if mode != packman.InstallResolveIfNeeded {
			t.Fatalf("sync mode = %v, want InstallResolveIfNeeded", mode)
		}
		synced = true
		return lock, nil
	}
	installLockedImports = func(cityRoot string) (*packman.Lockfile, error) {
		if cityRoot != cityDir {
			t.Fatalf("install cityRoot = %q, want %q", cityRoot, cityDir)
		}
		installed = true
		return lock, nil
	}
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		if !installed {
			return &packman.CheckReport{Issues: []packman.CheckIssue{{
				Severity: packman.CheckSeverityError,
				Code:     "missing-lockfile",
			}}}, nil
		}
		return &packman.CheckReport{CheckedSources: 1}, nil
	}

	check := newImportStateDoctorCheck(cityDir)
	before := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if before.Status != doctor.StatusError {
		t.Fatalf("before status = %v, want error", before.Status)
	}
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if !synced || !installed {
		t.Fatalf("sync/install called = %v/%v, want both", synced, installed)
	}
	after := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if after.Status != doctor.StatusOK {
		t.Fatalf("after status = %v, want OK; result=%#v", after.Status, after)
	}
}

func TestImportStateDoctorCheckReportsDurableRegistrySelectors(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.lighthouse]
source = "registry:main:lighthouse"
version = "^1.0"
`)

	prevCheck := checkInstalledImports
	t.Cleanup(func() { checkInstalledImports = prevCheck })
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		t.Fatal("checkInstalledImports should not run when durable registry selectors are present")
		return nil, nil
	}

	result := newImportStateDoctorCheck(cityDir).Run(&doctor.CheckContext{CityPath: cityDir})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if !strings.Contains(result.Message, "command-time registry selectors") {
		t.Fatalf("message = %q", result.Message)
	}
	if !strings.Contains(result.FixHint, "gc pack add") {
		t.Fatalf("fix hint = %q", result.FixHint)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "registry-selector-source") || !strings.Contains(result.Details[0], "registry:main:lighthouse") {
		t.Fatalf("details = %#v", result.Details)
	}
	if err := newImportStateDoctorCheck(cityDir).Fix(&doctor.CheckContext{CityPath: cityDir}); err == nil {
		t.Fatal("Fix succeeded for durable registry selector, want manual error")
	}
}

func TestDoDoctorRegistersImportStateCheck(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevCityFlag := cityFlag
	prevCheck := checkInstalledImports
	prevCityDoltCheck := newDoctorDoltServerCheck
	prevRigDoltCheck := newDoctorRigDoltServerCheck
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		checkInstalledImports = prevCheck
		newDoctorDoltServerCheck = prevCityDoltCheck
		newDoctorRigDoltServerCheck = prevRigDoltCheck
	})
	cityFlag = cityDir
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		return &packman.CheckReport{
			Issues: []packman.CheckIssue{{
				Severity:   packman.CheckSeverityError,
				Code:       "missing-lockfile",
				RepairHint: `run "gc pack sync"`,
			}},
		}, nil
	}
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, false, false, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "packv2-import-state") || !strings.Contains(out, `gc pack sync`) {
		t.Fatalf("doctor output missing import state check:\n%s", out)
	}
	if strings.Contains(out, `run "gc import install"`) {
		t.Fatalf("doctor output used stale import install hint:\n%s", out)
	}
}

func TestDoDoctorRunsImportStateCheckWhenPackSyncStateBroken(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCityToml(t, cityDir, "[workspace]\nname = \"demo\"\n")
	writePackToml(t, cityDir, `[pack]
name = "demo"
schema = 1

[imports.tools]
source = "https://example.com/tools.git"
version = "^1.0"
`)

	prevCityFlag := cityFlag
	prevCityDoltCheck := newDoctorDoltServerCheck
	prevRigDoltCheck := newDoctorRigDoltServerCheck
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		newDoctorDoltServerCheck = prevCityDoltCheck
		newDoctorRigDoltServerCheck = prevRigDoltCheck
	})
	cityFlag = cityDir
	newDoctorDoltServerCheck = func(cityPath string, _ bool) *doctor.DoltServerCheck {
		return doctor.NewDoltServerCheck(cityPath, true)
	}
	newDoctorRigDoltServerCheck = func(cityPath string, rig config.Rig, _ bool) *doctor.RigDoltServerCheck {
		return doctor.NewRigDoltServerCheck(cityPath, rig, true)
	}

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, false, false, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "packv2-import-state") || !strings.Contains(out, "missing-lockfile") {
		t.Fatalf("doctor output missing import-state failure for broken sync state:\n%s", out)
	}
	if !strings.Contains(out, `gc pack sync`) {
		t.Fatalf("doctor output missing sync hint:\n%s", out)
	}
	if strings.Contains(out, `run "gc import install"`) {
		t.Fatalf("doctor output used stale import install hint:\n%s", out)
	}
}

func TestDoDoctorSkipsImportStateCheckWhenCityConfigInvalid(t *testing.T) {
	clearGCEnv(t)
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	prevCityFlag := cityFlag
	prevCheck := checkInstalledImports
	t.Cleanup(func() {
		cityFlag = prevCityFlag
		checkInstalledImports = prevCheck
	})
	cityFlag = cityDir
	checkInstalledImports = func(_ string, _ map[string]config.Import) (*packman.CheckReport, error) {
		t.Fatal("import state check should not run when city.toml cannot load")
		return nil, nil
	}

	var stdout, stderr bytes.Buffer
	_ = doDoctor(false, true, false, false, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if strings.Contains(out, "packv2-import-state") {
		t.Fatalf("doctor output included import state check for invalid config:\n%s", out)
	}
	if !strings.Contains(out, "city-config") {
		t.Fatalf("doctor output missing city config failure:\n%s", out)
	}
}
