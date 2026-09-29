package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func writeDoctorProxiedScope(t *testing.T, scopeRoot, db string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(scopeRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", "metadata.json"),
		[]byte(`{"backend":"dolt","database":"dolt","dolt_mode":"proxied-server","dolt_database":"`+db+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func proxiedBackupCoverageCheckIn(checks []doctor.Check) doctor.Check {
	for _, c := range checks {
		if c.Name() == "proxied-backup-coverage" {
			return c
		}
	}
	return nil
}

// bd 1.3.1's `bd backup status` starts a stopped proxied scope's proxy and
// Dolt, so the coverage check must never reach a suspended rig, must stay
// unregistered when the store preflight failed, and must not ask bd about a
// scope whose proxy is not running. PATH holds no bd here: any bd call would
// surface as "could not read bd backup status" instead of "not checked".
func TestBuildDoctorChecksProxiedBackupCoverageNeverWakesAStore(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoctorProxiedScope(t, cityDir, "hq")
	writeDoctorProxiedScope(t, filepath.Join(cityDir, "awake"), "aw")
	writeDoctorProxiedScope(t, filepath.Join(cityDir, "sleeping"), "sl")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("PATH", filepath.Join(cityDir, "empty-path"))
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "awake", Path: "awake", Prefix: "aw"},
			{Name: "sleeping", Path: "sleeping", Prefix: "sl", SuspendedOnStart: true},
		},
	}
	opts := buildDoctorChecksOpts{SkipCityDoltCheck: true, SkipManagedDoltCheck: true, SkipRigDoltChecks: true}

	old := doctorBeadStorePreflight
	t.Cleanup(func() { doctorBeadStorePreflight = old })

	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error { return nil }
	check := proxiedBackupCoverageCheckIn(buildDoctorChecks(cityDir, cfg, nil, opts))
	if check == nil {
		t.Fatal("proxied-backup-coverage not registered for a proxied city with a healthy store")
	}
	result := check.Run(&doctor.CheckContext{CityPath: cityDir})
	if !strings.Contains(result.Message, "not checked: store not running (city, awake)") {
		t.Errorf("message = %q, want the city and the active rig reported as not checked", result.Message)
	}
	for _, unwanted := range []string{"sleeping", "could not read bd backup status"} {
		if strings.Contains(result.Message, unwanted) {
			t.Errorf("message mentions %q: %q", unwanted, result.Message)
		}
	}

	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error {
		return errors.New("dolt server unreachable")
	}
	if c := proxiedBackupCoverageCheckIn(buildDoctorChecks(cityDir, cfg, nil, opts)); c != nil {
		t.Error("proxied-backup-coverage registered although the store preflight failed")
	}
}
