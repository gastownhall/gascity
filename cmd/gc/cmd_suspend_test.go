package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citystate"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/rigstate"
)

// --- doSuspendCity ---

// TestSuspendResume exercises the canonical suspend → resume cycle.
// Suspension state is recorded in .gc/runtime/city-state.json and
// city.toml stays untouched.
func TestSuspendResume(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	cityPath := "/city"
	cityTOMLPath := filepath.Join(cityPath, "city.toml")
	f.Files[cityTOMLPath] = data
	originalTOML := append([]byte(nil), data...)

	// Suspend.
	var stdout, stderr bytes.Buffer
	code := doSuspendCity(f, cityPath, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("suspend code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "City suspended") {
		t.Errorf("stdout = %q, want suspend message", stdout.String())
	}

	// city.toml must stay byte-for-byte identical: suspension lives in
	// .gc/runtime/city-state.json, never in committed config.
	if !bytes.Equal(f.Files[cityTOMLPath], originalTOML) {
		t.Errorf("city.toml mutated by suspend; want byte-identical:\n got:  %s\n want: %s",
			f.Files[cityTOMLPath], originalTOML)
	}
	st, err := citystate.Load(f, cityPath)
	if err != nil {
		t.Fatalf("citystate.Load: %v", err)
	}
	if !citystate.IsSuspended(st) {
		t.Error("citystate should record explicit suspend after doSuspendCity(true)")
	}

	// Resume.
	stdout.Reset()
	stderr.Reset()
	code = doSuspendCity(f, cityPath, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resume code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "City resumed") {
		t.Errorf("stdout = %q, want resume message", stdout.String())
	}
	if !bytes.Equal(f.Files[cityTOMLPath], originalTOML) {
		t.Errorf("city.toml mutated by resume; want byte-identical:\n got:  %s\n want: %s",
			f.Files[cityTOMLPath], originalTOML)
	}
	st, err = citystate.Load(f, cityPath)
	if err != nil {
		t.Fatalf("citystate.Load: %v", err)
	}
	if v, ok := citystate.ExplicitSuspended(st); !ok || v {
		t.Errorf("citystate should record explicit resume after doSuspendCity(false); got (%v, %v)", v, ok)
	}
}

// TestSuspendAlreadySuspended pins the idempotency contract: calling
// suspend twice succeeds and leaves the runtime state alone.
func TestSuspendAlreadySuspended(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.City{
		Workspace: config.Workspace{Name: "bright-lights"},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data
	want := true
	if err := citystate.SetCitySuspended(f, "/city", &want); err != nil {
		t.Fatalf("pre-suspend: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doSuspendCity(f, "/city", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("suspend code = %d, want 0 (idempotent)", code)
	}
}

// TestResumeAlreadyResumed pins resume idempotency: calling resume on
// a city with no recorded state succeeds.
func TestResumeAlreadyResumed(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stdout, stderr bytes.Buffer
	code := doSuspendCity(f, "/city", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resume code = %d, want 0 (idempotent)", code)
	}
}

// --- Pack preservation: suspend/resume must not touch city.toml ---

// TestDoSuspendCityPreservesConfig pins the invariant that suspending
// the city never modifies city.toml — so include directives and
// other committable content can never get expanded or churned by a
// transient runtime-state change.
func TestDoSuspendCityPreservesConfig(t *testing.T) {
	f := fsys.NewFake()
	original := []byte(`include = ["packs/mypack/agents.toml"]

[workspace]
name = "test-city"

[[agent]]
name = "inline-agent"
`)
	f.Files["/city/city.toml"] = append([]byte(nil), original...)
	f.Files["/city/packs/mypack/agents.toml"] = []byte(`[[agent]]
name = "pack-worker"
dir = "myrig"
`)

	var stdout, stderr bytes.Buffer
	code := doSuspendCity(f, "/city", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("suspend code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !bytes.Equal(f.Files["/city/city.toml"], original) {
		t.Errorf("city.toml mutated by suspend:\n got:  %s\n want: %s",
			f.Files["/city/city.toml"], original)
	}
	st, err := citystate.Load(f, "/city")
	if err != nil {
		t.Fatalf("citystate.Load: %v", err)
	}
	if !citystate.IsSuspended(st) {
		t.Error("citystate should record explicit suspend")
	}

	// Resume should also preserve.
	stdout.Reset()
	stderr.Reset()
	code = doSuspendCity(f, "/city", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resume code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !bytes.Equal(f.Files["/city/city.toml"], original) {
		t.Errorf("city.toml mutated by resume:\n got:  %s\n want: %s",
			f.Files["/city/city.toml"], original)
	}
}

// --- citySuspended ---

// TestCitySuspendedFromConfig confirms workspace.suspended_on_start
// flows through citySuspendedWithState when no runtime override is
// present.
func TestCitySuspendedFromConfig(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", SuspendedOnStart: true},
	}
	if !citySuspendedWithState(cfg, citystate.State{}) {
		t.Error("citySuspendedWithState = false, want true with workspace.suspended_on_start=true")
	}
	cfg.Workspace.SuspendedOnStart = false
	if citySuspendedWithState(cfg, citystate.State{}) {
		t.Error("citySuspendedWithState = true, want false when nothing flags the city as suspended")
	}
}

// TestCitySuspendedRuntimeOverridesConfig pins the merge precedence:
// an explicit runtime resume must beat suspended_on_start=true.
func TestCitySuspendedRuntimeOverridesConfig(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", SuspendedOnStart: true},
	}
	resume := false
	st := citystate.State{City: citystate.Override{Suspended: &resume}}
	if citySuspendedWithState(cfg, st) {
		t.Error("explicit runtime resume must beat workspace.suspended_on_start=true")
	}

	suspend := true
	cfg.Workspace.SuspendedOnStart = false
	st = citystate.State{City: citystate.Override{Suspended: &suspend}}
	if !citySuspendedWithState(cfg, st) {
		t.Error("explicit runtime suspend must beat workspace.suspended_on_start=false")
	}
}

// TestCitySuspended_LegacyFieldIgnored pins the migration contract:
// the deprecated workspace.suspended field is never read for behavior.
// Doctor surfaces it as a warning so users migrate.
func TestCitySuspended_LegacyFieldIgnored(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Suspended: true},
	}
	if citySuspendedWithState(cfg, citystate.State{}) {
		t.Error("legacy [workspace] suspended = true must be ignored; only suspended_on_start and runtime state matter")
	}
}

// TestCitySuspendedEnvOverride verifies GC_SUSPENDED=1 still forces
// city-level suspension regardless of config or runtime state.
func TestCitySuspendedEnvOverride(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
	}
	t.Setenv("GC_SUSPENDED", "1")
	if !citySuspended(cfg) {
		t.Error("citySuspended = false, want true when GC_SUSPENDED=1")
	}
}

