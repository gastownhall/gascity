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

// stubDoctorStoreLiveness makes the scopes whose base name is in stopped read
// as stopped proxied stores, and fails the test if the bead-store preflight
// runs against a stopped city.
func stubDoctorStoreLiveness(t *testing.T, stopped ...string) {
	t.Helper()
	oldLive := doctorProxiedStoreNotRunning
	oldPreflight := doctorBeadStorePreflight
	t.Cleanup(func() {
		doctorProxiedStoreNotRunning = oldLive
		doctorBeadStorePreflight = oldPreflight
	})
	doctorProxiedStoreNotRunning = func(scopeRoot string) bool {
		for _, name := range stopped {
			if filepath.Base(scopeRoot) == name {
				return true
			}
		}
		return false
	}
	doctorBeadStorePreflight = func(cityPath string, _ func(string) (beads.Store, error)) error {
		if doctorProxiedStoreNotRunning(cityPath) {
			t.Error("bead-store preflight ran against a stopped proxied city")
		}
		return nil
	}
}

func doctorStoreGateCity(t *testing.T) (string, *config.City) {
	t.Helper()
	root := t.TempDir()
	cityDir := filepath.Join(root, "citystore")
	if err := os.MkdirAll(cityDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")
	return cityDir, &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "rigstore", Path: "rigstore", Prefix: "rs"}},
	}
}

func runDoctorCheckNamed(t *testing.T, checks []doctor.Check, name string) *doctor.CheckResult {
	t.Helper()
	for _, c := range checks {
		if c.Name() == name {
			return c.Run(&doctor.CheckContext{})
		}
	}
	t.Fatalf("no %s check registered", name)
	return nil
}

// A stopped proxied city: every check that reads the city store — the three
// session checks, which build the store-backed session provider, and every
// check the preflight gates — keeps its name but reports "not checked", and
// the preflight never runs.
func TestBuildDoctorChecksNeverReadsAStoppedCityStore(t *testing.T) {
	cityDir, cfg := doctorStoreGateCity(t)
	stubDoctorStoreLiveness(t, "citystore", "rigstore")

	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{SkipCityDoltCheck: true, SkipManagedDoltCheck: true, SkipRigDoltChecks: true})
	for _, name := range []string{
		"agent-sessions", "zombie-sessions", "orphan-sessions", "order-firing-current",
		"beads-store", "custom-types:city", "hold-label-conventions:city",
		"rig:rigstore:beads", "custom-types:rigstore", "hold-label-conventions:rigstore",
	} {
		r := runDoctorCheckNamed(t, checks, name)
		if r.Status != doctor.StatusOK || !strings.Contains(r.Message, doctor.StoreNotRunningMessage) {
			t.Errorf("%s = %v %q, want %q", name, r.Status, r.Message, doctor.StoreNotRunningMessage)
		}
		for _, c := range checks {
			if c.Name() == name && c.CanFix() {
				t.Errorf("%s offers a fix on a stopped store", name)
			}
		}
	}
}

// A running city with a stopped rig: the city's checks run, the rig's own
// checks report "not checked", and the shared store factory refuses the rig so
// a city-level check that walks rigs cannot wake it.
func TestBuildDoctorChecksNeverReadsAStoppedRigStore(t *testing.T) {
	cityDir, cfg := doctorStoreGateCity(t)
	stubDoctorStoreLiveness(t, "rigstore")

	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{SkipCityDoltCheck: true, SkipManagedDoltCheck: true, SkipRigDoltChecks: true})
	for _, name := range []string{"rig:rigstore:beads", "custom-types:rigstore", "hold-label-conventions:rigstore"} {
		if r := runDoctorCheckNamed(t, checks, name); !strings.Contains(r.Message, doctor.StoreNotRunningMessage) {
			t.Errorf("%s = %q, want not checked", name, r.Message)
		}
	}
	for _, c := range checks {
		if c.Name() == "custom-types:city" {
			if _, standIn := c.(*doctor.CustomTypesCheck); !standIn {
				t.Errorf("custom-types:city was replaced although the city store is running: %T", c)
			}
		}
	}

	gate := newDoctorStoreGate(false)
	opened := 0
	factory := gate.StoreFactory(func(string) (beads.Store, error) { opened++; return nil, nil })
	if _, err := factory(filepath.Join(cityDir, "rigstore")); !errors.Is(err, errDoctorStoreNotRunning) {
		t.Errorf("store factory on a stopped rig: err = %v, want errDoctorStoreNotRunning", err)
	}
	if _, err := factory(cityDir); err != nil {
		t.Errorf("store factory on a running city: %v", err)
	}
	if opened != 1 {
		t.Errorf("opened %d stores, want only the running city's", opened)
	}
}

// A running city (controller up) whose store proxy is dead: the skipped checks
// warn rather than read as OK, so the fault is not hidden.
func TestBuildDoctorChecksWarnsOnAStoppedStoreUnderARunningCity(t *testing.T) {
	cityDir, cfg := doctorStoreGateCity(t)
	stubDoctorStoreLiveness(t, "citystore")

	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{ControllerRunning: true, SkipCityDoltCheck: true, SkipManagedDoltCheck: true, SkipRigDoltChecks: true})
	for _, name := range []string{"agent-sessions", "beads-store", "custom-types:city"} {
		r := runDoctorCheckNamed(t, checks, name)
		if r.Status != doctor.StatusWarning || !strings.Contains(r.Message, doctor.StoreNotRunningMessage) {
			t.Errorf("%s = %v %q, want a not-running warning", name, r.Status, r.Message)
		}
	}
}
