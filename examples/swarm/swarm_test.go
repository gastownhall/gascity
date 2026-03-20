// Package swarm_test validates the Swarm example configuration.
//
// This test ensures the example stays valid as the SDK evolves:
// city.toml parses and validates, prompt template files exist, and
// the pack has the expected agents.
package swarm_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func exampleDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

// loadExpanded loads city.toml with full pack expansion.
func loadExpanded(t *testing.T) *config.City {
	t.Helper()
	dir := exampleDir()
	cfg, _, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("config.LoadWithIncludes: %v", err)
	}
	return cfg
}

func TestCityTomlParses(t *testing.T) {
	dir := exampleDir()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Workspace.Name != "swarm" {
		t.Errorf("Workspace.Name = %q, want %q", cfg.Workspace.Name, "swarm")
	}
	if len(cfg.Workspace.Includes) != 1 || cfg.Workspace.Includes[0] != "packs/swarm" {
		t.Errorf("Workspace.Includes = %v, want [packs/swarm]", cfg.Workspace.Includes)
	}
}

func TestCityTomlValidates(t *testing.T) {
	cfg := loadExpanded(t)
	if err := config.ValidateAgents(cfg.Agents); err != nil {
		t.Errorf("ValidateAgents: %v", err)
	}
}

func TestPromptFilesExist(t *testing.T) {
	dir := exampleDir()
	cfg := loadExpanded(t)
	for _, a := range cfg.Agents {
		if a.PromptTemplate == "" || a.Implicit {
			continue
		}
		path := filepath.Join(dir, a.PromptTemplate)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("agent %q: prompt_template %q: %v", a.Name, a.PromptTemplate, err)
		}
	}
}

func TestOverlayDirsExist(t *testing.T) {
	dir := exampleDir()
	cfg := loadExpanded(t)
	for _, a := range cfg.Agents {
		if a.OverlayDir == "" {
			continue
		}
		path := filepath.Join(dir, a.OverlayDir)
		if info, err := os.Stat(path); err != nil {
			t.Errorf("agent %q: overlay_dir %q: %v", a.Name, a.OverlayDir, err)
		} else if !info.IsDir() {
			t.Errorf("agent %q: overlay_dir %q is not a directory", a.Name, a.OverlayDir)
		}
	}
}

// packFileConfig mirrors the pack.toml structure for test parsing.
type packFileConfig struct {
	Pack   config.PackMeta `toml:"pack"`
	Agents []config.Agent  `toml:"agent"`
}

func TestCombinedPackParses(t *testing.T) {
	dir := exampleDir()
	topoPath := filepath.Join(dir, "packs", "swarm", "pack.toml")

	data, err := os.ReadFile(topoPath)
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}

	var tc packFileConfig
	if _, err := toml.Decode(string(data), &tc); err != nil {
		t.Fatalf("parsing pack.toml: %v", err)
	}

	if tc.Pack.Name != "swarm" {
		t.Errorf("[pack] name = %q, want %q", tc.Pack.Name, "swarm")
	}
	if tc.Pack.Schema != 1 {
		t.Errorf("[pack] schema = %d, want 1", tc.Pack.Schema)
	}

	// Expect 5 agents: mayor, deacon, dog (city), coder, committer (rig).
	want := map[string]bool{
		"mayor": false, "deacon": false, "dog": false,
		"coder": false, "committer": false,
	}
	for _, a := range tc.Agents {
		if _, ok := want[a.Name]; ok {
			want[a.Name] = true
		} else {
			t.Errorf("unexpected pack agent %q", a.Name)
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("missing pack agent %q", name)
		}
	}
	if len(tc.Agents) != 5 {
		t.Errorf("pack has %d agents, want 5", len(tc.Agents))
	}

	// Verify city-scoped agents have scope = "city".
	wantCity := map[string]bool{"mayor": true, "deacon": true, "dog": true}
	for _, a := range tc.Agents {
		if wantCity[a.Name] && a.Scope != "city" {
			t.Errorf("agent %q: scope = %q, want %q", a.Name, a.Scope, "city")
		}
	}
}

func TestCityAgentsFilter(t *testing.T) {
	// Without rigs, only city-scoped agents appear.
	cfg := loadExpanded(t)

	cityAgents := map[string]bool{"mayor": true, "deacon": true, "dog": true}
	var explicit int
	for _, a := range cfg.Agents {
		if a.Implicit {
			continue
		}
		explicit++
		if !cityAgents[a.Name] {
			t.Errorf("unexpected agent %q — should be filtered out without rigs", a.Name)
		}
		if a.Dir != "" {
			t.Errorf("city agent %q: dir = %q, want empty", a.Name, a.Dir)
		}
	}
	if explicit != 3 {
		t.Errorf("got %d explicit agents, want 3 city-scoped agents", explicit)
	}
}

func TestAgentNudgeField(t *testing.T) {
	cfg := loadExpanded(t)

	nudgeCounts := 0
	for _, a := range cfg.Agents {
		if a.Nudge != "" {
			nudgeCounts++
		}
	}
	if nudgeCounts == 0 {
		t.Error("no agents have nudge configured")
	}
}

func TestDaemonConfig(t *testing.T) {
	dir := exampleDir()
	cfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if cfg.Daemon.PatrolInterval != "30s" {
		t.Errorf("Daemon.PatrolInterval = %q, want %q", cfg.Daemon.PatrolInterval, "30s")
	}
	if cfg.Daemon.MaxRestartsOrDefault() != 5 {
		t.Errorf("Daemon.MaxRestarts = %d, want 5", cfg.Daemon.MaxRestartsOrDefault())
	}
	if cfg.Daemon.RestartWindow != "1h" {
		t.Errorf("Daemon.RestartWindow = %q, want %q", cfg.Daemon.RestartWindow, "1h")
	}
	if cfg.Daemon.ShutdownTimeout != "5s" {
		t.Errorf("Daemon.ShutdownTimeout = %q, want %q", cfg.Daemon.ShutdownTimeout, "5s")
	}
}

func TestAllPromptTemplatesExist(t *testing.T) {
	dir := exampleDir()
	promptDir := filepath.Join(dir, "packs", "swarm", "prompts")

	entries, err := os.ReadDir(promptDir)
	if err != nil {
		t.Fatalf("reading prompts dir: %v", err)
	}

	var count int
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md.tmpl") {
			continue
		}
		count++
		t.Run(e.Name(), func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(promptDir, e.Name()))
			if err != nil {
				t.Fatalf("reading %s: %v", e.Name(), err)
			}
			if len(data) == 0 {
				t.Errorf("%s is empty", e.Name())
			}
		})
	}

	if count != 5 {
		t.Errorf("found %d prompt template files, want 5", count)
	}
}

func TestPackPromptFilesExist(t *testing.T) {
	dir := exampleDir()
	topoDir := filepath.Join(dir, "packs", "swarm")
	topoPath := filepath.Join(topoDir, "pack.toml")

	data, err := os.ReadFile(topoPath)
	if err != nil {
		t.Fatalf("reading pack.toml: %v", err)
	}

	var tc packFileConfig
	if _, err := toml.Decode(string(data), &tc); err != nil {
		t.Fatalf("parsing pack.toml: %v", err)
	}

	for _, a := range tc.Agents {
		if a.PromptTemplate == "" {
			continue
		}
		path := filepath.Join(topoDir, a.PromptTemplate)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("agent %q: prompt_template %q resolves to %q: %v",
				a.Name, a.PromptTemplate, path, err)
		}
	}
}
