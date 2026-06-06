package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// --- doSuspendCity ---

// TestSuspendResume exercises the canonical suspend → resume cycle.
// Suspension state is recorded in .gc/runtime/suspension-state.json
// and city.toml stays untouched.
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
	code := doSuspendCity(f, cityPath, true, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("suspend code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "City suspended") {
		t.Errorf("stdout = %q, want suspend message", stdout.String())
	}

	// city.toml must stay byte-for-byte identical: suspension lives in
	// .gc/runtime/suspension-state.json, never in committed config.
	if !bytes.Equal(f.Files[cityTOMLPath], originalTOML) {
		t.Errorf("city.toml mutated by suspend; want byte-identical:\n got:  %s\n want: %s",
			f.Files[cityTOMLPath], originalTOML)
	}
	st, err := suspensionstate.Load(f, cityPath)
	if err != nil {
		t.Fatalf("suspensionstate.Load: %v", err)
	}
	if !suspensionstate.IsCitySuspended(st) {
		t.Error("runtime state should record explicit suspend after doSuspendCity(true)")
	}

	// Resume.
	stdout.Reset()
	stderr.Reset()
	code = doSuspendCity(f, cityPath, false, false, &stdout, &stderr)
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
	st, err = suspensionstate.Load(f, cityPath)
	if err != nil {
		t.Fatalf("suspensionstate.Load: %v", err)
	}
	if v, ok := suspensionstate.ExplicitCity(st); !ok || v {
		t.Errorf("runtime state should record explicit resume after doSuspendCity(false); got (%v, %v)", v, ok)
	}
}

// TestSuspendJSON pins the JSON-output contract for `gc suspend --json`:
// suspending a city writes a structured lifecycleActionJSON envelope to
// stdout and nothing to stderr.
func TestSuspendJSON(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	cityPath := "/city"
	f.Files[filepath.Join(cityPath, "city.toml")] = data

	var stdout, stderr bytes.Buffer
	code := doSuspendCity(f, cityPath, true, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("suspend code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	var got lifecycleActionJSON
	if err := json.Unmarshal(stdout.Bytes(), &got); err != nil {
		t.Fatalf("stdout is not JSON: %v\n%s", err, stdout.String())
	}
	if got.SchemaVersion != "1" || !got.OK || got.Command != "suspend" || got.CityPath != cityPath {
		t.Fatalf("payload = %+v", got)
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
	if err := suspensionstate.SetCitySuspended(f, "/city", &want); err != nil {
		t.Fatalf("pre-suspend: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doSuspendCity(f, "/city", true, false, &stdout, &stderr)
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
	code := doSuspendCity(f, "/city", false, false, &stdout, &stderr)
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
	code := doSuspendCity(f, "/city", true, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("suspend code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !bytes.Equal(f.Files["/city/city.toml"], original) {
		t.Errorf("city.toml mutated by suspend:\n got:  %s\n want: %s",
			f.Files["/city/city.toml"], original)
	}
	st, err := suspensionstate.Load(f, "/city")
	if err != nil {
		t.Fatalf("suspensionstate.Load: %v", err)
	}
	if !suspensionstate.IsCitySuspended(st) {
		t.Error("runtime state should record explicit suspend")
	}

	// Resume should also preserve.
	stdout.Reset()
	stderr.Reset()
	code = doSuspendCity(f, "/city", false, false, &stdout, &stderr)
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
	if !citySuspendedWithState(cfg, suspensionstate.State{}) {
		t.Error("citySuspendedWithState = false, want true with workspace.suspended_on_start=true")
	}
	cfg.Workspace.SuspendedOnStart = false
	if citySuspendedWithState(cfg, suspensionstate.State{}) {
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
	st := suspensionstate.State{City: suspensionstate.Override{Suspended: &resume}}
	if citySuspendedWithState(cfg, st) {
		t.Error("explicit runtime resume must beat workspace.suspended_on_start=true")
	}

	suspend := true
	cfg.Workspace.SuspendedOnStart = false
	st = suspensionstate.State{City: suspensionstate.Override{Suspended: &suspend}}
	if !citySuspendedWithState(cfg, st) {
		t.Error("explicit runtime suspend must beat workspace.suspended_on_start=false")
	}
}

// TestCitySuspended_LegacyFieldIsAlias pins the migration contract:
// the deprecated workspace.suspended field is honored as an alias for
// suspended_on_start so existing cities with `suspended = true`
// continue to start suspended after upgrade. Doctor warns and offers
// `--fix` to rename.
func TestCitySuspended_LegacyFieldIsAlias(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Suspended: true},
	}
	if !citySuspendedWithState(cfg, suspensionstate.State{}) {
		t.Error("legacy [workspace] suspended = true must keep starting the city suspended after upgrade (alias for suspended_on_start)")
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
	if !isAgentEffectivelySuspendedWith(cfg, &cfg.Agents[0], suspensionstate.State{}) {
		t.Error("agent with Suspended=true should be effectively suspended")
	}
}

func TestAgentEffectivelySuspendedViaRig(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "polecat", Dir: "myrig"}},
		Rigs:      []config.Rig{{Name: "myrig", Path: "/tmp/myrig", SuspendedOnStart: true}},
	}
	if !isAgentEffectivelySuspendedWith(cfg, &cfg.Agents[0], suspensionstate.State{}) {
		t.Error("agent in rig with suspended_on_start=true should be effectively suspended")
	}
}

func TestAgentEffectivelySuspendedViaCity(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", SuspendedOnStart: true},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	if !isAgentEffectivelySuspendedWith(cfg, &cfg.Agents[0], suspensionstate.State{}) {
		t.Error("agent in city with suspended_on_start=true should be effectively suspended")
	}
}

func TestAgentEffectivelySuspendedNot(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	if isAgentEffectivelySuspendedWith(cfg, &cfg.Agents[0], suspensionstate.State{}) {
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
		if !isAgentEffectivelySuspendedWith(cfg, a, suspensionstate.State{}) {
			t.Errorf("agent %q should be suspended when city has suspended_on_start=true", a.QualifiedName())
		}
	}
}
