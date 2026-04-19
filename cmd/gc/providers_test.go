package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestTmuxConfigFromSessionDefaultsSocketToCityName(t *testing.T) {
	sc := config.SessionConfig{}

	cfg := tmuxConfigFromSession(sc, "city", "/tmp/city-a")
	if cfg.SocketName != "city" {
		t.Fatalf("SocketName = %q, want %q", cfg.SocketName, "city")
	}
}

func TestTmuxConfigFromSessionPreservesExplicitSocket(t *testing.T) {
	sc := config.SessionConfig{Socket: "custom-socket"}

	cfg := tmuxConfigFromSession(sc, "city", "/tmp/city-a")
	if cfg.SocketName != "custom-socket" {
		t.Fatalf("SocketName = %q, want %q", cfg.SocketName, "custom-socket")
	}
}

func TestSessionProviderContextForCityUsesTargetCityAndEnvOverride(t *testing.T) {
	t.Setenv("GC_SESSION", "subprocess")

	cfg := &config.City{
		Workspace: config.Workspace{
			Name:            "bright-lights",
			SessionTemplate: "{{.Agent}}",
		},
		Session: config.SessionConfig{
			Provider: "tmux",
			Socket:   "from-config",
		},
		Agents: []config.Agent{
			{Name: "mayor"},
		},
	}

	ctx := sessionProviderContextForCity(cfg, "/tmp/city-a", os.Getenv("GC_SESSION"))
	if ctx.cityPath != "/tmp/city-a" {
		t.Fatalf("cityPath = %q, want %q", ctx.cityPath, "/tmp/city-a")
	}
	if ctx.cityName != "bright-lights" {
		t.Fatalf("cityName = %q, want %q", ctx.cityName, "bright-lights")
	}
	if ctx.providerName != "subprocess" {
		t.Fatalf("providerName = %q, want %q", ctx.providerName, "subprocess")
	}
	if ctx.sessionTemplate != "{{.Agent}}" {
		t.Fatalf("sessionTemplate = %q, want %q", ctx.sessionTemplate, "{{.Agent}}")
	}
	if len(ctx.agents) != 1 || ctx.agents[0].Name != "mayor" {
		t.Fatalf("agents = %#v, want mayor", ctx.agents)
	}
}

func TestRawBeadsProviderNormalizesManagedExecEnv(t *testing.T) {
	cityPath := t.TempDir()
	t.Setenv("GC_BEADS", "exec:"+gcBeadsBdScriptPath(cityPath))

	if got := rawBeadsProvider(cityPath); got != "bd" {
		t.Fatalf("rawBeadsProvider() = %q, want bd", got)
	}
}

func TestRawBeadsProviderPreservesCustomExecOverride(t *testing.T) {
	t.Setenv("GC_BEADS", "exec:/tmp/custom-beads")

	if got := rawBeadsProvider(t.TempDir()); got != "exec:/tmp/custom-beads" {
		t.Fatalf("rawBeadsProvider() = %q, want custom exec override", got)
	}
}

func TestRawBeadsProviderForScopePreservesExplicitEnvOverride(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")

	if got := rawBeadsProviderForScope(rigDir, cityDir); got != "file" {
		t.Fatalf("rawBeadsProviderForScope() = %q, want explicit env override", got)
	}
}

func TestRawBeadsProviderForScopePreservesCustomExecProvider(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "exec:/tmp/custom-beads"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := rawBeadsProviderForScope(rigDir, cityDir); got != "exec:/tmp/custom-beads" {
		t.Fatalf("rawBeadsProviderForScope() = %q, want custom exec provider", got)
	}
}

func TestRawBeadsProviderForScopeKeepsSessionOverrideScoped(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", rigDir)

	if got := rawBeadsProviderForScope(rigDir, cityDir); got != "bd" {
		t.Fatalf("rawBeadsProviderForScope(rig) = %q, want bd", got)
	}
	if got := rawBeadsProviderForScope(cityDir, cityDir); got != "file" {
		t.Fatalf("rawBeadsProviderForScope(city) = %q, want file outside scoped override", got)
	}
}

func TestRawBeadsProviderForScopeIgnoresConfigYamlWithoutMetadata(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte("issue_prefix: fe\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := rawBeadsProviderForScope(rigDir, cityDir); got != "file" {
		t.Fatalf("rawBeadsProviderForScope() = %q, want city provider without bd metadata", got)
	}
}

func TestRawBeadsProviderForScopePrefersBdMetadataOverFileMarker(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "file"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"fe"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".gc", "beads.json"), []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := rawBeadsProviderForScope(rigDir, cityDir); got != "bd" {
		t.Fatalf("rawBeadsProviderForScope() = %q, want bd metadata to outrank stale file marker", got)
	}
}