// --- isAgentEffectivelySuspended ---

func TestAgentEffectivelySuspendedDirect(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "worker", Suspended: true}},
	}
	if !isAgentEffectivelySuspendedWith(cfg, &cfg.Agents[0], citystate.State{}, emptyRigState()) {
		t.Error("agent with Suspended=true should be effectively suspended")
	}
}

func TestAgentEffectivelySuspendedViaRig(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "polecat", Dir: "myrig"}},
		Rigs:      []config.Rig{{Name: "myrig", Path: "/tmp/myrig", SuspendedOnStart: true}},
	}
	if !isAgentEffectivelySuspendedWith(cfg, &cfg.Agents[0], citystate.State{}, emptyRigState()) {
		t.Error("agent in rig with suspended_on_start=true should be effectively suspended")
	}
}

func TestAgentEffectivelySuspendedViaCity(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", SuspendedOnStart: true},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	if !isAgentEffectivelySuspendedWith(cfg, &cfg.Agents[0], citystate.State{}, emptyRigState()) {
		t.Error("agent in city with suspended_on_start=true should be effectively suspended")
	}
}

func TestAgentEffectivelySuspendedNot(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	if isAgentEffectivelySuspendedWith(cfg, &cfg.Agents[0], citystate.State{}, emptyRigState()) {
		t.Error("non-suspended agent should not be effectively suspended")
	}
}

// --- Inheritance: city suspend affects all three levels ---

func TestSuspendInheritance(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", SuspendedOnStart: true},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)}, // city-scoped
			{Name: "polecat", Dir: "myrig"},               // rig-scoped
			{Name: "builder", Suspended: true},            // individually suspended too
		},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/tmp/myrig"},
		},
	}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if !isAgentEffectivelySuspendedWith(cfg, a, citystate.State{}, emptyRigState()) {
			t.Errorf("agent %q should be suspended when city has suspended_on_start=true", a.QualifiedName())
		}
	}
}

// emptyRigState returns a fresh rigstate.SuspensionState for tests.
func emptyRigState() rigstate.SuspensionState { return rigstate.SuspensionState{} }
