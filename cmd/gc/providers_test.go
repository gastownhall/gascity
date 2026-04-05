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
