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

func gateSandboxEnvLookup(t *testing.T, cityPath string) map[string]string {
	t.Helper()
	lookup := make(map[string]string)
	for _, v := range gateSandboxEnv(cityPath) {
		if name, value, ok := strings.Cut(v, "="); ok {
			lookup[name] = value
		}
	}
	return lookup
}

// TestGateSandboxEnvMatchesTheGateContract pins what the probe simulates: the
// same HOME sandbox and explicit whitelist convergence hands a gate command.
func TestGateSandboxEnvMatchesTheGateContract(t *testing.T) {
	cityPath := t.TempDir()
	home := t.TempDir()
	credentials := filepath.Join(home, ".config", "beads", "credentials")
	if err := os.MkdirAll(filepath.Dir(credentials), 0o700); err != nil {
		t.Fatalf("mkdir credentials dir: %v", err)
	}
	if err := os.WriteFile(credentials, []byte("[127.0.0.1:3306]\npassword = secret\n"), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	t.Setenv("HOME", home)
	t.Setenv("BEADS_CREDENTIALS_FILE", "")

	lookup := gateSandboxEnvLookup(t, cityPath)

	if got := lookup["HOME"]; got != cityPath {
		t.Errorf("HOME = %q, want the city path %q (the probe must reproduce the gate sandbox)", got, cityPath)
	}
	if got := lookup["BEADS_CREDENTIALS_FILE"]; got != credentials {
		t.Errorf("BEADS_CREDENTIALS_FILE = %q, want %q", got, credentials)
	}
	// A diagnostic must never start a Dolt server as a side effect.
	if got := lookup["BEADS_DOLT_AUTO_START"]; got != "0" {
		t.Errorf("BEADS_DOLT_AUTO_START = %q, want %q (the check must not mutate)", got, "0")
	}
}

// TestGateSandboxEnvOverridesAmbientDoltAutoStart pins the one deliberate
// divergence from the gate contract: a gate inherits the controller's
// auto-start setting, but a diagnostic must never start a Dolt server, and the
// override must replace the inherited value rather than sit beside it.
func TestGateSandboxEnvOverridesAmbientDoltAutoStart(t *testing.T) {
	t.Setenv("BEADS_DOLT_AUTO_START", "1")

	occurrences := 0
	for _, entry := range gateSandboxEnv(t.TempDir()) {
		if strings.HasPrefix(entry, "BEADS_DOLT_AUTO_START=") {
			occurrences++
			if entry != "BEADS_DOLT_AUTO_START=0" {
				t.Errorf("BEADS_DOLT_AUTO_START entry = %q, want %q", entry, "BEADS_DOLT_AUTO_START=0")
			}
		}
	}
	if occurrences != 1 {
		t.Fatalf("BEADS_DOLT_AUTO_START appears %d times, want exactly 1", occurrences)
	}
}

func TestGateSandboxReadCheckOK(t *testing.T) {
	check := &gateSandboxReadCheck{
		cityPath: t.TempDir(),
		probe:    func(string, []string) error { return nil },
	}

	res := check.Run(&doctor.CheckContext{CityPath: check.cityPath})
	if res.Status != doctor.StatusOK {
		t.Fatalf("status = %v, want OK (message %q)", res.Status, res.Message)
	}
	if check.CanFix() {
		t.Error("CanFix() = true, want false (the remedy is controller-side env threading)")
	}
	if check.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false (the probe forks bd)")
	}
}

// TestGateSandboxReadCheckReportsBlindGate is the operator-facing half of
// ga-pqlgh: when a bd read works for the controller but not inside the gate
// sandbox, doctor must say so and name the remedy.
func TestGateSandboxReadCheckReportsBlindGate(t *testing.T) {
	check := &gateSandboxReadCheck{
		cityPath: t.TempDir(),
		probe: func(string, []string) error {
			return errors.New("native_store_unavailable: no credentials reached the store")
		},
	}

	res := check.Run(&doctor.CheckContext{CityPath: check.cityPath})
	if res.Status != doctor.StatusError {
		t.Fatalf("status = %v, want Error", res.Status)
	}
	if res.Severity != doctor.SeverityAdvisory {
		t.Errorf("severity = %v, want advisory (a forked probe must not gate dispatch)", res.Severity)
	}
	if !strings.Contains(res.Message, "native_store_unavailable") {
		t.Errorf("message = %q, want it to carry the probe failure", res.Message)
	}
	if !strings.Contains(res.FixHint, "BEADS_CREDENTIALS_FILE") {
		t.Errorf("fix hint = %q, want it to name the credentials env var threaded into the gate env", res.FixHint)
	}
}

func TestBuildDoctorChecksRegistersGateSandboxReadCheck(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	old := doctorBeadStorePreflight
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error { return nil }
	t.Cleanup(func() { doctorBeadStorePreflight = old })

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	}))
	if doctorCheckIndex(names, "gate-sandbox-reads") < 0 {
		t.Fatalf("gate-sandbox-reads check missing: %v", names)
	}
}

// TestBuildDoctorChecksOmitsGateSandboxReadCheckOnStoreOutage keeps the control
// intact: the probe only distinguishes a blind sandbox from a dead store
// because the store preflight already proved the store reachable with the
// controller's own environment. On an outage there is nothing to differentiate,
// so the check must be omitted with the other store-dependent checks.
func TestBuildDoctorChecksOmitsGateSandboxReadCheckOnStoreOutage(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	old := doctorBeadStorePreflight
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error {
		return errors.New("dolt circuit breaker is open: server appears down, failing fast")
	}
	t.Cleanup(func() { doctorBeadStorePreflight = old })

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	}))
	if doctorCheckIndex(names, "gate-sandbox-reads") >= 0 {
		t.Fatalf("gate-sandbox-reads registered during a store outage: %v", names)
	}
}
