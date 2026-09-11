package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestBuildDoctorChecks_NameSetUnchanged(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	// Force healthy preflight so ambient bd/dolt cannot change the name-set.
	old := doctorBeadStorePreflight
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error { return nil }
	t.Cleanup(func() { doctorBeadStorePreflight = old })

	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})
	names := doctorCheckNames(checks)

	data, err := os.ReadFile(filepath.Join("testdata", "doctor_check_names.golden"))
	if err != nil {
		t.Fatalf("read golden: %v", err)
	}
	want := strings.TrimSpace(string(data))
	got := strings.Join(names, "\n")
	if got != want {
		t.Fatalf("doctor check names changed\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestBuildDoctorChecksRegistersNamedAlwaysMinConflictCheck(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    false,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	}))

	formulaRequirements := doctorCheckIndex(names, "formula-requirements")
	if formulaRequirements < 0 {
		t.Fatalf("formula-requirements check missing: %v", names)
	}
	got := doctorCheckIndex(names, "named-always-min-conflict")
	if got != formulaRequirements+1 {
		t.Fatalf("named-always-min-conflict index = %d, want immediately after formula-requirements at %d; names=%v", got, formulaRequirements, names)
	}
}

func TestBuildDoctorChecksSkipsNamedAlwaysMinConflictCheckWithoutConfig(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")

	tests := []struct {
		name   string
		cfg    *config.City
		cfgErr error
	}{
		{name: "nil config", cfg: nil, cfgErr: nil},
		{name: "config load error", cfg: &config.City{Workspace: config.Workspace{Name: "demo"}}, cfgErr: os.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			names := doctorCheckNames(buildDoctorChecks(cityDir, tt.cfg, tt.cfgErr, buildDoctorChecksOpts{
				ControllerRunning:    false,
				SkipCityDoltCheck:    true,
				SkipManagedDoltCheck: true,
			}))
			if got := doctorCheckIndex(names, "named-always-min-conflict"); got >= 0 {
				t.Fatalf("named-always-min-conflict registered at %d, want absent; names=%v", got, names)
			}
		})
	}
}

// TestBuildDoctorChecksSessionLivenessChecksRegisteredRegardlessOfController_GH5742
// is the inverted characterization test from ga-o04bfr.1.6: while the
// controller runs, buildDoctorChecks must still register the read-only
// session-liveness checks so a dead/zombie named session produces a doctor
// finding instead of zero findings (gastownhall/gascity#5742).
func TestBuildDoctorChecks_SessionLivenessChecksRegisteredRegardlessOfController_GH5742(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Agents:    []config.Agent{{Name: "worker", ProcessNames: []string{"claude"}}},
	}
	sessionChecks := []string{"agent-sessions", "zombie-sessions", "orphan-sessions"}
	for _, running := range []bool{true, false} {
		names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
			ControllerRunning: running, SkipCityDoltCheck: true, SkipManagedDoltCheck: true,
		}))
		for _, name := range sessionChecks {
			if idx := doctorCheckIndex(names, name); idx < 0 {
				t.Errorf("ControllerRunning=%v: %q expected but missing; names=%v", running, name, names)
			}
		}
	}
}

// TestBuildDoctorChecks_StartupHealthEpisodesRegisteredRegardlessOfController_GH5742
// is the ga-o04bfr.1.4 counterpart to the session-liveness invariance test
// above: startup-health-episodes is a read-only reporting check (GH#5742)
// registered outside any controller-state gate, and must appear in the check
// list whether or not the controller is running. Unlike the session-liveness
// checks, this one is store-dependent (gated by the bead-store preflight), so
// the preflight is forced healthy to keep ambient bd/dolt state from masking
// the registration.
func TestBuildDoctorChecks_StartupHealthEpisodesRegisteredRegardlessOfController_GH5742(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	old := doctorBeadStorePreflight
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error { return nil }
	t.Cleanup(func() { doctorBeadStorePreflight = old })

	for _, running := range []bool{true, false} {
		names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
			ControllerRunning: running, SkipCityDoltCheck: true, SkipManagedDoltCheck: true,
		}))
		if idx := doctorCheckIndex(names, "startup-health-episodes"); idx < 0 {
			t.Errorf("ControllerRunning=%v: startup-health-episodes expected but missing; names=%v", running, names)
		}
	}
}

func doctorCheckNames(checks []doctor.Check) []string {
	names := make([]string, 0, len(checks))
	for _, check := range checks {
		names = append(names, check.Name())
	}
	return names
}

func doctorCheckIndex(names []string, want string) int {
	for i, name := range names {
		if name == want {
			return i
		}
	}
	return -1
}
