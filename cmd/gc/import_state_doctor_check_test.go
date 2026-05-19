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

func TestImportStateDoctorCheckReportsInstallHint(t *testing.T) {
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
				RepairHint: `run "gc import install"`,
			}},
		}, nil
	}

	check := newImportStateDoctorCheck(cityDir)
	result := check.Run(&doctor.CheckContext{CityPath: cityDir, Verbose: true})
	if result.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error; result=%#v", result.Status, result)
	}
	if check.CanFix() || result.FixHint != `run "gc import install"` {
		t.Fatalf("result = %#v, want non-fixable install hint", result)
	}
	if len(result.Details) != 1 || !strings.Contains(result.Details[0], "missing-cache") {
		t.Fatalf("details = %#v", result.Details)
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
				RepairHint: `run "gc import install"`,
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
	_ = doDoctor(false, true, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "packv2-import-state") || !strings.Contains(out, `run "gc import install"`) {
		t.Fatalf("doctor output missing import state check:\n%s", out)
	}
}

func TestDoDoctorRunsImportStateCheckWhenImportInstallStateBroken(t *testing.T) {
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
	_ = doDoctor(false, true, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if !strings.Contains(out, "packv2-import-state") || !strings.Contains(out, "missing-lockfile") {
		t.Fatalf("doctor output missing import-state failure for broken install state:\n%s", out)
	}
	if !strings.Contains(out, `run "gc import install"`) {
		t.Fatalf("doctor output missing install hint:\n%s", out)
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
	_ = doDoctor(false, true, &stdout, &stderr)
	out := stdout.String() + stderr.String()
	if strings.Contains(out, "packv2-import-state") {
		t.Fatalf("doctor output included import state check for invalid config:\n%s", out)
	}
	if !strings.Contains(out, "city-config") {
		t.Fatalf("doctor output missing city config failure:\n%s", out)
	}
}
