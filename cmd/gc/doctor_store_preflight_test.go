package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// Multi-scope + city store-dependent checks gated by preflight. Keep in sync
// with the register sites in buildDoctorChecks / doctorCityStoreCheckCount.
var doctorCityStoreDependentNames = []string{
	"beads-store",
	"v2-routed-to-namespace",
	"census-owner-liveness",
	"run-target-routed-to-backfill",
	"route-recovery-quarantine",
	"hold-label-routed-to",
	"pool-idle-routed-work",
	"work-option-metadata-migration",
	"backlog-depth",
	"order-tracking-retention",
	"session-model",
	"startup-health-episodes",
	"custom-types:city",
	"hold-label-conventions:city",
}

func TestIsBeadStoreUnreachable(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("database not initialized"), false},
		{errors.New("no beads directory"), false},
		{errors.New("dolt circuit breaker is open: server appears down, failing fast"), true},
		{errors.New("max waiting connections reached. Client rejected."), true}, //nolint:revive // error-strings: verbatim upstream pool message; the trailing period is part of the string isBeadStoreUnreachable must match
		{errors.New("dial tcp 127.0.0.1:3307: connect: connection refused"), true},
		{errors.New("dolt server unreachable: bead store preflight timed out after 5s"), true},
		{errors.New("bd list: timed out after 5s"), true},
		{errors.New("context deadline exceeded"), true},
		{errors.New("Error 1040: Too many connections"), true},
	}
	for _, tc := range cases {
		if got := isBeadStoreUnreachable(tc.err); got != tc.want {
			t.Errorf("isBeadStoreUnreachable(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestBuildDoctorChecks_SkipsStoreChecksWhenStoreUnreachable(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")

	old := doctorBeadStorePreflight
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error {
		return fmt.Errorf("dolt circuit breaker is open: server appears down, failing fast")
	}
	t.Cleanup(func() { doctorBeadStorePreflight = old })

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: "alpha", Prefix: "al"},
			{Name: "beta", Path: "beta", Prefix: "be"},
			{Name: "sleeping", Path: "sleeping", Prefix: "sl", Suspended: true},
		},
	}
	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    true,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
		SkipRigDoltChecks:    true,
	})
	names := doctorCheckNames(checks)

	var preflight doctor.Check
	for _, c := range checks {
		if c.Name() == "bead-store-preflight" {
			preflight = c
			break
		}
	}
	if preflight == nil {
		t.Fatalf("bead-store-preflight not registered; names=%v", names)
	}
	res := preflight.Run(&doctor.CheckContext{CityPath: cityDir})
	if res.Status != doctor.StatusError {
		t.Fatalf("preflight status = %v, want StatusError; message=%q", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "bead store unreachable") {
		t.Fatalf("preflight message = %q, want unreachable wording", res.Message)
	}
	if !strings.Contains(res.Message, "city store was probed") {
		t.Fatalf("preflight message = %q, want city-scope disclosure", res.Message)
	}
	if !strings.Contains(res.Message, "doltlite") {
		t.Fatalf("preflight message = %q, want doltlite residual note", res.Message)
	}
	// Fourteen city checks plus three per active rig, two rigs active.
	if !strings.Contains(res.Message, "skipped 20 store checks") {
		t.Fatalf("preflight message = %q, want skip count 20", res.Message)
	}
	if !strings.Contains(res.Message, "2 rigs") {
		t.Fatalf("preflight message = %q, want rig count 2", res.Message)
	}

	for _, name := range doctorCityStoreDependentNames {
		if doctorCheckIndex(names, name) >= 0 {
			t.Errorf("city store check %q registered despite unreachable store", name)
		}
	}
	for _, name := range []string{
		"rig:alpha:beads", "rig:beta:beads",
		"custom-types:alpha", "custom-types:beta",
		"hold-label-conventions:alpha", "hold-label-conventions:beta",
	} {
		if doctorCheckIndex(names, name) >= 0 {
			t.Errorf("per-rig store check %q registered despite unreachable store", name)
		}
	}
	for _, name := range []string{
		"rig:alpha:path", "rig:beta:path",
		"rig:alpha:git", "rig:beta:git",
		"rig:alpha:root-branch", "rig:beta:root-branch",
		"rig:alpha:bd-split-store", "rig:beta:bd-split-store",
		"bd-split-store",
	} {
		if doctorCheckIndex(names, name) < 0 {
			t.Errorf("store-independent check %q missing; names=%v", name, names)
		}
	}
	if doctorCheckIndex(names, "rig:sleeping:path") >= 0 {
		t.Error("suspended rig should still be skipped entirely")
	}
}

