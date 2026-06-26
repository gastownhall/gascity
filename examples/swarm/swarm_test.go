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
	"github.com/gastownhall/gascity/internal/builtinpacks"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/packman"
)

func exampleDir() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Dir(filename)
}

// primeBundledPackCache hydrates a hermetic repo cache with the bundled
// builtin pack content at the pinned commit so the example's packs.lock
// resolves offline.
func primeBundledPackCache(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("GC_HOME", filepath.Join(home, ".gc"))
	commit := strings.TrimPrefix(config.BundledPackImportVersion, "sha:")
	coreSource, ok := builtinpacks.Source("core")
	if !ok {
		t.Fatal("bundled core pack not registered")
	}
	cachePath, err := packman.RepoCachePath(coreSource, commit)
	if err != nil {
		t.Fatalf("RepoCachePath: %v", err)
	}
	if err := builtinpacks.MaterializeSyntheticRepo(cachePath, commit); err != nil {
		t.Fatalf("MaterializeSyntheticRepo: %v", err)
	}
}

// loadExpanded loads city.toml with full pack expansion.
func loadExpanded(t *testing.T) *config.City {
	t.Helper()
	primeBundledPackCache(t)
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
	if gotIncludes := cfg.Workspace.LegacyIncludes(); len(gotIncludes) != 0 {
		t.Errorf("Workspace.Includes = %v, want none (builtin packs compose via pack.toml imports)", gotIncludes)
	}
	if imp, ok := cfg.Imports["core"]; ok {
		t.Errorf("cfg.Imports[core] = %v, want absent from city.toml (pinned in pack.toml)", imp)
	}
}

func TestRootPackOwnsSwarmImport(t *testing.T) {
	dir := exampleDir()
	packCfg, err := config.Load(fsys.OSFS{}, filepath.Join(dir, "pack.toml"))
	if err != nil {
		t.Fatalf("config.Load(pack.toml): %v", err)
	}
	if got := packCfg.Imports["swarm"].Source; got != "packs/swarm" {
		t.Fatalf("pack import swarm = %q, want %q", got, "packs/swarm")
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
		path := resolveExamplePath(dir, a.PromptTemplate)
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
		path := resolveExamplePath(dir, a.OverlayDir)
		if info, err := os.Stat(path); err != nil {
			t.Errorf("agent %q: overlay_dir %q: %v", a.Name, a.OverlayDir, err)
		} else if !info.IsDir() {
			t.Errorf("agent %q: overlay_dir %q is not a directory", a.Name, a.OverlayDir)
		}
	}
}

// packFileConfig mirrors the pack.toml structure for test parsing.
type packFileConfig struct {
	Pack config.PackMeta `toml:"pack"`
}

func discoverPackAgents(t *testing.T, rel string) []config.Agent {
	t.Helper()
	packDir := filepath.Join(exampleDir(), rel)
	agents, err := config.DiscoverPackAgents(fsys.OSFS{}, packDir, filepath.Base(rel), nil)
	if err != nil {
		t.Fatalf("DiscoverPackAgents(%s): %v", rel, err)
	}
	return agents
}

func resolveExamplePath(base, candidate string) string {
	if filepath.IsAbs(candidate) {
		return candidate
	}
	return filepath.Join(base, candidate)
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
	if tc.Pack.Schema != 2 {
		t.Errorf("[pack] schema = %d, want 2", tc.Pack.Schema)
	}

	// Expect 4 locally-defined agents: mayor, deacon (city), coder, committer (rig).
	// The initialized city picks up dog from the system-provided maintenance pack.
	agents := discoverPackAgents(t, filepath.Join("packs", "swarm"))
	want := map[string]bool{
		"mayor": false, "deacon": false,
		"coder": false, "committer": false,
	}
	for _, a := range agents {
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
	if len(agents) != 4 {
		t.Errorf("pack has %d locally-defined agents, want 4", len(agents))
	}

	// Verify the pack's local city-scoped agents have scope = "city".
	wantCity := map[string]bool{"mayor": true, "deacon": true}
	for _, a := range agents {
		if wantCity[a.Name] && a.Scope != "city" {
			t.Errorf("agent %q: scope = %q, want %q", a.Name, a.Scope, "city")
		}
	}
}

func TestCityAgentsFilter(t *testing.T) {
	// Swarm's own city-scoped agents plus the dolt maintenance dog that the
	// composed builtin packs contribute (bd imports dolt transitively), plus
	// the visible core control dispatcher.
	cfg := loadExpanded(t)

	cityAgents := map[string]bool{"mayor": true, "deacon": true, "dog": true, "control-dispatcher": true}
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
		if a.Name == "dog" && a.BindingName != "bd" {
			t.Errorf("dog agent binding = %q, want bd (city-level imports stamp the city binding)", a.BindingName)
		}
	}
	if explicit != 4 {
		t.Errorf("got %d explicit agents, want mayor + deacon + dolt dog + control-dispatcher", explicit)
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
	var count int
	for _, a := range discoverPackAgents(t, filepath.Join("packs", "swarm")) {
		if a.PromptTemplate == "" {
			continue
		}
		count++
		data, err := os.ReadFile(a.PromptTemplate)
		if err != nil {
			t.Fatalf("reading %s prompt: %v", a.Name, err)
		}
		if len(data) == 0 {
			t.Errorf("%s prompt is empty", a.Name)
		}
	}

	if count != 4 {
		t.Errorf("found %d local prompt template files, want 4", count)
	}
}

func TestPackPromptFilesExist(t *testing.T) {
	for _, a := range discoverPackAgents(t, filepath.Join("packs", "swarm")) {
		if a.PromptTemplate == "" {
			continue
		}
		if _, err := os.Stat(a.PromptTemplate); err != nil {
			t.Errorf("agent %q: prompt_template %q: %v", a.Name, a.PromptTemplate, err)
		}
	}
}