func TestConfiguredACPSessionNames_UsesProvidedSnapshot(t *testing.T) {
	snapshot := newSessionBeadSnapshot([]beads.Bead{{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:reviewer"},
		Metadata: map[string]string{
			"template":     "reviewer",
			"agent_name":   "reviewer",
			"session_name": "custom-reviewer",
		},
	}})

	agents := []config.Agent{
		{Name: "reviewer", Session: "acp"},
		{Name: "witness", Session: "acp"},
		{Name: "mayor"},
	}

	got := configuredACPSessionNames(snapshot, "city", "", agents)
	want := []string{
		"custom-reviewer",
		agent.SessionNameFor("city", "witness", ""),
	}
	if len(got) != len(want) {
		t.Fatalf("configuredACPSessionNames len = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("configuredACPSessionNames[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestNewSessionProvider_PreregistersACPBeadAndLegacyNames(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_SESSION", "fake")

	cityDir := t.TempDir()
	t.Setenv("GC_CITY", cityDir)
	writeACPRouteCityTOML(t, cityDir, "test-city")

	store, err := openCityStoreAt(cityDir)
	if err != nil {
		t.Fatalf("openCityStoreAt: %v", err)
	}
	if _, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:reviewer"},
		Metadata: map[string]string{
			"template":     "reviewer",
			"agent_name":   "reviewer",
			"session_name": "custom-reviewer",
		},
	}); err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	sp := newSessionProvider()

	if err := sp.Attach("custom-reviewer"); err == nil || !strings.Contains(err.Error(), "ACP transport") {
		t.Fatalf("Attach(custom-reviewer) error = %v, want ACP transport error", err)
	}

	witnessName := agent.SessionNameFor("test-city", "witness", "")
	if err := sp.Attach(witnessName); err == nil || !strings.Contains(err.Error(), "ACP transport") {
		t.Fatalf("Attach(%q) error = %v, want ACP transport error", witnessName, err)
	}

	mayorName := agent.SessionNameFor("test-city", "mayor", "")
	if err := sp.Attach(mayorName); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("Attach(%q) error = %v, want fake-provider not found", mayorName, err)
	}
}

func TestLoadProviderSessionSnapshotSkipsStoreWithoutACPAgents(t *testing.T) {
	oldOpen := openSessionProviderStore
	t.Cleanup(func() { openSessionProviderStore = oldOpen })

	calls := 0
	openSessionProviderStore = func(string) (beads.Store, error) {
		calls++
		return beads.NewMemStore(), nil
	}

	snapshot := loadProviderSessionSnapshot(sessionProviderContext{
		providerName: "tmux",
		cityPath:     "/tmp/city",
		agents: []config.Agent{
			{Name: "mayor"},
		},
	})
	if snapshot != nil {
		t.Fatalf("loadProviderSessionSnapshot() = %#v, want nil", snapshot)
	}
	if calls != 0 {
		t.Fatalf("openSessionProviderStore called %d times, want 0", calls)
	}
}

func TestLoadProviderSessionSnapshotLoadsOpenACPAgents(t *testing.T) {
	oldOpen := openSessionProviderStore
	t.Cleanup(func() { openSessionProviderStore = oldOpen })

	store := beads.NewMemStore()
	if _, err := store.Create(beads.Bead{
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel, "agent:reviewer"},
		Metadata: map[string]string{
			"template":     "reviewer",
			"agent_name":   "reviewer",
			"session_name": "custom-reviewer",
		},
	}); err != nil {
		t.Fatalf("Create(session bead): %v", err)
	}

	calls := 0
	openSessionProviderStore = func(string) (beads.Store, error) {
		calls++
		return store, nil
	}

	snapshot := loadProviderSessionSnapshot(sessionProviderContext{
		providerName: "tmux",
		cityPath:     "/tmp/city",
		agents: []config.Agent{
			{Name: "reviewer", Session: "acp"},
		},
	})
	if calls != 1 {
		t.Fatalf("openSessionProviderStore called %d times, want 1", calls)
	}
	if snapshot == nil {
		t.Fatal("loadProviderSessionSnapshot() = nil, want snapshot")
	}
	if got := snapshot.FindSessionNameByTemplate("reviewer"); got != "custom-reviewer" {
		t.Fatalf("snapshot.FindSessionNameByTemplate(reviewer) = %q, want %q", got, "custom-reviewer")
	}
}

func writeACPRouteCityTOML(t *testing.T, dir, cityName string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.gc): %v", err)
	}
	data := []byte(`[workspace]
name = "` + cityName + `"

[beads]
provider = "file"

[[agent]]
name = "reviewer"
provider = "claude"
start_command = "echo"
session = "acp"

[[agent]]
name = "witness"
provider = "claude"
start_command = "echo"
session = "acp"

[[agent]]
name = "mayor"
provider = "claude"
start_command = "echo"
`)
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatalf("WriteFile(city.toml): %v", err)
	}
}