func TestBuildDoctorChecks_RegistersStoreChecksWhenStoreReachable(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")

	old := doctorBeadStorePreflight
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error {
		return nil
	}
	t.Cleanup(func() { doctorBeadStorePreflight = old })

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: "alpha", Prefix: "al"},
			{Name: "beta", Path: "beta", Prefix: "be"},
		},
	}
	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    true,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
		SkipRigDoltChecks:    true,
	})
	names := doctorCheckNames(checks)

	if doctorCheckIndex(names, "bead-store-preflight") >= 0 {
		t.Fatalf("bead-store-preflight registered when store reachable; names=%v", names)
	}
	for _, name := range doctorCityStoreDependentNames {
		if doctorCheckIndex(names, name) < 0 {
			t.Errorf("city store check %q missing when store reachable; names=%v", name, names)
		}
	}
	for _, name := range []string{
		"rig:alpha:beads", "rig:beta:beads",
		"custom-types:alpha", "custom-types:beta",
		"hold-label-conventions:alpha", "hold-label-conventions:beta",
		"rig:alpha:path", "rig:beta:path",
	} {
		if doctorCheckIndex(names, name) < 0 {
			t.Errorf("check %q missing when store reachable; names=%v", name, names)
		}
	}
}

func TestBuildDoctorChecks_NonUnreachableProbeKeepsStoreChecks(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")

	old := doctorBeadStorePreflight
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error {
		return errors.New("database not initialized")
	}
	t.Cleanup(func() { doctorBeadStorePreflight = old })

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "alpha", Path: "alpha", Prefix: "al"}},
	}
	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    true,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
		SkipRigDoltChecks:    true,
	}))
	if doctorCheckIndex(names, "bead-store-preflight") >= 0 {
		t.Fatalf("preflight skip registered for non-outage probe error; names=%v", names)
	}
	if doctorCheckIndex(names, "rig:alpha:beads") < 0 {
		t.Fatalf("rig beads check missing after non-outage probe error; names=%v", names)
	}
	if doctorCheckIndex(names, "beads-store") < 0 {
		t.Fatalf("city beads-store missing after non-outage probe error; names=%v", names)
	}
	if doctorCheckIndex(names, "hold-label-routed-to") < 0 {
		t.Fatalf("hold-label-routed-to missing after non-outage probe error; names=%v", names)
	}
}

// Name-set lock for the rig-populated preflight paths. Distinct from the
// zero-rig doctor_check_names.golden (TestBuildDoctorChecks_NameSetUnchanged),
// which does not exercise this gate.
func TestBuildDoctorChecks_RigStoreNameSetPreflight(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "alpha", Path: "alpha", Prefix: "al"},
			{Name: "beta", Path: "beta", Prefix: "be"},
		},
	}
	opts := buildDoctorChecksOpts{
		ControllerRunning:    true,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
		SkipRigDoltChecks:    true,
	}

	old := doctorBeadStorePreflight
	t.Cleanup(func() { doctorBeadStorePreflight = old })

	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error { return nil }
	healthy := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, opts))
	mustHave := append(append([]string{}, doctorCityStoreDependentNames...),
		"rig:alpha:beads", "rig:beta:beads",
		"custom-types:alpha", "custom-types:beta",
		"hold-label-conventions:alpha", "hold-label-conventions:beta",
		"rig:alpha:path", "rig:beta:path",
	)
	for _, name := range mustHave {
		if doctorCheckIndex(healthy, name) < 0 {
			t.Errorf("healthy name-set missing %q; names=%v", name, healthy)
		}
	}
	if doctorCheckIndex(healthy, "bead-store-preflight") >= 0 {
		t.Errorf("healthy name-set must not include bead-store-preflight")
	}

	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error {
		return fmt.Errorf("connection refused")
	}
	outage := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, opts))
	if doctorCheckIndex(outage, "bead-store-preflight") < 0 {
		t.Fatalf("outage name-set missing bead-store-preflight; names=%v", outage)
	}
	for _, name := range mustHave {
		if strings.HasSuffix(name, ":path") {
			if doctorCheckIndex(outage, name) < 0 {
				t.Errorf("outage name-set missing independent %q", name)
			}
			continue
		}
		if doctorCheckIndex(outage, name) >= 0 {
			t.Errorf("outage name-set still has store check %q", name)
		}
	}
	// Constant-drift lock: omit N store checks and add one preflight entry.
	// A silent 12th city store check would make this delta diverge without
	// updating doctorCityStoreCheckCount / beadStorePreflightSkipCount.
	wantDelta := beadStorePreflightSkipCount(2) - 1 // -1 for bead-store-preflight
	if got := len(healthy) - len(outage); got != wantDelta {
		t.Fatalf("healthy-outage name delta = %d, want %d (skipCount-1=%d); healthy=%d outage=%d",
			got, wantDelta, beadStorePreflightSkipCount(2)-1, len(healthy), len(outage))
	}
}

