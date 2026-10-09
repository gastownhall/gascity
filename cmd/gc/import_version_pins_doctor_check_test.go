package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/doctor"
)

const (
	pinsToolsSource = "https://github.com/example/packs/tree/main/tools"
	pinsRolesSource = "https://github.com/example/packs/tree/main/roles"
	pinsRefSource   = "https://github.com/example/other.git//pack#main"
)

func setupImportPinsCity(t *testing.T, packToml, cityToml, lock string) string {
	t.Helper()
	clearGCEnv(t)
	cityDir := t.TempDir()
	writePackToml(t, cityDir, packToml)
	writeCityToml(t, cityDir, cityToml)
	if lock != "" {
		if err := os.WriteFile(filepath.Join(cityDir, "packs.lock"), []byte(lock), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return cityDir
}

func importPinsCheck(cityDir string, resolve func(string, string) (string, error)) *importVersionPinsDoctorCheck {
	c := newImportVersionPinsDoctorCheck(cityDir)
	c.resolveDefault = resolve
	return c
}

func failIfResolved(t *testing.T) func(string, string) (string, error) {
	return func(_, source string) (string, error) {
		t.Fatalf("Run resolved a version for %s; only Fix may resolve", source)
		return "", nil
	}
}

const pinsPackToml = `[pack]
name = "demo"
schema = 2

[imports.tools]
source = "` + pinsToolsSource + `"

[imports.pinned]
source = "https://github.com/example/packs/tree/main/pinned"
version = "^1.2"

[imports.local]
source = "./packs/local"
`

const pinsCityToml = `[workspace]
name = "demo"

[[rigs]]
name = "work"

[rigs.imports.gc]
source = "` + pinsRolesSource + `"
`

const pinsLock = `schema = 1

[packs."` + pinsToolsSource + `"]
version = "1.4.0"
commit = "1111111111111111111111111111111111111111"

[packs."` + pinsRolesSource + `"]
version = "sha:2222222222222222222222222222222222222222"
commit = "2222222222222222222222222222222222222222"
`

func TestImportVersionPinsRunReportsFloatingImports(t *testing.T) {
	cityDir := setupImportPinsCity(t, pinsPackToml, pinsCityToml, pinsLock)
	res := importPinsCheck(cityDir, failIfResolved(t)).Run(&doctor.CheckContext{CityPath: cityDir})

	if res.Status != doctor.StatusWarning || res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("status/severity = %v/%v, want warning/advisory; message=%q", res.Status, res.Severity, res.Message)
	}
	payload, ok := res.Payload.(importVersionPinsPayload)
	if !ok {
		t.Fatalf("payload type = %T", res.Payload)
	}
	want := []unpinnedImport{
		{Import: "pack:tools", Source: pinsToolsSource, LockedCommit: "1111111111111111111111111111111111111111", Fixable: true},
		{Import: "rig:work:gc", Source: pinsRolesSource, LockedCommit: "2222222222222222222222222222222222222222", Fixable: true},
	}
	if len(payload.Unpinned) != len(want) {
		t.Fatalf("unpinned = %+v, want %+v", payload.Unpinned, want)
	}
	for i := range want {
		if payload.Unpinned[i] != want[i] {
			t.Errorf("unpinned[%d] = %+v, want %+v", i, payload.Unpinned[i], want[i])
		}
	}
	if !strings.Contains(strings.Join(res.Details, "\n"), "rig:work:gc") {
		t.Errorf("details = %v, want the rig-scoped import named", res.Details)
	}
}

func TestImportVersionPinsRunCleanWhenEveryRemoteImportIsPinned(t *testing.T) {
	cityDir := setupImportPinsCity(t, `[pack]
name = "demo"
schema = 2

[imports.pinned]
source = "https://github.com/example/packs/tree/main/pinned"
version = "sha:3333333333333333333333333333333333333333"

[imports.local]
source = "./packs/local"
`, "[workspace]\nname = \"demo\"\n", "")
	res := importPinsCheck(cityDir, failIfResolved(t)).Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusOK || res.Payload != nil {
		t.Fatalf("status = %v payload = %v, want OK with no payload; message=%q details=%v", res.Status, res.Payload, res.Message, res.Details)
	}
}

func TestImportVersionPinsFixPinsEveryScope(t *testing.T) {
	cityDir := setupImportPinsCity(t, pinsPackToml, pinsCityToml, pinsLock)
	resolved := map[string]string{
		pinsToolsSource: "^1.4",
		pinsRolesSource: "sha:2222222222222222222222222222222222222222",
	}
	var asked []string
	check := importPinsCheck(cityDir, func(_, source string) (string, error) {
		asked = append(asked, source)
		return resolved[source], nil
	})
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if len(asked) != 2 {
		t.Errorf("resolved %v, want one resolution per floating source", asked)
	}
	pack := readCityFile(t, cityDir, "pack.toml")
	if !strings.Contains(pack, `version = "^1.4"`) || !strings.Contains(pack, `version = "^1.2"`) {
		t.Errorf("pack.toml after fix:\n%s\nwant tools pinned to ^1.4 and pinned left at ^1.2", pack)
	}
	city := readCityFile(t, cityDir, "city.toml")
	if !strings.Contains(city, `version = "sha:2222222222222222222222222222222222222222"`) {
		t.Errorf("city.toml after fix:\n%s\nwant the rig import pinned", city)
	}
	if res := check.Run(&doctor.CheckContext{CityPath: cityDir}); res.Status != doctor.StatusOK {
		t.Errorf("Run after Fix: status = %v, details = %v, want OK", res.Status, res.Details)
	}
}

func TestImportVersionPinsFixWritesNothingWhenResolutionFails(t *testing.T) {
	cityDir := setupImportPinsCity(t, pinsPackToml, pinsCityToml, pinsLock)
	beforePack := readCityFile(t, cityDir, "pack.toml")
	beforeCity := readCityFile(t, cityDir, "city.toml")
	check := importPinsCheck(cityDir, func(_, source string) (string, error) {
		if source == pinsRolesSource {
			return "", errors.New("offline")
		}
		return "^1.4", nil
	})
	err := check.Fix(&doctor.CheckContext{CityPath: cityDir})
	if err == nil || !strings.Contains(err.Error(), "offline") {
		t.Fatalf("Fix error = %v, want the resolution failure", err)
	}
	if readCityFile(t, cityDir, "pack.toml") != beforePack || readCityFile(t, cityDir, "city.toml") != beforeCity {
		t.Error("Fix wrote files although resolution failed")
	}
}

func TestImportVersionPinsEmbeddedRefIsReportedButNotFixed(t *testing.T) {
	cityDir := setupImportPinsCity(t, `[pack]
name = "demo"
schema = 2

[imports.other]
source = "`+pinsRefSource+`"
`, "[workspace]\nname = \"demo\"\n", "")
	check := importPinsCheck(cityDir, failIfResolved(t))
	res := check.Run(&doctor.CheckContext{CityPath: cityDir})
	payload, ok := res.Payload.(importVersionPinsPayload)
	if res.Status != doctor.StatusWarning || !ok || len(payload.Unpinned) != 1 || payload.Unpinned[0].Fixable {
		t.Fatalf("Run = %v %+v, want one unfixable unpinned import", res.Status, res.Payload)
	}
	before := readCityFile(t, cityDir, "pack.toml")
	if err := check.Fix(&doctor.CheckContext{CityPath: cityDir}); err == nil || !strings.Contains(err.Error(), "pack:other") {
		t.Fatalf("Fix error = %v, want a manual-edit error naming pack:other", err)
	}
	if readCityFile(t, cityDir, "pack.toml") != before {
		t.Error("Fix rewrote an import with an embedded ref")
	}
}
