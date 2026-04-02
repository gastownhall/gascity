package main

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// --- doSuspendCity ---

func TestSuspendResume(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.DefaultCity("bright-lights")
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	cityPath := "/city"
	f.Files[filepath.Join(cityPath, "city.toml")] = data

	// Suspend.
	var stdout, stderr bytes.Buffer
	code := doSuspendCity(f, cityPath, true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("suspend code = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "City suspended") {
		t.Errorf("stdout = %q, want suspend message", stdout.String())
	}

	// Verify config was updated.
	written := f.Files[filepath.Join(cityPath, "city.toml")]
	got, err := config.Parse(written)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if !got.Workspace.Suspended {
		t.Error("Workspace.Suspended = false after suspend, want true")
	}
	if !strings.Contains(string(written), "suspended = true") {
		t.Errorf("written TOML missing 'suspended = true':\n%s", written)
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

	// Verify config was updated (suspended field dropped via omitempty).
	written = f.Files[filepath.Join(cityPath, "city.toml")]
	got, err = config.Parse(written)
	if err != nil {
		t.Fatalf("parsing written config: %v", err)
	}
	if got.Workspace.Suspended {
		t.Error("Workspace.Suspended = true after resume, want false")
	}
	if strings.Contains(string(written), "suspended") {
		t.Errorf("written TOML should omit 'suspended' when false:\n%s", written)
	}
}

func TestSuspendAlreadySuspended(t *testing.T) {
	f := fsys.NewFake()
	cfg := config.City{
		Workspace: config.Workspace{Name: "bright-lights", Suspended: true},
		Agents:    []config.Agent{{Name: "mayor", MaxActiveSessions: intPtr(1)}},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	f.Files[filepath.Join("/city", "city.toml")] = data

	var stdout, stderr bytes.Buffer
	code := doSuspendCity(f, "/city", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("suspend code = %d, want 0 (idempotent)", code)
	}
}

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

// --- Pack preservation: suspend/resume must not expand includes ---

func TestDoSuspendCityPreservesConfig(t *testing.T) {
	f := fsys.NewFake()
	f.Files["/city/city.toml"] = []byte(`include = ["packs/mypack/agents.toml"]

[workspace]
name = "test-city"

[[agent]]
name = "inline-agent"
`)
	f.Files["/city/packs/mypack/agents.toml"] = []byte(`[[agent]]
name = "pack-worker"
dir = "myrig"
`)

	var stdout, stderr bytes.Buffer
	code := doSuspendCity(f, "/city", true, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("suspend code = %d, want 0; stderr: %s", code, stderr.String())
	}

	data := string(f.Files["/city/city.toml"])
	if !strings.Contains(data, "packs/mypack/agents.toml") {
		t.Errorf("city.toml should preserve include directive:\n%s", data)
	}
	if strings.Contains(data, "pack-worker") {
		t.Errorf("city.toml should NOT contain expanded pack agent:\n%s", data)
	}
	if !strings.Contains(data, "suspended = true") {
		t.Errorf("city.toml should contain suspended = true:\n%s", data)
	}

	// Resume should also preserve.
	stdout.Reset()
	stderr.Reset()
	code = doSuspendCity(f, "/city", false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("resume code = %d, want 0; stderr: %s", code, stderr.String())
	}
	data = string(f.Files["/city/city.toml"])
	if !strings.Contains(data, "packs/mypack/agents.toml") {
		t.Errorf("city.toml should preserve include after resume:\n%s", data)
	}
	if strings.Contains(data, "pack-worker") {
		t.Errorf("city.toml should NOT contain pack agent after resume:\n%s", data)
	}
}

// --- citySuspended ---

func TestCitySuspendedFromConfig(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Suspended: true},
	}
	if !citySuspended(cfg) {
		t.Error("citySuspended = false, want true when config is suspended")
	}

	cfg.Workspace.Suspended = false
	if citySuspended(cfg) {
		t.Error("citySuspended = true, want false when config is not suspended")
	}
}

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
	if !isAgentEffectivelySuspended(cfg, &cfg.Agents[0]) {
		t.Error("agent with Suspended=true should be effectively suspended")
	}
}

func TestAgentEffectivelySuspendedViaRig(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "polecat", Dir: "myrig"}},
		Rigs:      []config.Rig{{Name: "myrig", Path: "/tmp/myrig", Suspended: true}},
	}
	if !isAgentEffectivelySuspended(cfg, &cfg.Agents[0]) {
		t.Error("agent in suspended rig should be effectively suspended")
	}
}

func TestAgentEffectivelySuspendedViaCity(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Suspended: true},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	if !isAgentEffectivelySuspended(cfg, &cfg.Agents[0]) {
		t.Error("agent in suspended city should be effectively suspended")
	}
}

func TestAgentEffectivelySuspendedNot(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	if isAgentEffectivelySuspended(cfg, &cfg.Agents[0]) {
		t.Error("non-suspended agent should not be effectively suspended")
	}
}

// --- Inheritance: city suspend affects all three levels ---

func TestSuspendInheritance(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test", Suspended: true},
		Agents: []config.Agent{
			{Name: "mayor", MaxActiveSessions: intPtr(1)}, // city-scoped
			{Name: "polecat", Dir: "myrig"},               // rig-scoped
			{Name: "builder", Suspended: true},            // individually suspended too
		},
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/tmp/myrig"},
		},
	}
	for _, a := range cfg.Agents {
		if !isAgentEffectivelySuspended(cfg, &a) {
			t.Errorf("agent %q should be suspended when city is suspended", a.QualifiedName())
		}
	}
}
