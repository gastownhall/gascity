package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

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
	// A passing probe has no cause to attribute, so it reports the four
	// whitelisted values and nothing else; the controller-only key list belongs
	// to the blind branch (see gateSandboxBlindDetails) and would be noise in
	// every healthy report.
	if strings.Contains(strings.Join(res.Details, "\n"), "not threaded into the gate env") {
		t.Errorf("details = %v, want no blind-result attribution on a passing probe", res.Details)
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
	// The blind branch must use the attributing details, not the plain four:
	// those four are only part of the controller/gate env delta.
	if !strings.Contains(strings.Join(res.Details, "\n"), "not threaded into the gate env: ") {
		t.Errorf("details = %v, want the controller-only keys named as candidate causes", res.Details)
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

// TestBuildDoctorChecksOmitsGateSandboxReadCheckOnNonOutagePreflightFailure
// covers the other way the control can be absent. isBeadStoreUnreachable
// deliberately matches only live outages, so a missing or uninitialized store
// leaves storeOK true with the controller's own read still failing. Registering
// the probe there would print "the same read succeeded for the controller" when
// it did not, sending the operator after a sandbox problem that is really a
// store-shape problem.
func TestBuildDoctorChecksOmitsGateSandboxReadCheckOnNonOutagePreflightFailure(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	probeErr := errors.New("no bead store found: .beads is not initialized")
	if isBeadStoreUnreachable(probeErr) {
		t.Fatalf("premise broken: %v is now outage-shaped, pick another non-outage error", probeErr)
	}

	old := doctorBeadStorePreflight
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error { return probeErr }
	t.Cleanup(func() { doctorBeadStorePreflight = old })

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	}))
	// The control is gone but the store is not declared down, so the other
	// store-dependent checks stay registered; only the differential is dropped.
	if doctorCheckIndex(names, "beads-store") < 0 {
		t.Fatalf("store checks omitted on a non-outage preflight failure: %v", names)
	}
	if doctorCheckIndex(names, "gate-sandbox-reads") >= 0 {
		t.Fatalf("gate-sandbox-reads registered without a passing control: %v", names)
	}
}

// TestBuildDoctorChecksOmitsGateSandboxReadCheckWhenPreflightSkipped: the
// `gc start` warmup path skips the probe entirely, so there is no control at
// all. The check is WarmupEligible() == false and would be filtered out of
// warmup output anyway; this pins that it is never registered rather than
// relying on the downstream filter.
func TestBuildDoctorChecksOmitsGateSandboxReadCheckWhenPreflightSkipped(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_DOLT", "skip")
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}

	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
		SkipStorePreflight:   true,
	}))
	if doctorCheckIndex(names, "gate-sandbox-reads") >= 0 {
		t.Fatalf("gate-sandbox-reads registered with the preflight skipped: %v", names)
	}
}

// TestGateSandboxRunnerIsTheExactEnvRunner pins the binding the whole check
// rests on. Every other test here injects probe, so swapping the layered
// ExecCommandRunnerWithEnvContext back in would hand the probe the controller's
// own environment — reporting a healthy sandbox while gates stay blind — with
// the suite still green. Mirrors TestHookClaimRunnerIsTheExactEnvRunner.
func TestGateSandboxRunnerIsTheExactEnvRunner(t *testing.T) {
	got := reflect.ValueOf(gateSandboxCommandRunnerWithEnvContext).Pointer()
	want := reflect.ValueOf(beads.ExecCommandRunnerWithExactEnvContext).Pointer()
	if got != want {
		t.Fatal("gateSandboxCommandRunnerWithEnvContext must be beads.ExecCommandRunnerWithExactEnvContext; " +
			"the layering runners merge the controller's own process env onto the probe, which is the exact asymmetry this check exists to detect (ga-pqlgh)")
	}
}

func TestDoctorClipGateOutput(t *testing.T) {
	t.Parallel()

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		if got := doctorClipGateOutput(""); got != "" {
			t.Fatalf("clip(%q) = %q, want empty", "", got)
		}
	})

	t.Run("short output collapses whitespace and is not clipped", func(t *testing.T) {
		t.Parallel()
		got := doctorClipGateOutput("bd list failed:\n  Error 1045\t(28000)\n")
		want := "bd list failed: Error 1045 (28000)"
		if got != want {
			t.Fatalf("clip = %q, want %q", got, want)
		}
	})

	t.Run("long ascii output is clipped with an ellipsis", func(t *testing.T) {
		t.Parallel()
		got := doctorClipGateOutput(strings.Repeat("a", 500))
		if want := strings.Repeat("a", 300) + "…"; got != want {
			t.Fatalf("clip len = %d, want the 300-byte prefix plus an ellipsis", len(got))
		}
	})

	t.Run("a multibyte rune across the cut is not split", func(t *testing.T) {
		t.Parallel()
		// 299 ASCII bytes, then a 3-byte rune straddling byte 300.
		got := doctorClipGateOutput(strings.Repeat("a", 299) + "世" + strings.Repeat("b", 50))
		if !utf8.ValidString(got) {
			t.Fatalf("clip produced invalid UTF-8: %q", got)
		}
		if want := strings.Repeat("a", 299) + "…"; got != want {
			t.Fatalf("clip = %q, want the straddling rune dropped entirely", got)
		}
	})
}

// TestGateSandboxBlindDetailsNameTheControllerOnlyKeys is the attribution half
// of the check: the four HOME-derived values are not the whole delta between
// the controller's read and the gate's, so a blind result must also name the
// keys the gate env structurally cannot carry.
func TestGateSandboxBlindDetailsNameTheControllerOnlyKeys(t *testing.T) {
	env := gateSandboxEnv(t.TempDir())
	details := gateSandboxBlindDetails(env)

	joined := strings.Join(details, "\n")
	for _, want := range []string{"HOME=", "GC_HOME=", "BEADS_DIR=", "BEADS_CREDENTIALS_FILE="} {
		if !strings.Contains(joined, want) {
			t.Errorf("details %v missing %q", details, want)
		}
	}
	if !strings.Contains(joined, "not threaded into the gate env: ") {
		t.Fatalf("details %v do not name the controller-only keys", details)
	}
	// BD_BIN and the hosted credential passthrough are set by
	// bdRuntimeEnvWithErrorRecoveryContext and never by ConditionEnv.Environ.
	for _, want := range append([]string{"BD_BIN"}, hostedBeadsCredentialPassthroughKeys...) {
		if !strings.Contains(joined, want) {
			t.Errorf("details %v missing controller-only key %q", details, want)
		}
	}

	// The healthy branch stays quiet: a passing probe has no cause to
	// attribute, and nine absent key names would be noise in every report.
	if healthy := strings.Join(gateSandboxDetails(env), "\n"); strings.Contains(healthy, "not threaded into the gate env") {
		t.Errorf("healthy details %q must not carry the blind-result attribution line", healthy)
	}
}
