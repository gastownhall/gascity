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

	gate := newDoctorStoreGate(false, nil)
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

// The store gate's decision matrix: city running or stopped, the scope's idle
// policy never or finite, its proxy live or absent, and its rig suspended. A
// finite scope whose proxy retired on its idle timeout under a running city is
// read (waking it for one more idle period), not warned about; a stopped city
// is never started; a suspended scope is never woken.
func TestDoctorStoreGateMatrix(t *testing.T) {
	oldLive, oldScope, oldIdle := doctorProxiedStoreNotRunning, doctorProxiedStoreScope, doctorProxiedStoreRetiresWhenIdle
	t.Cleanup(func() {
		doctorProxiedStoreNotRunning, doctorProxiedStoreScope, doctorProxiedStoreRetiresWhenIdle = oldLive, oldScope, oldIdle
	})
	type want struct {
		state  doctorScopeState
		status doctor.CheckStatus
		msg    string
	}
	for _, tc := range []struct {
		name                       string
		running, finite, live, sus bool
		want                       want
	}{
		{name: "running/finite/live", running: true, finite: true, live: true, want: want{state: doctorScopeReadable}},
		{name: "running/finite/retired wakes", running: true, finite: true, want: want{state: doctorScopeReadable}},
		{name: "running/never/dead warns", running: true, want: want{doctorScopeStopped, doctor.StatusWarning, doctor.StoreNotRunningMessage}},
		{name: "running/never/live", running: true, live: true, want: want{state: doctorScopeReadable}},
		{name: "stopped/finite/absent never starts", finite: true, want: want{doctorScopeStopped, doctor.StatusOK, doctor.StoreNotRunningMessage}},
		{name: "stopped/never/absent", want: want{doctorScopeStopped, doctor.StatusOK, doctor.StoreNotRunningMessage}},
		{name: "stopped/live", live: true, want: want{state: doctorScopeReadable}},
		{name: "running/suspended/finite/absent", running: true, finite: true, sus: true, want: want{doctorScopeSuspended, doctor.StatusOK, doctor.StoreSuspendedMessage}},
		{name: "running/suspended/live", running: true, live: true, sus: true, want: want{doctorScopeSuspended, doctor.StatusOK, doctor.StoreSuspendedMessage}},
		{name: "stopped/suspended", sus: true, want: want{doctorScopeSuspended, doctor.StatusOK, doctor.StoreSuspendedMessage}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doctorProxiedStoreScope = func(string) bool { return true }
			doctorProxiedStoreNotRunning = func(string) bool { return !tc.live }
			doctorProxiedStoreRetiresWhenIdle = func(string) bool { return tc.finite }
			gate := newDoctorStoreGate(tc.running, func(string) bool { return tc.sus })
			scope := filepath.Join(t.TempDir(), "r1")
			if got := gate.State(scope); got != tc.want.state {
				t.Fatalf("State = %v, want %v", got, tc.want.state)
			}
			realCheck := doctor.ErrorCheck("rig:r1:beads", "the real check")
			c := gate.Check(realCheck, []string{scope}, []string{"r1"})
			if tc.want.state == doctorScopeReadable {
				if c != realCheck {
					t.Fatalf("readable scope got a stand-in: %T", c)
				}
				return
			}
			r := c.Run(&doctor.CheckContext{})
			if r.Status != tc.want.status || !strings.Contains(r.Message, tc.want.msg) {
				t.Fatalf("stand-in = %v %q, want %v %q", r.Status, r.Message, tc.want.status, tc.want.msg)
			}
			opened := false
			if _, err := gate.StoreFactory(func(string) (beads.Store, error) { opened = true; return nil, nil })(scope); err == nil || opened {
				t.Fatalf("store factory opened a skipped scope (err=%v)", err)
			}
		})
	}
}

// A suspended city under a running controller: every check that reads the
// city's proxied store reports "not checked: suspended" with OK status, and
// the store is never opened. (Suspended rigs are already left out of doctor's
// per-rig checks entirely.)
func TestBuildDoctorChecksLeavesASuspendedCityCold(t *testing.T) {
	cityDir, cfg := doctorStoreGateCity(t)
	stubDoctorStoreLiveness(t)
	oldScope := doctorProxiedStoreScope
	t.Cleanup(func() { doctorProxiedStoreScope = oldScope })
	doctorProxiedStoreScope = func(string) bool { return true }
	t.Setenv("GC_SUSPENDED", "1")
	doctorBeadStorePreflight = func(string, func(string) (beads.Store, error)) error {
		t.Error("bead-store preflight ran against a suspended city")
		return nil
	}

	checks := buildDoctorChecks(cityDir, cfg, nil, buildDoctorChecksOpts{ControllerRunning: true, SkipCityDoltCheck: true, SkipManagedDoltCheck: true, SkipRigDoltChecks: true})
	for _, name := range []string{"agent-sessions", "beads-store", "custom-types:city", "hold-label-conventions:city"} {
		r := runDoctorCheckNamed(t, checks, name)
		if r.Status != doctor.StatusOK || !strings.Contains(r.Message, doctor.StoreSuspendedMessage) {
			t.Errorf("%s = %v %q, want %q", name, r.Status, r.Message, doctor.StoreSuspendedMessage)
		}
	}
}
