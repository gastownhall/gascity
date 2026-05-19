package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestPrefixedWorkQueryForProbe_UsesNamedSessionRuntimeName(t *testing.T) {
	cityPath := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityPath, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
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

	command := prefixedWorkQueryForProbe(cfg, cityPath, "test-city", nil, nil, &cfg.Agents[0], nil)
	if !strings.Contains(command, "gc.routed_to=demo/witness") {
		t.Fatalf("prefixedWorkQueryForProbe() = %q, want gc.routed_to=demo/witness", command)
	}
}

func TestControllerQueryRuntimeEnvInheritedRigUsesCityStorePassword(t *testing.T) {
	cityPath, rigDir, cfg := newControllerProbeFixture(t)
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeScopePassword(t, rigDir, "rig-secret")

	env := controllerQueryRuntimeEnv(cityPath, cfg, &cfg.Agents[0])
	if got := env["GC_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "city-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "city-secret")
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want rig beads dir", got)
	}
}

func TestControllerQueryRuntimeEnvExplicitRigUsesRigStorePassword(t *testing.T) {
	cityPath, rigDir, cfg := newControllerProbeFixture(t)
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "4406",
		DoltUser:       "rig-user",
	})
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "de",
	}); err != nil {
		t.Fatal(err)
	}
	writeScopePassword(t, rigDir, "rig-secret")

	env := controllerQueryRuntimeEnv(cityPath, cfg, &cfg.Agents[0])
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "4406")
	}
	if got := env["GC_DOLT_USER"]; got != "rig-user" {
		t.Fatalf("GC_DOLT_USER = %q, want %q", got, "rig-user")
	}
	if got := env["GC_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("BEADS_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
}

func TestControllerQueryRuntimeEnvSupportsExecGcBeadsBd(t *testing.T) {
	cityPath, rigDir, cfg := newControllerProbeFixture(t)
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityPath))
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "4406",
		DoltUser:       "rig-user",
	})
	writeScopePassword(t, rigDir, "rig-secret")

	env := controllerQueryRuntimeEnv(cityPath, cfg, &cfg.Agents[0])
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "4406")
	}
	if got := env["GC_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
	if got := env["BEADS_DIR"]; got != filepath.Join(rigDir, ".beads") {
		t.Fatalf("BEADS_DIR = %q, want rig beads dir", got)
	}
}

func TestControllerQueryEnvOmitsCredentialsFromPrefix(t *testing.T) {
	cityPath, rigDir, cfg := newControllerProbeFixture(t)
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "4406",
		DoltUser:       "rig-user",
	})
	writeScopePassword(t, rigDir, "rig-secret")

	env := controllerQueryEnv(cityPath, cfg, &cfg.Agents[0])
	if got := env["GC_DOLT_PASSWORD"]; got != "" {
		t.Fatalf("GC_DOLT_PASSWORD leaked into prefix env as %q", got)
	}
	if got := env["BEADS_DOLT_PASSWORD"]; got != "" {
		t.Fatalf("BEADS_DOLT_PASSWORD leaked into prefix env as %q", got)
	}
	if got := env["GC_DOLT_USER"]; got != "" {
		t.Fatalf("GC_DOLT_USER leaked into prefix env as %q", got)
	}
	if got := env["BEADS_DOLT_SERVER_HOST"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_HOST leaked into prefix env as %q", got)
	}
	if got := env["BEADS_DOLT_SERVER_PORT"]; got != "" {
		t.Fatalf("BEADS_DOLT_SERVER_PORT leaked into prefix env as %q", got)
	}
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "4406")
	}
	command := prefixedWorkQueryForProbeWithEnv(env, cfg, cityPath, cfg.Workspace.Name, nil, nil, &cfg.Agents[0], nil)
	if strings.Contains(command, "rig-secret") || strings.Contains(command, "city-secret") {
		t.Fatalf("prefixedWorkQueryForProbeWithEnv leaked credentials into command: %q", command)
	}
}

func TestControllerQueryRuntimeEnvReturnsNilForNonBD(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents:    []config.Agent{{Name: "worker"}},
	}
	if env := controllerQueryRuntimeEnv(cityPath, cfg, &cfg.Agents[0]); env != nil {
		t.Fatalf("controllerQueryRuntimeEnv() = %#v, want nil for non-bd provider", env)
	}
}

func TestControllerQueryRuntimeEnvUsesRigBdScopeUnderFileBackedCity(t *testing.T) {
	cityPath := t.TempDir()
	rigDir := filepath.Join(cityPath, "demo")
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(`[workspace]
name = "test-city"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeCanonicalScopeConfig(t, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "4406",
		DoltUser:       "rig-user",
	})
	if _, err := contract.EnsureCanonicalMetadata(fsys.OSFS{}, filepath.Join(rigDir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: "de",
	}); err != nil {
		t.Fatal(err)
	}
	writeScopePassword(t, rigDir, "rig-secret")
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{{
			Name:   "demo",
			Path:   rigDir,
			Prefix: "de",
		}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "demo",
		}},
	}

	env := controllerQueryRuntimeEnv(cityPath, cfg, &cfg.Agents[0])
	if got := env["GC_DOLT_HOST"]; got != "rig-db.example.com" {
		t.Fatalf("GC_DOLT_HOST = %q, want %q", got, "rig-db.example.com")
	}
	if got := env["GC_DOLT_PORT"]; got != "4406" {
		t.Fatalf("GC_DOLT_PORT = %q, want %q", got, "4406")
	}
	if got := env["GC_DOLT_PASSWORD"]; got != "rig-secret" {
		t.Fatalf("GC_DOLT_PASSWORD = %q, want %q", got, "rig-secret")
	}
}

func newControllerProbeFixture(t *testing.T) (string, string, *config.City) {
	t.Helper()
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_DOLT_PASSWORD", "")
	_ = os.Unsetenv("GC_DOLT_PASSWORD")

	cityPath := t.TempDir()
	rigDir := filepath.Join(cityPath, "demo")
	mustMkdirAll(t, filepath.Join(cityPath, ".beads"))
	mustMkdirAll(t, filepath.Join(rigDir, ".beads"))
	writeCanonicalScopeConfig(t, cityPath, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeScopePassword(t, cityPath, "city-secret")
	_ = writeReachableManagedDoltState(t, cityPath)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{{
			Name:   "demo",
			Path:   rigDir,
			Prefix: "de",
		}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "demo",
		}},
	}
	return cityPath, rigDir, cfg
}

func writeCanonicalScopeConfig(t *testing.T, scopeRoot string, state contract.ConfigState) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(scopeRoot, ".beads"))
	if _, err := contract.EnsureCanonicalConfig(fsys.OSFS{}, filepath.Join(scopeRoot, ".beads", "config.yaml"), state); err != nil {
		t.Fatalf("EnsureCanonicalConfig(%s): %v", scopeRoot, err)
	}
}

func writeScopePassword(t *testing.T, scopeRoot, password string) {
	t.Helper()
	mustMkdirAll(t, filepath.Join(scopeRoot, ".beads"))
	if err := os.WriteFile(filepath.Join(scopeRoot, ".beads", ".env"), []byte("BEADS_DOLT_PASSWORD="+password+"\n"), 0o600); err != nil {
		t.Fatalf("write scope password: %v", err)
	}
}

func mustMkdirAll(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}
}