func TestBeadStorePreflightSkipMessage(t *testing.T) {
	t.Parallel()
	got := beadStorePreflightSkipMessage(17, 2, errors.New("connection refused"))
	if !strings.Contains(got, "skipped 17 store checks") {
		t.Fatalf("message = %q, want count 17", got)
	}
	if !strings.Contains(got, "2 rigs") {
		t.Fatalf("message = %q, want 2 rigs", got)
	}
	if !strings.Contains(got, "connection refused") {
		t.Fatalf("message = %q, want probe error", got)
	}
	if !strings.Contains(got, "city store was probed") {
		t.Fatalf("message = %q, want city-scope disclosure", got)
	}
	if !strings.Contains(got, "doltlite") {
		t.Fatalf("message = %q, want doltlite residual note", got)
	}
}

func TestBeadStorePreflightSkipCount(t *testing.T) {
	t.Parallel()
	if got := beadStorePreflightSkipCount(0); got != doctorCityStoreCheckCount {
		t.Fatalf("skip count 0 rigs = %d, want %d", got, doctorCityStoreCheckCount)
	}
	if got := beadStorePreflightSkipCount(2); got != doctorCityStoreCheckCount+2*doctorPerRigStoreCheckCount {
		t.Fatalf("skip count 2 rigs = %d, want %d", got, doctorCityStoreCheckCount+6)
	}
	if len(doctorCityStoreDependentNames) != doctorCityStoreCheckCount {
		t.Fatalf("city name list len %d != doctorCityStoreCheckCount %d", len(doctorCityStoreDependentNames), doctorCityStoreCheckCount)
	}
}

// withHealthyStorePreflight stubs the live bd probe so registration tests
// asserting gated check names do not depend on ambient bd/Dolt state.
func withHealthyStorePreflight(t *testing.T) {
	t.Helper()
	old := doctorBeadStorePreflight
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error { return nil }
	t.Cleanup(func() { doctorBeadStorePreflight = old })
}

func TestBuildDoctorChecks_SkipStorePreflightSkipsProbe(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"demo\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")

	called := false
	old := doctorBeadStorePreflight
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error {
		called = true
		return fmt.Errorf("connection refused")
	}
	t.Cleanup(func() { doctorBeadStorePreflight = old })

	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs:      []config.Rig{{Name: "alpha", Path: "alpha", Prefix: "al"}},
	}
	names := doctorCheckNames(buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{
		ControllerRunning:    true,
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
		SkipRigDoltChecks:    true,
		SkipStorePreflight:   true,
	}))
	if called {
		t.Fatal("preflight probe ran despite SkipStorePreflight")
	}
	if doctorCheckIndex(names, "bead-store-preflight") >= 0 {
		t.Fatalf("bead-store-preflight registered with SkipStorePreflight; names=%v", names)
	}
	if doctorCheckIndex(names, "beads-store") < 0 {
		t.Fatalf("store checks omitted with SkipStorePreflight; names=%v", names)
	}
}
