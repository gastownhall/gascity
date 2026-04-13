package main

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestPrefixedWorkQueryForProbe_UsesNamedSessionRuntimeName(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name: "witness",
			Dir:  "demo",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "witness",
			Dir:      "demo",
		}},
	}

	command := prefixedWorkQueryForProbe(cfg, cityPath, "test-city", nil, nil, &cfg.Agents[0])
	// All agents now use metadata routing via gc.routed_to.
	if !strings.Contains(command, "gc.routed_to=demo/witness") {
		t.Fatalf("prefixedWorkQueryForProbe() = %q, want gc.routed_to=demo/witness", command)
	}
}

// ── controllerQueryEnv tests ────────────────────────────────────────────

func TestControllerQueryEnv_NilInputs(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{}
	agent := &config.Agent{Name: "worker"}

	if got := controllerQueryEnv("", cfg, agent); got != nil {
		t.Errorf("empty cityPath: got %v, want nil", got)
	}
	if got := controllerQueryEnv("  ", cfg, agent); got != nil {
		t.Errorf("blank cityPath: got %v, want nil", got)
	}
	if got := controllerQueryEnv(cityPath, nil, agent); got != nil {
		t.Errorf("nil cfg: got %v, want nil", got)
	}
	if got := controllerQueryEnv(cityPath, cfg, nil); got != nil {
		t.Errorf("nil agentCfg: got %v, want nil", got)
	}
}

func TestControllerQueryEnv_CityAgent_BdProvider(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "city-host.example.com")
	t.Setenv("GC_DOLT_PORT", "3306")

	cityPath := t.TempDir()
	cfg := &config.City{}
	agent := &config.Agent{Name: "worker"}

	env := controllerQueryEnv(cityPath, cfg, agent)
	if env == nil {
		t.Fatal("expected env for city agent with bd provider, got nil")
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "city-host.example.com" {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q", got, "city-host.example.com")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3306" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q", got, "3306")
	}
}

func TestControllerQueryEnv_CityAgent_FileProvider_ReturnsNil(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "city-host.example.com")
	t.Setenv("GC_DOLT_PORT", "3306")

	cityPath := t.TempDir()
	cfg := &config.City{}
	agent := &config.Agent{Name: "worker"}

	env := controllerQueryEnv(cityPath, cfg, agent)
	if env != nil {
		t.Errorf("expected nil for city agent with file provider, got %v", env)
	}
}

// Regression test: rig agents must get rig dolt env even when the city uses
// the file provider. Before the fix, controllerQueryEnv gated on
// rawBeadsProvider(cityPath) == "bd" and returned nil for file-provider cities,
// leaving rig scale_check subprocesses with the wrong (or no) dolt port.
func TestControllerQueryEnv_RigAgent_FileProvider_StillGetsDoltEnv(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "rig-repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close() //nolint:errcheck // test cleanup

	portStr := fmt.Sprintf("%d", ln.Addr().(*net.TCPAddr).Port)
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "dolt-server.port"), []byte(portStr), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{{
			Name: "rig-repo",
			Path: rigDir,
		}},
	}
	agent := &config.Agent{Name: "sonnet", Dir: "rig-repo"}

	env := controllerQueryEnv(cityPath, cfg, agent)
	if env == nil {
		t.Fatal("expected env for rig agent with file city provider, got nil — this was the bug")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != portStr {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q (rig's managed port)", got, portStr)
	}
}

func TestControllerQueryEnv_RigAgent_ExplicitDoltConfig(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "city-host.example.com")
	t.Setenv("GC_DOLT_PORT", "3306")

	cityPath := t.TempDir()
	rigDir := filepath.Join(t.TempDir(), "rig-repo")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Rigs: []config.Rig{{
			Name:     "rig-repo",
			Path:     rigDir,
			DoltHost: "rig-host.example.com",
			DoltPort: "3307",
		}},
	}
	agent := &config.Agent{Name: "sonnet", Dir: "rig-repo"}

	env := controllerQueryEnv(cityPath, cfg, agent)
	if env == nil {
		t.Fatal("expected env for rig agent, got nil")
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "rig-host.example.com" {
		t.Errorf("BEADS_DOLT_SERVER_HOST = %q, want %q (rig's host, not city)", got, "rig-host.example.com")
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "3307" {
		t.Errorf("BEADS_DOLT_SERVER_PORT = %q, want %q (rig's port, not city)", got, "3307")
	}
}

func TestControllerQueryEnv_OmitsCredentials(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_DOLT_HOST", "host.example.com")
	t.Setenv("GC_DOLT_PORT", "3306")
	t.Setenv("GC_DOLT_USER", "agent")
	t.Setenv("GC_DOLT_PASSWORD", "s3cret")

	cityPath := t.TempDir()
	cfg := &config.City{}
	agent := &config.Agent{Name: "worker"}

	env := controllerQueryEnv(cityPath, cfg, agent)
	if env == nil {
		t.Fatal("expected env, got nil")
	}
	for _, key := range []string{"GC_DOLT_USER", "GC_DOLT_PASSWORD", "BEADS_DOLT_SERVER_USER", "BEADS_DOLT_PASSWORD"} {
		if _, ok := env[key]; ok {
			t.Errorf("%s should not be in controllerQueryEnv output (credentials leak risk)", key)
		}
	}
}
