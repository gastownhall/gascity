package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// writeFile is a test helper that creates a file in dir.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestExpandPacks_Basic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "witness"
prompt_template = "prompts/witness.md"

[[agent]]
name = "refinery"
`)

	writeFile(t, dir, "packs/gastown/prompts/witness.md", "you are a witness")

	cfg := &City{
		Rigs: []Rig{
			{Name: "hello-world", Path: "/home/user/hello-world", Includes: []string{"packs/gastown"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(cfg.Agents))
	}
	// Agents should have dir stamped to rig name.
	for _, a := range cfg.Agents {
		if a.Dir != "hello-world" {
			t.Errorf("agent %q: dir = %q, want %q", a.Name, a.Dir, "hello-world")
		}
	}
	// witness should have adjusted prompt_template path.
	if !strings.Contains(cfg.Agents[0].PromptTemplate, "prompts/witness.md") {
		t.Errorf("witness prompt_template = %q, want to contain prompts/witness.md", cfg.Agents[0].PromptTemplate)
	}
}

func TestExpandPacks_MultipleRigs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "polecat"
min_active_sessions = 0
max_active_sessions = 3
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "proj-a", Path: "/a", Includes: []string{"packs/gastown"}},
			{Name: "proj-b", Path: "/b", Includes: []string{"packs/gastown"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(cfg.Agents))
	}
	// Each rig gets its own stamped copy.
	if cfg.Agents[0].Dir != "proj-a" {
		t.Errorf("first polecat dir = %q, want proj-a", cfg.Agents[0].Dir)
	}
	if cfg.Agents[1].Dir != "proj-b" {
		t.Errorf("second polecat dir = %q, want proj-b", cfg.Agents[1].Dir)
	}
	// Scaling config should be preserved.
	if cfg.Agents[0].MaxActiveSessions == nil || *cfg.Agents[0].MaxActiveSessions != 3 {
		t.Errorf("first polecat scaling not preserved: max=%v", cfg.Agents[0].MaxActiveSessions)
	}
}

func TestExpandPacks_NoPack(t *testing.T) {
	cfg := &City{
		Agents: []Agent{{Name: "mayor"}},
		Rigs:   []Rig{{Name: "simple", Path: "/simple"}},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, "/tmp", nil); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Errorf("got %d agents, want 1 (unchanged)", len(cfg.Agents))
	}
}

func TestExpandPacks_MixedRigs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/basic/pack.toml", `
[pack]
name = "basic"
version = "0.1.0"
schema = 1

[[agent]]
name = "worker"
`)

	cfg := &City{
		Agents: []Agent{{Name: "mayor"}},
		Rigs: []Rig{
			{Name: "with-topo", Path: "/a", Includes: []string{"packs/basic"}},
			{Name: "no-topo", Path: "/b"},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("first agent should be mayor, got %q", cfg.Agents[0].Name)
	}
	if cfg.Agents[1].Name != "worker" || cfg.Agents[1].Dir != "with-topo" {
		t.Errorf("second agent: name=%q dir=%q, want worker/with-topo", cfg.Agents[1].Name, cfg.Agents[1].Dir)
	}
}

func TestExpandPacks_OverrideDirStamp(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "witness"
`)

	dirOverride := "services/api"
	cfg := &City{
		Rigs: []Rig{
			{
				Name:     "monorepo",
				Path:     "/home/user/mono",
				Includes: []string{"packs/gt"},
				Overrides: []AgentOverride{
					{Agent: "witness", Dir: &dirOverride},
				},
			},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	if cfg.Agents[0].Dir != "services/api" {
		t.Errorf("dir = %q, want %q", cfg.Agents[0].Dir, "services/api")
	}
}

func TestExpandPacks_OverridePool(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "polecat"
min_active_sessions = 0
max_active_sessions = 3
`)

	maxOverride := 10
	cfg := &City{
		Rigs: []Rig{
			{
				Name:     "big-project",
				Path:     "/big",
				Includes: []string{"packs/gt"},
				Overrides: []AgentOverride{
					{Agent: "polecat", Pool: &PoolOverride{Max: &maxOverride}},
				},
			},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	if cfg.Agents[0].MaxActiveSessions == nil {
		t.Fatal("MaxActiveSessions is nil")
	}
	if *cfg.Agents[0].MaxActiveSessions != 10 {
		t.Errorf("MaxActiveSessions = %d, want 10", *cfg.Agents[0].MaxActiveSessions)
	}
	if cfg.Agents[0].MinActiveSessions == nil || *cfg.Agents[0].MinActiveSessions != 0 {
		t.Errorf("MinActiveSessions = %v, want 0 (preserved from pack)", cfg.Agents[0].MinActiveSessions)
	}
}

func TestExpandPacks_OverrideSuspend(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "witness"
`)

	suspended := true
	cfg := &City{
		Rigs: []Rig{
			{
				Name:     "hw",
				Path:     "/hw",
				Includes: []string{"packs/gt"},
				Overrides: []AgentOverride{
					{Agent: "witness", Suspended: &suspended},
				},
			},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	if !cfg.Agents[0].Suspended {
		t.Error("witness should be suspended")
	}
}

func TestExpandPacks_OverrideNotFound(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "witness"
`)

	suspended := true
	cfg := &City{
		Rigs: []Rig{
			{
				Name:     "hw",
				Path:     "/hw",
				Includes: []string{"packs/gt"},
				Overrides: []AgentOverride{
					{Agent: "nonexistent", Suspended: &suspended},
				},
			},
		},
	}

	err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil)
	if err == nil {
		t.Fatal("expected error for nonexistent override target")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error should mention nonexistent, got: %v", err)
	}
}

func TestExpandPacks_MissingPackFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/empty/.keep", "")

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/empty"}},
		},
	}

	err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil)
	if err == nil {
		t.Fatal("expected error for missing pack.toml")
	}
}

func TestExpandPacks_BadSchema(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/future/pack.toml", `
[pack]
name = "future"
version = "9.0.0"
schema = 99
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/future"}},
		},
	}

	err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil)
	if err == nil {
		t.Fatal("expected error for unsupported schema")
	}
	if !strings.Contains(err.Error(), "schema 99 not supported") {
		t.Errorf("error should mention schema, got: %v", err)
	}
}

func TestExpandPacks_MissingName(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/bad/pack.toml", `
[pack]
version = "1.0.0"
schema = 1
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/bad"}},
		},
	}

	err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil)
	if err == nil {
		t.Fatal("expected error for missing pack name")
	}
}

func TestExpandPacks_MissingSchema(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/bad/pack.toml", `
[pack]
name = "bad"
version = "1.0.0"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/bad"}},
		},
	}

	err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil)
	if err == nil {
		t.Fatal("expected error for missing schema")
	}
}

func TestExpandPacks_RejectsUnknownPackTomlFields(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/bad/pack.toml", `
[pack]
name = "bad"
schema = 1
scheam = 1
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/bad"}},
		},
	}

	err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil)
	if err == nil {
		t.Fatal("expected error for unknown pack.toml field")
	}
	if !strings.Contains(err.Error(), `unknown field "pack.scheam"`) {
		t.Fatalf("error should mention unknown pack field, got: %v", err)
	}
	if !strings.Contains(err.Error(), `did you mean "schema"`) {
		t.Fatalf("error should suggest schema, got: %v", err)
	}
}

func TestExpandPacks_RejectsUnknownPackImportFields(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/helper/pack.toml", `
[pack]
name = "helper"
schema = 1
`)
	writeFile(t, dir, "packs/bad/pack.toml", `
[pack]
name = "bad"
schema = 2

[imports.helper]
sorce = "../helper"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/bad"}},
		},
	}

	err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil)
	if err == nil {
		t.Fatal("expected error for unknown import field")
	}
	if !strings.Contains(err.Error(), `unknown field "imports.helper.sorce"`) {
		t.Fatalf("error should mention unknown import field, got: %v", err)
	}
	if !strings.Contains(err.Error(), `did you mean "source"`) {
		t.Fatalf("error should suggest source, got: %v", err)
	}
}

func TestExpandPacks_PromptPathResolution(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "witness"
prompt_template = "prompts/witness.md"

[[agent]]
name = "refinery"
prompt_template = "//prompts/shared.md"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// Relative path: resolved relative to pack dir, then made city-root-relative.
	if cfg.Agents[0].PromptTemplate != "packs/gt/prompts/witness.md" {
		t.Errorf("witness prompt = %q, want packs/gt/prompts/witness.md", cfg.Agents[0].PromptTemplate)
	}
	// "//" path: resolved to city root.
	if cfg.Agents[1].PromptTemplate != "prompts/shared.md" {
		t.Errorf("refinery prompt = %q, want prompts/shared.md", cfg.Agents[1].PromptTemplate)
	}
}

func TestExpandPacks_ProvidersMerged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[providers.codex]
command = "codex"
args = ["--full-auto"]

[[agent]]
name = "witness"
provider = "codex"
`)

	cfg := &City{
		Providers: map[string]ProviderSpec{
			"claude": {Command: "claude"},
		},
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// codex provider should be added.
	if _, ok := cfg.Providers["codex"]; !ok {
		t.Error("codex provider should be merged from pack")
	}
	// claude should still exist.
	if _, ok := cfg.Providers["claude"]; !ok {
		t.Error("claude provider should still exist")
	}
}

func TestExpandPacks_ProvidersNoOverwrite(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[providers.claude]
command = "claude-from-topo"

[[agent]]
name = "witness"
`)

	cfg := &City{
		Providers: map[string]ProviderSpec{
			"claude": {Command: "claude-original"},
		},
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// City's existing provider should NOT be overwritten by pack.
	if cfg.Providers["claude"].Command != "claude-original" {
		t.Errorf("claude command = %q, want claude-original (should not be overwritten)", cfg.Providers["claude"].Command)
	}
}

func TestPackContentHash_Deterministic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pack.toml", `[pack]
name = "test"
schema = 1
`)
	writeFile(t, dir, "prompts/witness.md", "witness prompt")

	h1 := PackContentHash(fsys.OSFS{}, dir)
	h2 := PackContentHash(fsys.OSFS{}, dir)
	if h1 != h2 {
		t.Errorf("hash not deterministic: %q vs %q", h1, h2)
	}
	if h1 == "" {
		t.Error("hash should not be empty")
	}
}

func TestPackContentHash_ChangesOnModification(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pack.toml", `[pack]
name = "test"
schema = 1
`)

	h1 := PackContentHash(fsys.OSFS{}, dir)

	// Modify the file.
	writeFile(t, dir, "pack.toml", `[pack]
name = "test-modified"
schema = 1
`)

	h2 := PackContentHash(fsys.OSFS{}, dir)
	if h1 == h2 {
		t.Error("hash should change when file content changes")
	}
}

func TestPackContentHashRecursive(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pack.toml", "test")
	writeFile(t, dir, "prompts/a.md", "prompt a")
	writeFile(t, dir, "prompts/b.md", "prompt b")

	h1 := PackContentHashRecursive(fsys.OSFS{}, dir)
	if h1 == "" {
		t.Error("hash should not be empty")
	}

	// Should be deterministic.
	h2 := PackContentHashRecursive(fsys.OSFS{}, dir)
	if h1 != h2 {
		t.Errorf("hash not deterministic: %q vs %q", h1, h2)
	}

	// Change a subdirectory file.
	writeFile(t, dir, "prompts/a.md", "modified prompt a")
	h3 := PackContentHashRecursive(fsys.OSFS{}, dir)
	if h3 == h1 {
		t.Error("hash should change when subdirectory file changes")
	}
}

func TestExpandPacks_ViaLoadWithIncludes(t *testing.T) {
	dir := t.TempDir()

	// Write pack.
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "witness"
prompt_template = "prompts/witness.md"
`)
	writeFile(t, dir, "packs/gt/prompts/witness.md", "you are a witness")

	// Write city.toml with a rig that references the pack.
	writeFile(t, dir, "city.toml", `
[workspace]
name = "test-city"

[[agent]]
name = "mayor"

[[rigs]]
name = "hello-world"
path = "/home/user/hw"
includes = ["packs/gt"]
`)

	cfg, prov, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	// Should have mayor + witness (explicit agents only).
	explicit := explicitAgents(cfg.Agents)
	if len(explicit) != 2 {
		t.Fatalf("got %d explicit agents, want 2", len(explicit))
	}
	if explicit[0].Name != "mayor" {
		t.Errorf("first agent = %q, want mayor", explicit[0].Name)
	}
	if explicit[1].Name != "witness" {
		t.Errorf("second agent = %q, want witness", explicit[1].Name)
	}
	if explicit[1].Dir != "hello-world" {
		t.Errorf("witness dir = %q, want hello-world", explicit[1].Dir)
	}

	// Provenance should track pack agents.
	if src, ok := prov.Agents["hello-world/witness"]; !ok {
		t.Error("provenance should track hello-world/witness")
	} else if !strings.Contains(src, "pack.toml") {
		t.Errorf("witness provenance = %q, want to contain pack.toml", src)
	}
}

func TestExpandPacks_OverrideEnv(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "witness"
[agent.env]
ROLE = "witness"
DEBUG = "false"
`)

	cfg := &City{
		Rigs: []Rig{
			{
				Name:     "hw",
				Path:     "/hw",
				Includes: []string{"packs/gt"},
				Overrides: []AgentOverride{
					{
						Agent:     "witness",
						Env:       map[string]string{"DEBUG": "true", "EXTRA": "val"},
						EnvRemove: []string{"ROLE"},
					},
				},
			},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	env := cfg.Agents[0].Env
	if env["DEBUG"] != "true" {
		t.Errorf("DEBUG = %q, want true", env["DEBUG"])
	}
	if env["EXTRA"] != "val" {
		t.Errorf("EXTRA = %q, want val", env["EXTRA"])
	}
	if _, ok := env["ROLE"]; ok {
		t.Error("ROLE should have been removed")
	}
}

func TestPackSummary(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "2.1.0"
schema = 1

[[agent]]
name = "witness"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}},
			{Name: "simple", Path: "/simple"},
		},
	}

	summary := PackSummary(cfg, fsys.OSFS{}, dir)

	if _, ok := summary["simple"]; ok {
		t.Error("simple rig (no pack) should not appear in summary")
	}
	s, ok := summary["hw"]
	if !ok {
		t.Fatal("hw should appear in summary")
	}
	if !strings.Contains(s, "gastown") {
		t.Errorf("summary should contain pack name, got: %q", s)
	}
	if !strings.Contains(s, "2.1.0") {
		t.Errorf("summary should contain version, got: %q", s)
	}
}

func TestResolveNamedPacks_Basic(t *testing.T) {
	cfg := &City{
		Packs: map[string]PackSource{
			"gastown": {Source: "https://example.com/gastown.git"},
		},
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"gastown"}},
		},
	}

	resolveNamedPacks(cfg, "/city")

	want := "/city/.gc/cache/packs/gastown"
	if cfg.Rigs[0].Includes[0] != want {
		t.Errorf("Includes[0] = %q, want %q", cfg.Rigs[0].Includes[0], want)
	}
}

func TestResolveNamedPacks_WithPath(t *testing.T) {
	cfg := &City{
		Packs: map[string]PackSource{
			"mono": {Source: "https://example.com/mono.git", Path: "packages/topo"},
		},
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"mono"}},
		},
	}

	resolveNamedPacks(cfg, "/city")

	want := "/city/.gc/cache/packs/mono/packages/topo"
	if cfg.Rigs[0].Includes[0] != want {
		t.Errorf("Includes[0] = %q, want %q", cfg.Rigs[0].Includes[0], want)
	}
}

func TestResolveNamedPacks_LocalPathUnchanged(t *testing.T) {
	cfg := &City{
		Packs: map[string]PackSource{
			"gastown": {Source: "https://example.com/gastown.git"},
		},
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/mine"}},
		},
	}

	resolveNamedPacks(cfg, "/city")

	// "packs/mine" doesn't match any key in Packs, so it stays as-is.
	if cfg.Rigs[0].Includes[0] != "packs/mine" {
		t.Errorf("Includes[0] = %q, want %q", cfg.Rigs[0].Includes[0], "packs/mine")
	}
}

func TestResolveNamedPacks_EmptyPacksMap(t *testing.T) {
	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/local"}},
		},
	}

	resolveNamedPacks(cfg, "/city")

	// No packs map — should be a no-op.
	if cfg.Rigs[0].Includes[0] != "packs/local" {
		t.Errorf("Includes[0] = %q, want %q", cfg.Rigs[0].Includes[0], "packs/local")
	}
}

func TestHasPackRigs(t *testing.T) {
	if HasPackRigs(nil) {
		t.Error("nil rigs should return false")
	}
	if HasPackRigs([]Rig{{Name: "a", Path: "/a"}}) {
		t.Error("rig without pack should return false")
	}
	if !HasPackRigs([]Rig{{Name: "a", Path: "/a", Includes: []string{"topo"}}}) {
		t.Error("rig with includes should return true")
	}
}

// The EffectiveCityPacks/EffectiveRigPacks helper functions have been
// removed — callers now access Workspace.Includes and Rig.Includes
// directly. The former tests were trivial pass-through validations.

// --- ExpandCityPacks (plural) tests ---

func TestExpandCityPacks_Multiple(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[[agent]]
name = "agent-a"
`)
	writeFile(t, dir, "packs/beta/pack.toml", `
[pack]
name = "beta"
schema = 1

[[agent]]
name = "agent-b"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{
			"packs/alpha", "packs/beta",
		}},
		Agents: []Agent{{Name: "existing"}},
	}

	dirs, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	// Should have 3 agents: agent-a, agent-b (from packs), then existing.
	if len(cfg.Agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "agent-a" {
		t.Errorf("first agent = %q, want agent-a", cfg.Agents[0].Name)
	}
	if cfg.Agents[1].Name != "agent-b" {
		t.Errorf("second agent = %q, want agent-b", cfg.Agents[1].Name)
	}
	if cfg.Agents[2].Name != "existing" {
		t.Errorf("third agent = %q, want existing", cfg.Agents[2].Name)
	}

	// No formulas configured → empty list.
	if len(dirs) != 0 {
		t.Errorf("formula dirs = %v, want empty", dirs)
	}
}

func TestExpandCityPacks_FormulaDirsStacked(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[formulas]
dir = "formulas"

[[agent]]
name = "agent-a"
`)
	writeFile(t, dir, "packs/alpha/formulas/mol-a.toml", "test")
	writeFile(t, dir, "packs/beta/pack.toml", `
[pack]
name = "beta"
schema = 1

[formulas]
dir = "formulas"

[[agent]]
name = "agent-b"
`)
	writeFile(t, dir, "packs/beta/formulas/mol-b.toml", "test")

	cfg := &City{
		Workspace: Workspace{Includes: []string{
			"packs/alpha", "packs/beta",
		}},
	}

	dirs, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	if len(dirs) != 2 {
		t.Fatalf("formula dirs = %d, want 2", len(dirs))
	}
	if dirs[0] != filepath.Join(dir, "packs/alpha/formulas") {
		t.Errorf("dirs[0] = %q, want alpha formulas", dirs[0])
	}
	if dirs[1] != filepath.Join(dir, "packs/beta/formulas") {
		t.Errorf("dirs[1] = %q, want beta formulas", dirs[1])
	}
}

func TestExpandCityPacks_Empty(t *testing.T) {
	cfg := &City{
		Agents: []Agent{{Name: "mayor"}},
	}

	dirs, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(dirs) != 0 {
		t.Errorf("formula dirs = %v, want empty", dirs)
	}
	if len(cfg.Agents) != 1 {
		t.Errorf("got %d agents, want 1 (unchanged)", len(cfg.Agents))
	}
}

func TestExpandCityPacks_BackwardCompat(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "mayor"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/gt"}},
	}

	dirs, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	if len(cfg.Agents) != 1 || cfg.Agents[0].Name != "mayor" {
		t.Errorf("agents = %v, want [mayor]", cfg.Agents)
	}
	if len(dirs) != 0 {
		t.Errorf("formula dirs = %v, want empty (no formulas)", dirs)
	}
}

func TestExpandCityPacks_ProvidersMerged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[providers.codex]
command = "codex"

[[agent]]
name = "agent-a"
`)
	writeFile(t, dir, "packs/beta/pack.toml", `
[pack]
name = "beta"
schema = 1

[providers.gemini]
command = "gemini"

[providers.codex]
command = "codex-from-beta"

[[agent]]
name = "agent-b"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{
			"packs/alpha", "packs/beta",
		}},
		Providers: map[string]ProviderSpec{
			"claude": {Command: "claude"},
		},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	// codex from alpha (first wins).
	if cfg.Providers["codex"].Command != "codex" {
		t.Errorf("codex command = %q, want codex (first wins)", cfg.Providers["codex"].Command)
	}
	// gemini from beta.
	if _, ok := cfg.Providers["gemini"]; !ok {
		t.Error("gemini provider should be merged from beta")
	}
	// claude unchanged.
	if cfg.Providers["claude"].Command != "claude" {
		t.Error("existing claude provider should not be overwritten")
	}
}

// --- ExpandPacks plural rig tests ---

func TestExpandPacks_MultiplePerRig(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[[agent]]
name = "worker-a"
`)
	writeFile(t, dir, "packs/beta/pack.toml", `
[pack]
name = "beta"
schema = 1

[[agent]]
name = "worker-b"
`)

	cfg := &City{
		Agents: []Agent{{Name: "mayor"}},
		Rigs: []Rig{
			{
				Name: "hw",
				Path: "/hw",
				Includes: []string{
					"packs/alpha", "packs/beta",
				},
			},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	if len(cfg.Agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("first agent = %q, want mayor", cfg.Agents[0].Name)
	}
	if cfg.Agents[1].Name != "worker-a" || cfg.Agents[1].Dir != "hw" {
		t.Errorf("second agent: name=%q dir=%q, want worker-a/hw", cfg.Agents[1].Name, cfg.Agents[1].Dir)
	}
	if cfg.Agents[2].Name != "worker-b" || cfg.Agents[2].Dir != "hw" {
		t.Errorf("third agent: name=%q dir=%q, want worker-b/hw", cfg.Agents[2].Name, cfg.Agents[2].Dir)
	}
}

func TestExpandPacks_RigFormulaDirsMultiple(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[formulas]
dir = "formulas"

[[agent]]
name = "worker-a"
`)
	writeFile(t, dir, "packs/alpha/formulas/mol-a.toml", "test")
	writeFile(t, dir, "packs/beta/pack.toml", `
[pack]
name = "beta"
schema = 1

[formulas]
dir = "formulas"

[[agent]]
name = "worker-b"
`)
	writeFile(t, dir, "packs/beta/formulas/mol-b.toml", "test")

	cfg := &City{
		Rigs: []Rig{
			{
				Name: "hw",
				Path: "/hw",
				Includes: []string{
					"packs/alpha", "packs/beta",
				},
			},
		},
	}

	rigFormulaDirs := make(map[string][]string)
	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, rigFormulaDirs); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	got := rigFormulaDirs["hw"]
	if len(got) != 2 {
		t.Fatalf("rigFormulaDirs[hw] = %d entries, want 2", len(got))
	}
	if got[0] != filepath.Join(dir, "packs/alpha/formulas") {
		t.Errorf("got[0] = %q, want alpha formulas", got[0])
	}
	if got[1] != filepath.Join(dir, "packs/beta/formulas") {
		t.Errorf("got[1] = %q, want beta formulas", got[1])
	}
}

// --- FormulaLayers plural tests ---

func TestFormulaLayers_MultipleCityAndRigTopoFormulas(t *testing.T) {
	rigTopoFormulas := map[string][]string{
		"hw": {"/city/packs/alpha/formulas", "/city/packs/beta/formulas"},
	}
	rigs := []Rig{
		{Name: "hw", Path: "/home/user/hw", FormulasDir: "local-formulas"},
	}

	fl := ComputeFormulaLayers(
		[]string{"/city/topo-a/formulas", "/city/topo-b/formulas"},
		"/city/formulas",
		rigTopoFormulas, rigs, "/city")

	// City layers: 2 topo + 1 local = 3.
	if len(fl.City) != 3 {
		t.Fatalf("City layers = %d, want 3", len(fl.City))
	}
	if fl.City[0] != "/city/topo-a/formulas" {
		t.Errorf("City[0] = %q", fl.City[0])
	}
	if fl.City[1] != "/city/topo-b/formulas" {
		t.Errorf("City[1] = %q", fl.City[1])
	}
	if fl.City[2] != "/city/formulas" {
		t.Errorf("City[2] = %q", fl.City[2])
	}

	// Rig "hw": 3 city + 2 rig topo + 1 rig local = 6.
	hwLayers := fl.Rigs["hw"]
	if len(hwLayers) != 6 {
		t.Fatalf("hw layers = %d, want 6", len(hwLayers))
	}
	if hwLayers[3] != "/city/packs/alpha/formulas" {
		t.Errorf("hw[3] = %q, want rig topo alpha", hwLayers[3])
	}
	if hwLayers[4] != "/city/packs/beta/formulas" {
		t.Errorf("hw[4] = %q, want rig topo beta", hwLayers[4])
	}
}

func TestExpandPacks_OverrideInstallAgentHooks(t *testing.T) {
	fs := fsys.NewFake()
	topoTOML := `[pack]
name = "test"
schema = 1

[[agent]]
name = "polecat"
install_agent_hooks = ["claude"]
`
	fs.Files["/city/packs/test/pack.toml"] = []byte(topoTOML)

	cfg := &City{
		Workspace: Workspace{Name: "test"},
		Rigs: []Rig{{
			Name:     "myrig",
			Path:     "/repo",
			Includes: []string{"packs/test"},
			Overrides: []AgentOverride{{
				Agent:             "polecat",
				InstallAgentHooks: []string{"gemini", "copilot"},
			}},
		}},
	}

	if err := ExpandPacks(cfg, fs, "/city", nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// Find the expanded agent.
	var found *Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "polecat" {
			found = &cfg.Agents[i]
			break
		}
	}
	if found == nil {
		t.Fatal("polecat agent not found after expansion")
	}
	if len(found.InstallAgentHooks) != 2 || found.InstallAgentHooks[0] != "gemini" {
		t.Errorf("InstallAgentHooks = %v, want [gemini copilot]", found.InstallAgentHooks)
	}
}

// --- City pack tests ---

func TestExpandCityPack_Basic(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "mayor"
prompt_template = "prompts/mayor.md"

[[agent]]
name = "deacon"
`)
	writeFile(t, dir, "packs/gastown/prompts/mayor.md", "you are the mayor")

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/gastown"}},
		Agents:    []Agent{{Name: "existing"}},
	}

	formulaDirs, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	// Should have 3 agents: mayor, deacon (from pack), then existing.
	if len(cfg.Agents) != 3 {
		t.Fatalf("got %d agents, want 3", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("first agent = %q, want mayor", cfg.Agents[0].Name)
	}
	if cfg.Agents[1].Name != "deacon" {
		t.Errorf("second agent = %q, want deacon", cfg.Agents[1].Name)
	}
	if cfg.Agents[2].Name != "existing" {
		t.Errorf("third agent = %q, want existing", cfg.Agents[2].Name)
	}

	// City pack agents should have dir="" (city-scoped).
	for _, a := range cfg.Agents[:2] {
		if a.Dir != "" {
			t.Errorf("city pack agent %q: dir = %q, want empty", a.Name, a.Dir)
		}
	}

	// No formulas configured → empty slice.
	if len(formulaDirs) != 0 {
		t.Errorf("formulaDirs = %v, want empty", formulaDirs)
	}
}

func TestExpandCityPack_FormulasDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[formulas]
dir = "formulas"

[[agent]]
name = "mayor"
`)
	writeFile(t, dir, "packs/gastown/formulas/mol-a.toml", "test formula")

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/gastown"}},
	}

	formulaDirs, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	want := filepath.Join(dir, "packs/gastown/formulas")
	if len(formulaDirs) != 1 || formulaDirs[0] != want {
		t.Errorf("formulaDirs = %v, want [%q]", formulaDirs, want)
	}
}

func TestExpandCityPack_NoPack(t *testing.T) {
	cfg := &City{
		Agents: []Agent{{Name: "mayor"}},
	}

	formulaDirs, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, "/tmp")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(formulaDirs) != 0 {
		t.Errorf("formulaDirs = %v, want empty", formulaDirs)
	}
	if len(cfg.Agents) != 1 {
		t.Errorf("got %d agents, want 1 (unchanged)", len(cfg.Agents))
	}
}

func TestExpandCityPack_ProvidersMerged(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[providers.codex]
command = "codex"

[[agent]]
name = "mayor"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/gt"}},
		Providers: map[string]ProviderSpec{
			"claude": {Command: "claude"},
		},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	if _, ok := cfg.Providers["codex"]; !ok {
		t.Error("codex provider should be merged from city pack")
	}
	if cfg.Providers["claude"].Command != "claude" {
		t.Error("existing claude provider should not be overwritten")
	}
}

// --- FormulaLayers tests ---

func TestFormulaLayers_CityOnly(t *testing.T) {
	fl := ComputeFormulaLayers([]string{"/city/topo/formulas"}, "/city/formulas", nil, nil, "/city")

	if len(fl.City) != 2 {
		t.Fatalf("City layers = %d, want 2", len(fl.City))
	}
	if fl.City[0] != "/city/topo/formulas" {
		t.Errorf("City[0] = %q, want city topo formulas", fl.City[0])
	}
	if fl.City[1] != "/city/formulas" {
		t.Errorf("City[1] = %q, want city local formulas", fl.City[1])
	}
	if len(fl.Rigs) != 0 {
		t.Errorf("Rigs = %d entries, want 0", len(fl.Rigs))
	}
}

func TestFormulaLayers_WithRigs(t *testing.T) {
	rigTopoFormulas := map[string][]string{
		"hw": {"/city/packs/gt/formulas"},
	}
	rigs := []Rig{
		{Name: "hw", Path: "/home/user/hw", FormulasDir: "local-formulas"},
	}

	fl := ComputeFormulaLayers([]string{"/city/topo/formulas"}, "/city/formulas", rigTopoFormulas, rigs, "/city")

	// City layers should be [city-topo, city-local].
	if len(fl.City) != 2 {
		t.Fatalf("City layers = %d, want 2", len(fl.City))
	}

	// Rig "hw" should have 4 layers.
	hwLayers := fl.Rigs["hw"]
	if len(hwLayers) != 4 {
		t.Fatalf("hw layers = %d, want 4", len(hwLayers))
	}
	if hwLayers[0] != "/city/topo/formulas" {
		t.Errorf("hw[0] = %q, want city topo", hwLayers[0])
	}
	if hwLayers[1] != "/city/formulas" {
		t.Errorf("hw[1] = %q, want city local", hwLayers[1])
	}
	if hwLayers[2] != "/city/packs/gt/formulas" {
		t.Errorf("hw[2] = %q, want rig topo", hwLayers[2])
	}
	// Layer 4: rig local formulas_dir resolved relative to city root.
	if hwLayers[3] != filepath.Join("/city", "local-formulas") {
		t.Errorf("hw[3] = %q, want rig local formulas", hwLayers[3])
	}
}

func TestFormulaLayers_RigLocalFormulasOnly(t *testing.T) {
	rigs := []Rig{
		{Name: "hw", Path: "/home/user/hw", FormulasDir: "formulas"},
	}

	fl := ComputeFormulaLayers(nil, "", nil, rigs, "/city")

	// City should have no layers (no pack, no local).
	if len(fl.City) != 0 {
		t.Errorf("City layers = %d, want 0", len(fl.City))
	}

	// Rig should have just the local layer.
	hwLayers := fl.Rigs["hw"]
	if len(hwLayers) != 1 {
		t.Fatalf("hw layers = %d, want 1", len(hwLayers))
	}
	if hwLayers[0] != filepath.Join("/city", "formulas") {
		t.Errorf("hw[0] = %q, want rig local formulas", hwLayers[0])
	}
}

func TestFormulaLayers_NoFormulas(t *testing.T) {
	rigs := []Rig{
		{Name: "hw", Path: "/home/user/hw"},
	}

	fl := ComputeFormulaLayers(nil, "", nil, rigs, "/city")

	if len(fl.City) != 0 {
		t.Errorf("City layers = %d, want 0", len(fl.City))
	}
	// Rig with no formula sources should not appear in map.
	if _, ok := fl.Rigs["hw"]; ok {
		t.Error("hw should not appear in Rigs (no formula layers)")
	}
}

func TestExpandPacks_FormulaDirsRecorded(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[formulas]
dir = "formulas"

[[agent]]
name = "witness"
`)
	writeFile(t, dir, "packs/gt/formulas/mol-a.toml", "test")

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/home/user/hw", Includes: []string{"packs/gt"}},
		},
	}

	rigFormulaDirs := make(map[string][]string)
	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, rigFormulaDirs); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	want := filepath.Join(dir, "packs/gt/formulas")
	if got := rigFormulaDirs["hw"]; len(got) != 1 || got[0] != want {
		t.Errorf("rigFormulaDirs[hw] = %v, want [%q]", got, want)
	}
}

func TestExpandPacks_SourceDirSet(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "witness"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	wantDir := filepath.Join(dir, "packs/gt")
	if cfg.Agents[0].SourceDir != wantDir {
		t.Errorf("SourceDir = %q, want %q", cfg.Agents[0].SourceDir, wantDir)
	}
}

func TestExpandPacks_SessionSetupScriptAdjusted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "witness"
session_setup_script = "scripts/setup.sh"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// session_setup_script should be adjusted relative to pack dir → city root.
	want := "packs/gt/scripts/setup.sh"
	if cfg.Agents[0].SessionSetupScript != want {
		t.Errorf("SessionSetupScript = %q, want %q", cfg.Agents[0].SessionSetupScript, want)
	}
}

func TestExpandCityPack_SourceDirSet(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "mayor"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/gastown"}},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	wantDir := filepath.Join(dir, "packs/gastown")
	if cfg.Agents[0].SourceDir != wantDir {
		t.Errorf("SourceDir = %q, want %q", cfg.Agents[0].SourceDir, wantDir)
	}
}

func TestExpandPacks_OverlayDirAdjusted(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gt/pack.toml", `
[pack]
name = "gastown"
version = "1.0.0"
schema = 1

[[agent]]
name = "witness"
overlay_dir = "overlays/worker"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/hw", Includes: []string{"packs/gt"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// overlay_dir should be adjusted relative to pack dir → city root.
	want := "packs/gt/overlays/worker"
	if cfg.Agents[0].OverlayDir != want {
		t.Errorf("OverlayDir = %q, want %q", cfg.Agents[0].OverlayDir, want)
	}
}

func TestExpandCityPackFilters(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/combined/pack.toml", `
[pack]
name = "combined"
schema = 1

[[agent]]
name = "mayor"
scope = "city"
prompt_template = "prompts/mayor.md"

[[agent]]
name = "deacon"
scope = "city"

[[agent]]
name = "witness"
scope = "rig"
prompt_template = "prompts/witness.md"

[[agent]]
name = "polecat"
scope = "rig"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/combined"}},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	// Should only have city agents (mayor, deacon).
	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(cfg.Agents))
	}
	names := make(map[string]bool)
	for _, a := range cfg.Agents {
		names[a.Name] = true
		if a.Dir != "" {
			t.Errorf("city agent %q: dir = %q, want empty", a.Name, a.Dir)
		}
	}
	if !names["mayor"] || !names["deacon"] {
		t.Errorf("agents = %v, want mayor and deacon", names)
	}
	if names["witness"] || names["polecat"] {
		t.Error("rig agents should be filtered out of city pack expansion")
	}
}

func TestExpandPacksFilters(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/combined/pack.toml", `
[pack]
name = "combined"
schema = 1

[[agent]]
name = "mayor"
scope = "city"

[[agent]]
name = "deacon"
scope = "city"

[[agent]]
name = "witness"
scope = "rig"
prompt_template = "prompts/witness.md"

[[agent]]
name = "polecat"
scope = "rig"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/home/user/hw", Includes: []string{"packs/combined"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// Should only have rig agents (witness, polecat), not city agents.
	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(cfg.Agents))
	}
	names := make(map[string]bool)
	for _, a := range cfg.Agents {
		names[a.Name] = true
		if a.Dir != "hw" {
			t.Errorf("rig agent %q: dir = %q, want %q", a.Name, a.Dir, "hw")
		}
	}
	if !names["witness"] || !names["polecat"] {
		t.Errorf("agents = %v, want witness and polecat", names)
	}
	if names["mayor"] || names["deacon"] {
		t.Error("city agents should be filtered out of rig pack expansion")
	}
}

func TestExpandCityPackNoScope(t *testing.T) {
	// When scope is not set, all agents are unscoped (included in both city and rig).
	dir := t.TempDir()
	writeFile(t, dir, "packs/simple/pack.toml", `
[pack]
name = "simple"
schema = 1

[[agent]]
name = "alpha"

[[agent]]
name = "beta"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/simple"}},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2 (all unscoped agents included)", len(cfg.Agents))
	}
}

func TestExpandPacks_DuplicateAgentCollision(t *testing.T) {
	// Two rig packs defining the same agent name should produce
	// a provenance-aware error naming both pack directories.
	dir := t.TempDir()
	writeFile(t, dir, "packs/base/pack.toml", `
[pack]
name = "base"
schema = 1

[[agent]]
name = "worker"
`)
	writeFile(t, dir, "packs/extras/pack.toml", `
[pack]
name = "extras"
schema = 1

[[agent]]
name = "worker"
`)

	cfg := &City{
		Rigs: []Rig{{
			Name:     "myrig",
			Path:     "/tmp/myrig",
			Includes: []string{"packs/base", "packs/extras"},
		}},
	}

	err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil)
	if err == nil {
		t.Fatal("expected error for duplicate agent across rig packs")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "duplicate agent") {
		t.Errorf("error should mention 'duplicate agent', got: %s", errStr)
	}
	if !strings.Contains(errStr, "myrig") {
		t.Errorf("error should mention rig name 'myrig', got: %s", errStr)
	}
	if !strings.Contains(errStr, "packs/base") {
		t.Errorf("error should mention first pack dir, got: %s", errStr)
	}
	if !strings.Contains(errStr, "packs/extras") {
		t.Errorf("error should mention second pack dir, got: %s", errStr)
	}
}

func TestExpandCityPacks_DuplicateAgentCollision(t *testing.T) {
	// Two city packs defining the same agent name should produce
	// a provenance-aware error.
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[[agent]]
name = "overseer"
`)
	writeFile(t, dir, "packs/beta/pack.toml", `
[pack]
name = "beta"
schema = 1

[[agent]]
name = "overseer"
`)

	cfg := &City{
		Workspace: Workspace{
			Includes: []string{"packs/alpha", "packs/beta"},
		},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err == nil {
		t.Fatal("expected error for duplicate agent across city packs")
	}
	errStr := err.Error()
	if !strings.Contains(errStr, "duplicate agent") {
		t.Errorf("error should mention 'duplicate agent', got: %s", errStr)
	}
	if !strings.Contains(errStr, "city") {
		t.Errorf("error should mention 'city' scope, got: %s", errStr)
	}
	if !strings.Contains(errStr, "packs/alpha") {
		t.Errorf("error should mention first pack dir, got: %s", errStr)
	}
	if !strings.Contains(errStr, "packs/beta") {
		t.Errorf("error should mention second pack dir, got: %s", errStr)
	}
}

func TestExpandPacks_DifferentNamesNoCollision(t *testing.T) {
	// Two rig packs with different agent names should compose without error.
	dir := t.TempDir()
	writeFile(t, dir, "packs/base/pack.toml", `
[pack]
name = "base"
schema = 1

[[agent]]
name = "worker"
`)
	writeFile(t, dir, "packs/extras/pack.toml", `
[pack]
name = "extras"
schema = 1

[[agent]]
name = "reviewer"
`)

	cfg := &City{
		Rigs: []Rig{{
			Name:     "myrig",
			Path:     "/tmp/myrig",
			Includes: []string{"packs/base", "packs/extras"},
		}},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("unexpected error for different-named agents: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(cfg.Agents))
	}
}

func TestExpandPacks_SamePackDifferentRigsNoCollision(t *testing.T) {
	// Same pack applied to two different rigs should not collide
	// (different dir scope).
	dir := t.TempDir()
	writeFile(t, dir, "packs/shared/pack.toml", `
[pack]
name = "shared"
schema = 1

[[agent]]
name = "worker"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "rig-a", Path: "/tmp/a", Includes: []string{"packs/shared"}},
			{Name: "rig-b", Path: "/tmp/b", Includes: []string{"packs/shared"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("unexpected error for same pack on different rigs: %v", err)
	}
	if len(cfg.Agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(cfg.Agents))
	}
	if cfg.Agents[0].Dir != "rig-a" || cfg.Agents[1].Dir != "rig-b" {
		t.Errorf("agents should have different dirs: %q, %q", cfg.Agents[0].Dir, cfg.Agents[1].Dir)
	}
}

// --- Pack includes tests ---

func TestPackIncludes(t *testing.T) {
	dir := t.TempDir()

	// maintenance pack: defines "dog" agent.
	writeFile(t, dir, "packs/maintenance/pack.toml", `
[pack]
name = "maintenance"
schema = 1

[[agent]]
name = "dog"
`)

	// gastown pack: includes maintenance, defines "mayor".
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 1
includes = ["../maintenance"]

[[agent]]
name = "mayor"
`)

	agents, _, _, _, _, _, _, err := loadPack(
		fsys.OSFS{},
		filepath.Join(dir, "packs/gastown/pack.toml"),
		filepath.Join(dir, "packs/gastown"),
		dir, "", nil)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}

	// Should have 2 agents: dog (from include, first) then mayor (parent).
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}
	if agents[0].Name != "dog" {
		t.Errorf("agents[0].Name = %q, want dog (from include)", agents[0].Name)
	}
	if agents[1].Name != "mayor" {
		t.Errorf("agents[1].Name = %q, want mayor (from parent)", agents[1].Name)
	}
}

func TestPackIncludesScope(t *testing.T) {
	dir := t.TempDir()

	// maintenance pack: defines "dog" with scope="city".
	writeFile(t, dir, "packs/maintenance/pack.toml", `
[pack]
name = "maintenance"
schema = 1

[[agent]]
name = "dog"
scope = "city"
`)

	// gastown pack: includes maintenance, mayor is scope="city".
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 1
includes = ["../maintenance"]

[[agent]]
name = "mayor"
scope = "city"
`)

	agents, _, _, _, _, _, _, err := loadPack(
		fsys.OSFS{},
		filepath.Join(dir, "packs/gastown/pack.toml"),
		filepath.Join(dir, "packs/gastown"),
		dir, "", nil)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}

	// scope="city" on each agent: dog and mayor should be city-scoped.
	cityScoped := make(map[string]bool)
	for _, a := range agents {
		if a.Scope == "city" {
			cityScoped[a.Name] = true
		}
	}
	if !cityScoped["dog"] || !cityScoped["mayor"] {
		scopes := make(map[string]string)
		for _, a := range agents {
			scopes[a.Name] = a.Scope
		}
		t.Errorf("expected dog and mayor to be city-scoped, got scopes: %v", scopes)
	}
}

func TestExpandCityPacks_IncludesCityScopedNamedSessions(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "mayor"
scope = "city"

[[named_session]]
template = "mayor"
scope = "city"

[[agent]]
name = "witness"
scope = "rig"

[[named_session]]
template = "witness"
scope = "rig"
`)

	cfg := &City{
		Workspace: Workspace{
			Name:     "test-city",
			Includes: []string{"packs/gastown"},
		},
	}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	if len(cfg.NamedSessions) != 1 {
		t.Fatalf("NamedSessions = %d, want 1", len(cfg.NamedSessions))
	}
	if got := cfg.NamedSessions[0].QualifiedName(); got != "mayor" {
		t.Fatalf("NamedSessions[0] = %q, want mayor", got)
	}
}

func TestExpandPacks_IncludesRigScopedNamedSessions(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "mayor"
scope = "city"

[[named_session]]
template = "mayor"
scope = "city"

[[agent]]
name = "witness"
scope = "rig"

[[named_session]]
template = "witness"
scope = "rig"
`)

	cfg := &City{
		Workspace: Workspace{Name: "test-city"},
		Rigs: []Rig{
			{Name: "frontend", Path: filepath.Join(dir, "frontend"), Includes: []string{"packs/gastown"}},
		},
	}
	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	if len(cfg.NamedSessions) != 1 {
		t.Fatalf("NamedSessions = %d, want 1", len(cfg.NamedSessions))
	}
	if got := cfg.NamedSessions[0].QualifiedName(); got != "frontend/witness" {
		t.Fatalf("NamedSessions[0] = %q, want frontend/witness", got)
	}
}

func TestPackIncludesFormulas(t *testing.T) {
	dir := t.TempDir()

	// maintenance pack with formulas.
	writeFile(t, dir, "packs/maintenance/pack.toml", `
[pack]
name = "maintenance"
schema = 1

[formulas]
dir = "formulas"

[[agent]]
name = "dog"
`)
	writeFile(t, dir, "packs/maintenance/formulas/.keep", "")

	// gastown pack with formulas, includes maintenance.
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 1
includes = ["../maintenance"]

[formulas]
dir = "formulas"

[[agent]]
name = "mayor"
`)
	writeFile(t, dir, "packs/gastown/formulas/.keep", "")

	_, _, _, _, topoDirs, _, _, err := loadPack(
		fsys.OSFS{},
		filepath.Join(dir, "packs/gastown/pack.toml"),
		filepath.Join(dir, "packs/gastown"),
		dir, "", nil)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}

	// Should have 2 pack dirs: maintenance first (included), then gastown (parent).
	if len(topoDirs) != 2 {
		t.Fatalf("got %d topoDirs, want 2: %v", len(topoDirs), topoDirs)
	}
	if !strings.Contains(topoDirs[0], "maintenance") {
		t.Errorf("topoDirs[0] = %q, want maintenance pack dir", topoDirs[0])
	}
	if !strings.Contains(topoDirs[1], "gastown") {
		t.Errorf("topoDirs[1] = %q, want gastown pack dir", topoDirs[1])
	}
}

func TestPackIncludesCycle(t *testing.T) {
	dir := t.TempDir()

	// A includes B, B includes A → cycle.
	writeFile(t, dir, "packs/a/pack.toml", `
[pack]
name = "a"
schema = 1
includes = ["../b"]

[[agent]]
name = "alpha"
`)
	writeFile(t, dir, "packs/b/pack.toml", `
[pack]
name = "b"
schema = 1
includes = ["../a"]

[[agent]]
name = "beta"
`)

	_, _, _, _, _, _, _, err := loadPack(
		fsys.OSFS{},
		filepath.Join(dir, "packs/a/pack.toml"),
		filepath.Join(dir, "packs/a"),
		dir, "", nil)
	if err == nil {
		t.Fatal("expected cycle detection error")
	}
	if !strings.Contains(err.Error(), "cycle") {
		t.Errorf("error = %v, want to contain 'cycle'", err)
	}
}

func TestPackIncludesNotFound(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, dir, "packs/main/pack.toml", `
[pack]
name = "main"
schema = 1
includes = ["../nonexistent"]

[[agent]]
name = "alpha"
`)

	_, _, _, _, _, _, _, err := loadPack(
		fsys.OSFS{},
		filepath.Join(dir, "packs/main/pack.toml"),
		filepath.Join(dir, "packs/main"),
		dir, "", nil)
	if err == nil {
		t.Fatal("expected error for missing include")
	}
	if !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("error = %v, want to contain 'nonexistent'", err)
	}
}

func TestPackIncludesProviderMerge(t *testing.T) {
	dir := t.TempDir()

	// Included pack defines provider "claude".
	writeFile(t, dir, "packs/base/pack.toml", `
[pack]
name = "base"
schema = 1

[providers.claude]
command = "base-claude"

[[agent]]
name = "worker"
`)

	// Parent pack also defines "claude" — parent wins.
	writeFile(t, dir, "packs/main/pack.toml", `
[pack]
name = "main"
schema = 1
includes = ["../base"]

[providers.claude]
command = "main-claude"

[[agent]]
name = "boss"
`)

	_, _, providers, _, _, _, _, err := loadPack(
		fsys.OSFS{},
		filepath.Join(dir, "packs/main/pack.toml"),
		filepath.Join(dir, "packs/main"),
		dir, "", nil)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}

	if providers["claude"].Command != "main-claude" {
		t.Errorf("provider claude = %q, want main-claude (parent wins)", providers["claude"].Command)
	}
}

func TestExpandCityPacksWithIncludes(t *testing.T) {
	dir := t.TempDir()

	// maintenance pack.
	writeFile(t, dir, "packs/maintenance/pack.toml", `
[pack]
name = "maintenance"
schema = 1

[formulas]
dir = "formulas"

[[agent]]
name = "dog"
scope = "city"
`)
	writeFile(t, dir, "packs/maintenance/formulas/.keep", "")

	// gastown pack includes maintenance.
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 1
includes = ["../maintenance"]

[formulas]
dir = "formulas"

[[agent]]
name = "mayor"
scope = "city"

[[agent]]
name = "witness"
scope = "rig"
`)
	writeFile(t, dir, "packs/gastown/formulas/.keep", "")

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/gastown"}},
	}
	formulaDirs, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	// scope="city" agents included, scope="rig" witness filtered out.
	agentNames := make(map[string]bool)
	for _, a := range cfg.Agents {
		agentNames[a.Name] = true
	}
	if !agentNames["dog"] {
		t.Error("expected dog agent (from included maintenance)")
	}
	if !agentNames["mayor"] {
		t.Error("expected mayor agent (from gastown)")
	}
	if agentNames["witness"] {
		t.Error("witness should be filtered out (rig-scoped)")
	}

	// Formula dirs: maintenance then gastown.
	if len(formulaDirs) != 2 {
		t.Fatalf("got %d formulaDirs, want 2: %v", len(formulaDirs), formulaDirs)
	}
}

func TestPackDirsCollected(t *testing.T) {
	tmp := t.TempDir()

	// Create a pack with a prompts/shared/ directory.
	topoDir := filepath.Join(tmp, "packs", "alpha")
	writeFile(t, topoDir, "pack.toml", `
[pack]
name = "alpha"
schema = 1

[[agent]]
name = "worker"
prompt_template = "prompts/worker.template.md"
`)
	writeFile(t, filepath.Join(topoDir, "prompts"), "worker.template.md", "Worker prompt")
	writeFile(t, filepath.Join(topoDir, "prompts", "shared"), "common.template.md",
		`{{ define "common" }}shared content{{ end }}`)

	writeFile(t, tmp, "city.toml", `
[workspace]
name = "test"
includes = ["packs/alpha"]
`)

	cfg, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(tmp, "city.toml"))
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}

	if len(cfg.PackDirs) == 0 {
		t.Fatal("PackDirs is empty, want at least one entry")
	}

	found := false
	for _, d := range cfg.PackDirs {
		if strings.HasSuffix(d, filepath.Join("packs", "alpha")) {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("PackDirs = %v, want entry ending with packs/alpha", cfg.PackDirs)
	}
}

// ---------------------------------------------------------------------------
// Scope field tests
// ---------------------------------------------------------------------------

func TestLoadPack_ScopeField(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/test/pack.toml", `
[pack]
name = "test"
schema = 1

[[agent]]
name = "mayor"
scope = "city"
prompt_template = "prompts/mayor.md"

[[agent]]
name = "polecat"
scope = "rig"
`)

	agents, _, _, _, _, _, _, err := loadPack(
		fsys.OSFS{}, filepath.Join(dir, "packs/test/pack.toml"),
		filepath.Join(dir, "packs/test"), dir, "myrig", nil)
	if err != nil {
		t.Fatalf("loadPack: %v", err)
	}

	// Both agents should be in the returned list.
	if len(agents) != 2 {
		t.Fatalf("got %d agents, want 2", len(agents))
	}
	// scope is preserved on each agent.
	for _, a := range agents {
		switch a.Name {
		case "mayor":
			if a.Scope != "city" {
				t.Errorf("mayor scope = %q, want city", a.Scope)
			}
		case "polecat":
			if a.Scope != "rig" {
				t.Errorf("polecat scope = %q, want rig", a.Scope)
			}
		}
	}
}

func TestExpandCityPacks_ScopeFiltering(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/test/pack.toml", `
[pack]
name = "test"
schema = 1

[[agent]]
name = "mayor"
scope = "city"

[[agent]]
name = "polecat"
scope = "rig"
`)

	cfg := &City{
		Workspace: Workspace{
			Includes: []string{"packs/test"},
		},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	// Only scope="city" agents should be kept.
	if len(cfg.Agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("agent name = %q, want mayor", cfg.Agents[0].Name)
	}
}

func TestExpandPacks_ScopeExcludesCity(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/test/pack.toml", `
[pack]
name = "test"
schema = 1

[[agent]]
name = "mayor"
scope = "city"

[[agent]]
name = "polecat"
scope = "rig"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "myrig", Path: "/tmp/myrig", Includes: []string{"packs/test"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// Only scope="rig" agents should be kept (scope="city" excluded).
	if len(cfg.Agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "polecat" {
		t.Errorf("agent name = %q, want polecat", cfg.Agents[0].Name)
	}
}

// ---------------------------------------------------------------------------
// Workspace/Rig Includes tests
// ---------------------------------------------------------------------------

func TestHasPackRigs_Includes(t *testing.T) {
	rigs := []Rig{
		{Name: "test", Path: "/test", Includes: []string{"packs/alpha"}},
	}
	if !HasPackRigs(rigs) {
		t.Error("HasPackRigs = false, want true for rig with includes")
	}
}

func TestExpandCityPacks_ViaIncludes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/test/pack.toml", `
[pack]
name = "test"
schema = 1

[[agent]]
name = "mayor"
`)

	cfg := &City{
		Workspace: Workspace{
			Includes: []string{"packs/test"},
		},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "mayor" {
		t.Errorf("agent name = %q, want mayor", cfg.Agents[0].Name)
	}
}

func TestExpandPacks_ViaRigIncludes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/test/pack.toml", `
[pack]
name = "test"
schema = 1

[[agent]]
name = "polecat"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "myrig", Path: "/tmp/myrig", Includes: []string{"packs/test"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Dir != "myrig" {
		t.Errorf("agent dir = %q, want myrig", cfg.Agents[0].Dir)
	}
}

// --- pack.requires tests ---

func TestPackRequires_CitySatisfied(t *testing.T) {
	dir := t.TempDir()

	// provider pack provides "dog" agent
	writeFile(t, dir, "packs/provider/pack.toml", `
[pack]
name = "provider"
schema = 1

[[agent]]
name = "dog"
scope = "city"
`)
	// consumer pack requires "dog" agent
	writeFile(t, dir, "packs/consumer/pack.toml", `
[pack]
name = "consumer"
schema = 1
includes = ["../provider"]

[[pack.requires]]
scope = "city"
agent = "dog"

[[agent]]
name = "worker"
scope = "city"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/consumer"}},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	// Should have 2 city agents: dog (from provider) + worker (from consumer).
	if len(cfg.Agents) != 2 {
		t.Errorf("got %d agents, want 2", len(cfg.Agents))
	}
}

func TestPackRequires_CityUnsatisfied(t *testing.T) {
	dir := t.TempDir()

	// Pack requires "dog" but no pack provides it.
	writeFile(t, dir, "packs/consumer/pack.toml", `
[pack]
name = "consumer"
schema = 1

[[pack.requires]]
scope = "city"
agent = "dog"

[[agent]]
name = "worker"
scope = "city"
`)

	// Use LoadWithIncludes to trigger the city requirement validation.
	writeFile(t, dir, "city.toml", `
[workspace]
name = "test"
includes = ["packs/consumer"]
`)
	_, _, err := LoadWithIncludes(fsys.OSFS{}, filepath.Join(dir, "city.toml"))
	if err == nil {
		t.Fatal("expected error for unsatisfied city requirement, got nil")
	}
	if !strings.Contains(err.Error(), "requires city agent") {
		t.Errorf("error = %q, want mention of requires city agent", err.Error())
	}
	if !strings.Contains(err.Error(), "dog") {
		t.Errorf("error = %q, want mention of dog", err.Error())
	}
}

func TestPackRequires_RigSatisfied(t *testing.T) {
	dir := t.TempDir()

	// provider pack provides "helper" agent
	writeFile(t, dir, "packs/provider/pack.toml", `
[pack]
name = "provider"
schema = 1

[[agent]]
name = "helper"
scope = "rig"
`)
	// consumer pack requires "helper" agent at rig scope
	writeFile(t, dir, "packs/consumer/pack.toml", `
[pack]
name = "consumer"
schema = 1
includes = ["../provider"]

[[pack.requires]]
scope = "rig"
agent = "helper"

[[agent]]
name = "worker"
scope = "rig"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "myrig", Path: "/tmp/myrig", Includes: []string{"packs/consumer"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// Should have 2 rig agents: helper + worker.
	if len(cfg.Agents) != 2 {
		t.Errorf("got %d agents, want 2", len(cfg.Agents))
	}
}

func TestPackRequires_RigUnsatisfied(t *testing.T) {
	dir := t.TempDir()

	// Pack requires rig agent "helper" but no pack provides it.
	writeFile(t, dir, "packs/consumer/pack.toml", `
[pack]
name = "consumer"
schema = 1

[[pack.requires]]
scope = "rig"
agent = "helper"

[[agent]]
name = "worker"
scope = "rig"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "myrig", Path: "/tmp/myrig", Includes: []string{"packs/consumer"}},
		},
	}

	err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil)
	if err == nil {
		t.Fatal("expected error for unsatisfied rig requirement, got nil")
	}
	if !strings.Contains(err.Error(), "requires rig agent") {
		t.Errorf("error = %q, want mention of requires rig agent", err.Error())
	}
	if !strings.Contains(err.Error(), "helper") {
		t.Errorf("error = %q, want mention of helper", err.Error())
	}
}

func TestPackRequires_InvalidScope(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, dir, "packs/bad/pack.toml", `
[pack]
name = "bad"
schema = 1

[[pack.requires]]
scope = "invalid"
agent = "dog"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/bad"}},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err == nil {
		t.Fatal("expected error for invalid scope, got nil")
	}
	if !strings.Contains(err.Error(), "scope must be") {
		t.Errorf("error = %q, want mention of scope", err.Error())
	}
}

func TestPackRequires_MissingAgent(t *testing.T) {
	dir := t.TempDir()

	writeFile(t, dir, "packs/bad/pack.toml", `
[pack]
name = "bad"
schema = 1

[[pack.requires]]
scope = "city"
agent = ""
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/bad"}},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err == nil {
		t.Fatal("expected error for empty agent, got nil")
	}
	if !strings.Contains(err.Error(), "agent is required") {
		t.Errorf("error = %q, want mention of agent required", err.Error())
	}
}

// ---------------------------------------------------------------------------
// Fallback agent tests
// ---------------------------------------------------------------------------

func TestFallbackAgent_NonFallbackWins(t *testing.T) {
	// Non-fallback dog from pack A, fallback dog from pack B.
	// Only A's dog should survive.
	dir := t.TempDir()
	writeFile(t, dir, "packs/maintenance/pack.toml", `
[pack]
name = "maintenance"
schema = 1

[[agent]]
name = "dog"
scope = "city"
nudge = "full dog"
`)
	writeFile(t, dir, "packs/dolt/pack.toml", `
[pack]
name = "dolt"
schema = 1

[[agent]]
name = "dog"
scope = "city"
fallback = true
nudge = "fallback dog"
`)

	cfg := &City{
		Workspace: Workspace{
			Includes: []string{"packs/maintenance", "packs/dolt"},
		},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	// Only the non-fallback dog should remain.
	var dogs []Agent
	for _, a := range cfg.Agents {
		if a.Name == "dog" {
			dogs = append(dogs, a)
		}
	}
	if len(dogs) != 1 {
		t.Fatalf("got %d dogs, want 1", len(dogs))
	}
	if dogs[0].Nudge != "full dog" {
		t.Errorf("surviving dog nudge = %q, want %q", dogs[0].Nudge, "full dog")
	}
}

func TestFallbackAgent_BothFallback_FirstWins(t *testing.T) {
	// Two fallback dogs from different packs. First loaded wins.
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[[agent]]
name = "dog"
scope = "city"
fallback = true
nudge = "alpha dog"
`)
	writeFile(t, dir, "packs/beta/pack.toml", `
[pack]
name = "beta"
schema = 1

[[agent]]
name = "dog"
scope = "city"
fallback = true
nudge = "beta dog"
`)

	cfg := &City{
		Workspace: Workspace{
			Includes: []string{"packs/alpha", "packs/beta"},
		},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	var dogs []Agent
	for _, a := range cfg.Agents {
		if a.Name == "dog" {
			dogs = append(dogs, a)
		}
	}
	if len(dogs) != 1 {
		t.Fatalf("got %d dogs, want 1", len(dogs))
	}
	if dogs[0].Nudge != "alpha dog" {
		t.Errorf("surviving dog nudge = %q, want %q (first loaded wins)", dogs[0].Nudge, "alpha dog")
	}
}

func TestFallbackAgent_NeitherFallback_CollisionError(t *testing.T) {
	// Two non-fallback dogs from different packs. Should still error.
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1

[[agent]]
name = "dog"
scope = "city"
`)
	writeFile(t, dir, "packs/beta/pack.toml", `
[pack]
name = "beta"
schema = 1

[[agent]]
name = "dog"
scope = "city"
`)

	cfg := &City{
		Workspace: Workspace{
			Includes: []string{"packs/alpha", "packs/beta"},
		},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err == nil {
		t.Fatal("expected collision error for two non-fallback dogs")
	}
	if !strings.Contains(err.Error(), "duplicate agent") {
		t.Errorf("error = %q, want 'duplicate agent'", err.Error())
	}
}

func TestFallbackAgent_StandaloneWorks(t *testing.T) {
	// Single fallback agent, no collision — should be kept normally.
	dir := t.TempDir()
	writeFile(t, dir, "packs/health/pack.toml", `
[pack]
name = "health"
schema = 1

[[agent]]
name = "dog"
scope = "city"
fallback = true
nudge = "standalone fallback"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/health"}},
	}

	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	if len(cfg.Agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(cfg.Agents))
	}
	if cfg.Agents[0].Name != "dog" {
		t.Errorf("agent name = %q, want dog", cfg.Agents[0].Name)
	}
	if !cfg.Agents[0].Fallback {
		t.Error("agent should have Fallback = true")
	}
}

func TestExpandPacks_OverrideAppendAlone(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/test/pack.toml", `
[pack]
name = "test"
schema = 1

[[agent]]
name = "polecat"
pre_start = ["base-setup.sh"]
session_setup = ["tmux set status"]
install_agent_hooks = ["claude"]
inject_fragments = ["tdd"]
`)
	cfg := &City{
		Rigs: []Rig{{
			Name: "hw", Path: "/tmp/hw", Includes: []string{"packs/test"},
			Overrides: []AgentOverride{{
				Agent:                   "polecat",
				PreStartAppend:          []string{"extra-setup.sh"},
				SessionSetupAppend:      []string{"tmux set mouse on"},
				InstallAgentHooksAppend: []string{"gemini"},
				InjectFragmentsAppend:   []string{"safety"},
			}},
		}},
	}
	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}
	a := cfg.Agents[0]
	wantPreStart := []string{"base-setup.sh", "extra-setup.sh"}
	if !sliceEqual(a.PreStart, wantPreStart) {
		t.Errorf("PreStart = %v, want %v", a.PreStart, wantPreStart)
	}
	wantSetup := []string{"tmux set status", "tmux set mouse on"}
	if !sliceEqual(a.SessionSetup, wantSetup) {
		t.Errorf("SessionSetup = %v, want %v", a.SessionSetup, wantSetup)
	}
	wantHooks := []string{"claude", "gemini"}
	if !sliceEqual(a.InstallAgentHooks, wantHooks) {
		t.Errorf("InstallAgentHooks = %v, want %v", a.InstallAgentHooks, wantHooks)
	}
	wantFragments := []string{"tdd", "safety"}
	if !sliceEqual(a.InjectFragments, wantFragments) {
		t.Errorf("InjectFragments = %v, want %v", a.InjectFragments, wantFragments)
	}
}

func TestExpandPacks_OverrideReplacePlusAppend(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/test/pack.toml", `
[pack]
name = "test"
schema = 1

[[agent]]
name = "polecat"
pre_start = ["old-a.sh", "old-b.sh"]
`)
	cfg := &City{
		Rigs: []Rig{{
			Name: "hw", Path: "/tmp/hw", Includes: []string{"packs/test"},
			Overrides: []AgentOverride{{
				Agent:          "polecat",
				PreStart:       []string{"new-base.sh"},
				PreStartAppend: []string{"extra.sh"},
			}},
		}},
	}
	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}
	want := []string{"new-base.sh", "extra.sh"}
	if !sliceEqual(cfg.Agents[0].PreStart, want) {
		t.Errorf("PreStart = %v, want %v", cfg.Agents[0].PreStart, want)
	}
}

func TestExpandPacks_OverrideAppendToEmptyBase(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/test/pack.toml", `
[pack]
name = "test"
schema = 1

[[agent]]
name = "polecat"
`)
	cfg := &City{
		Rigs: []Rig{{
			Name: "hw", Path: "/tmp/hw", Includes: []string{"packs/test"},
			Overrides: []AgentOverride{{
				Agent:              "polecat",
				PreStartAppend:     []string{"setup.sh"},
				SessionSetupAppend: []string{"tmux set mouse on"},
			}},
		}},
	}
	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}
	a := cfg.Agents[0]
	if !sliceEqual(a.PreStart, []string{"setup.sh"}) {
		t.Errorf("PreStart = %v, want [setup.sh]", a.PreStart)
	}
	if !sliceEqual(a.SessionSetup, []string{"tmux set mouse on"}) {
		t.Errorf("SessionSetup = %v, want [tmux set mouse on]", a.SessionSetup)
	}
}

// --- Pack-level patches tests ---

func TestPackLevelPatches_Agent(t *testing.T) {
	dir := t.TempDir()
	// Base pack with one agent.
	writeFile(t, dir, "packs/base/pack.toml", `
[pack]
name = "base"
schema = 1

[[agent]]
name = "worker"
nudge = "do work"
`)
	// Overlay pack includes base and patches the agent's session_setup_script.
	writeFile(t, dir, "packs/overlay/pack.toml", `
[pack]
name = "overlay"
schema = 1
includes = ["../base"]

[[patches.agent]]
name = "worker"
session_setup_script = "scripts/theme.sh"
`)
	writeFile(t, dir, "packs/overlay/scripts/theme.sh", "#!/bin/sh\necho themed")

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/overlay"}},
	}
	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	if len(cfg.Agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(cfg.Agents))
	}
	a := cfg.Agents[0]
	if a.Name != "worker" {
		t.Errorf("name = %q, want worker", a.Name)
	}
	// session_setup_script should be set (resolved path).
	if a.SessionSetupScript == "" {
		t.Fatal("SessionSetupScript not set by patch")
	}
	if !strings.Contains(a.SessionSetupScript, "scripts/theme.sh") {
		t.Errorf("SessionSetupScript = %q, want to contain scripts/theme.sh", a.SessionSetupScript)
	}
	// Nudge should be inherited from base (not cleared by patch).
	if a.Nudge != "do work" {
		t.Errorf("Nudge = %q, want %q (inherited from base)", a.Nudge, "do work")
	}
}

func TestPackLevelPatches_PathResolution(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/base/pack.toml", `
[pack]
name = "base"
schema = 1

[[agent]]
name = "agent1"
`)
	// Overlay with relative script path — should resolve to overlay dir.
	writeFile(t, dir, "packs/overlay/pack.toml", `
[pack]
name = "overlay"
schema = 1
includes = ["../base"]

[[patches.agent]]
name = "agent1"
session_setup_script = "scripts/neon.sh"
prompt_template = "prompts/custom.md"
overlay_dir = "overlays/custom"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/overlay"}},
	}
	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	a := cfg.Agents[0]
	// Paths should be resolved relative to the overlay pack dir.
	wantScript := "packs/overlay/scripts/neon.sh"
	if a.SessionSetupScript != wantScript {
		t.Errorf("SessionSetupScript = %q, want %q", a.SessionSetupScript, wantScript)
	}
	wantTemplate := "packs/overlay/prompts/custom.md"
	if a.PromptTemplate != wantTemplate {
		t.Errorf("PromptTemplate = %q, want %q", a.PromptTemplate, wantTemplate)
	}
	wantOverlay := "packs/overlay/overlays/custom"
	if a.OverlayDir != wantOverlay {
		t.Errorf("OverlayDir = %q, want %q", a.OverlayDir, wantOverlay)
	}
}

func TestPackLevelPatches_NotFound(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/base/pack.toml", `
[pack]
name = "base"
schema = 1

[[agent]]
name = "worker"
`)
	// Patch targets nonexistent agent.
	writeFile(t, dir, "packs/overlay/pack.toml", `
[pack]
name = "overlay"
schema = 1
includes = ["../base"]

[[patches.agent]]
name = "ghost"
nudge = "boo"
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/overlay"}},
	}
	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err == nil {
		t.Fatal("expected error for patch targeting nonexistent agent")
	}
	if !strings.Contains(err.Error(), "ghost") {
		t.Errorf("error = %q, want mention of 'ghost'", err.Error())
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want mention of 'not found'", err.Error())
	}
}

func TestPackLevelPatches_AppendFields(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/base/pack.toml", `
[pack]
name = "base"
schema = 1

[[agent]]
name = "worker"
session_setup = ["tmux set status on"]
pre_start = ["init.sh"]
`)
	// Patch uses _append variants to add to existing lists.
	writeFile(t, dir, "packs/overlay/pack.toml", `
[pack]
name = "overlay"
schema = 1
includes = ["../base"]

[[patches.agent]]
name = "worker"
session_setup_append = ["tmux set mouse on"]
pre_start_append = ["extra.sh"]
`)

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/overlay"}},
	}
	_, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir)
	if err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	a := cfg.Agents[0]
	wantSetup := []string{"tmux set status on", "tmux set mouse on"}
	if !sliceEqual(a.SessionSetup, wantSetup) {
		t.Errorf("SessionSetup = %v, want %v", a.SessionSetup, wantSetup)
	}
	wantPreStart := []string{"init.sh", "extra.sh"}
	if !sliceEqual(a.PreStart, wantPreStart) {
		t.Errorf("PreStart = %v, want %v", a.PreStart, wantPreStart)
	}
}

func TestPackLevelPatches_RigScoped(t *testing.T) {
	dir := t.TempDir()
	// Base pack with a rig-scoped agent.
	writeFile(t, dir, "packs/base/pack.toml", `
[pack]
name = "base"
schema = 1

[[agent]]
name = "witness"
scope = "rig"
nudge = "patrol"
start_command = "claude --model opus"
`)
	// Overlay pack includes base and patches the agent's start_command.
	// This is the rig-scoped case: agents get dir-stamped during recursive
	// loadPack, so the patch must match by name alone (dir = "").
	writeFile(t, dir, "packs/overlay/pack.toml", `
[pack]
name = "overlay"
schema = 1
includes = ["../base"]

[[patches.agent]]
name = "witness"
start_command = "claude --model sonnet"
`)

	cfg := &City{
		Rigs: []Rig{{
			Name:     "myrig",
			Path:     dir,
			Includes: []string{"packs/overlay"},
		}},
	}
	err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil)
	if err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}
	// Find the witness agent for myrig.
	var found *Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "witness" && cfg.Agents[i].Dir == "myrig" {
			found = &cfg.Agents[i]
			break
		}
	}
	if found == nil {
		t.Fatal("witness agent not found for myrig")
	}
	if found.StartCommand != "claude --model sonnet" {
		t.Errorf("StartCommand = %q, want %q", found.StartCommand, "claude --model sonnet")
	}
	// Nudge should be inherited from base (not cleared by patch).
	if found.Nudge != "patrol" {
		t.Errorf("Nudge = %q, want %q (inherited from base)", found.Nudge, "patrol")
	}
}

func TestPackDoctorEntriesParsed(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pack.toml", `
[pack]
name = "test-topo"
schema = 1

[[doctor]]
name = "check-binaries"
script = "doctor/check-binaries.sh"
description = "Verify required binaries"

[[doctor]]
name = "check-config"
script = "doctor/check-config.sh"

[[agent]]
name = "worker"
`)

	entries := LoadPackDoctorEntries(fsys.OSFS{}, []string{dir})
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	if entries[0].PackName != "test-topo" {
		t.Errorf("PackName = %q, want %q", entries[0].PackName, "test-topo")
	}
	if entries[0].Entry.Name != "check-binaries" {
		t.Errorf("Entry.Name = %q, want %q", entries[0].Entry.Name, "check-binaries")
	}
	if entries[0].Entry.Script != "doctor/check-binaries.sh" {
		t.Errorf("Entry.Script = %q, want %q", entries[0].Entry.Script, "doctor/check-binaries.sh")
	}
	if entries[0].Entry.Description != "Verify required binaries" {
		t.Errorf("Entry.Description = %q, want %q", entries[0].Entry.Description, "Verify required binaries")
	}
	if entries[0].TopoDir != dir {
		t.Errorf("TopoDir = %q, want %q", entries[0].TopoDir, dir)
	}

	// Second entry should have empty description (optional field).
	if entries[1].Entry.Name != "check-config" {
		t.Errorf("second Entry.Name = %q, want %q", entries[1].Entry.Name, "check-config")
	}
	if entries[1].Entry.Description != "" {
		t.Errorf("second Entry.Description = %q, want empty", entries[1].Entry.Description)
	}

	// Fix field defaults to empty when not declared (diagnostic-only check).
	if entries[0].Entry.Fix != "" {
		t.Errorf("Entry.Fix = %q, want empty when not declared", entries[0].Entry.Fix)
	}
}

func TestPackDoctorEntriesParsesFixField(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pack.toml", `
[pack]
name = "fixable"
schema = 1

[[doctor]]
name = "check-with-fix"
script = "doctor/check.sh"
fix = "doctor/fix.sh"
description = "Check that opts into auto-remediation"

[[doctor]]
name = "check-no-fix"
script = "doctor/check2.sh"
`)

	entries := LoadPackDoctorEntries(fsys.OSFS{}, []string{dir})
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}

	if entries[0].Entry.Fix != "doctor/fix.sh" {
		t.Errorf("Entry.Fix = %q, want %q", entries[0].Entry.Fix, "doctor/fix.sh")
	}
	if entries[1].Entry.Fix != "" {
		t.Errorf("Entry.Fix without fix field = %q, want empty", entries[1].Entry.Fix)
	}
}

func TestLegacyPackDoctorsRejectsEscapingFixPaths(t *testing.T) {
	dir := t.TempDir()
	tests := []struct {
		name string
		fix  string
	}{
		{name: "absolute", fix: filepath.Join(dir, "outside.sh")},
		{name: "relative escape", fix: "../../../outside.sh"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := legacyPackDoctors(fsys.OSFS{}, []PackDoctorEntry{{
				Name:   "check",
				Script: "doctor/check.sh",
				Fix:    tt.fix,
			}}, filepath.Join(dir, "pack"), "pack")
			if err == nil {
				t.Fatal("legacyPackDoctors error = nil, want containment error")
			}
			if !strings.Contains(err.Error(), "doctor check fix") {
				t.Fatalf("legacyPackDoctors error = %v, want check fix context", err)
			}
		})
	}
}

func TestLegacyPackDoctorsRejectsMissingFixScript(t *testing.T) {
	packDir := filepath.Join(t.TempDir(), "pack")
	if err := os.MkdirAll(packDir, 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := legacyPackDoctors(fsys.OSFS{}, []PackDoctorEntry{{
		Name:   "check",
		Script: "doctor/check.sh",
		Fix:    "doctor/missing-fix.sh",
	}}, packDir, "pack")
	if err == nil {
		t.Fatal("legacyPackDoctors error = nil, want missing fix script error")
	}
	if !strings.Contains(err.Error(), "doctor check fix") {
		t.Fatalf("legacyPackDoctors error = %v, want check fix context", err)
	}
}

func TestPackDoctorEntriesDeduplicatesDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pack.toml", `
[pack]
name = "test-topo"
schema = 1

[[doctor]]
name = "check-foo"
script = "doctor/check-foo.sh"
`)

	// Pass the same directory twice.
	entries := LoadPackDoctorEntries(fsys.OSFS{}, []string{dir, dir})
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 (deduplication)", len(entries))
	}
}

func TestPackDoctorEntriesNoDoctorSection(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "pack.toml", `
[pack]
name = "bare"
schema = 1

[[agent]]
name = "worker"
`)

	entries := LoadPackDoctorEntries(fsys.OSFS{}, []string{dir})
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0 for pack without [[doctor]]", len(entries))
	}
}

func TestPackDoctorEntriesSkipsBadDir(t *testing.T) {
	goodDir := t.TempDir()
	writeFile(t, goodDir, "pack.toml", `
[pack]
name = "good"
schema = 1

[[doctor]]
name = "check-a"
script = "doctor/a.sh"
`)

	entries := LoadPackDoctorEntries(fsys.OSFS{}, []string{"/nonexistent/dir", goodDir})
	if len(entries) != 1 {
		t.Fatalf("got %d entries, want 1 (bad dir skipped)", len(entries))
	}
	if entries[0].PackName != "good" {
		t.Errorf("PackName = %q, want %q", entries[0].PackName, "good")
	}
}

func TestPackDoctorEntriesMultiplePacks(t *testing.T) {
	dir1 := t.TempDir()
	writeFile(t, dir1, "pack.toml", `
[pack]
name = "alpha"
schema = 1

[[doctor]]
name = "check-a"
script = "doctor/a.sh"
`)

	dir2 := t.TempDir()
	writeFile(t, dir2, "pack.toml", `
[pack]
name = "beta"
schema = 1

[[doctor]]
name = "check-b"
script = "doctor/b.sh"
`)

	entries := LoadPackDoctorEntries(fsys.OSFS{}, []string{dir1, dir2})
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	if entries[0].PackName != "alpha" {
		t.Errorf("first PackName = %q, want %q", entries[0].PackName, "alpha")
	}
	if entries[1].PackName != "beta" {
		t.Errorf("second PackName = %q, want %q", entries[1].PackName, "beta")
	}
}

// --- PackOverlayDirs tests ---

func TestExpandCityPacks_OverlayDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/skills/pack.toml", `
[pack]
name = "skills"
schema = 1

[[agent]]
name = "worker"
`)
	// Create overlay/ directory in the pack.
	if err := os.MkdirAll(filepath.Join(dir, "packs/skills/overlay/.claude/skills/plan"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "packs/skills/overlay/.claude/skills/plan/SKILL.md", "plan skill")

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/skills"}},
	}

	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	if len(cfg.PackOverlayDirs) != 1 {
		t.Fatalf("got %d PackOverlayDirs, want 1", len(cfg.PackOverlayDirs))
	}
	want := filepath.Join(dir, "packs/skills/overlay")
	if cfg.PackOverlayDirs[0] != want {
		t.Errorf("PackOverlayDirs[0] = %q, want %q", cfg.PackOverlayDirs[0], want)
	}
}

func TestExpandCityPacks_NoOverlayDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/bare/pack.toml", `
[pack]
name = "bare"
schema = 1

[[agent]]
name = "worker"
`)
	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/bare"}},
	}

	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	if len(cfg.PackOverlayDirs) != 0 {
		t.Errorf("got %d PackOverlayDirs, want 0 (no overlay/ dir)", len(cfg.PackOverlayDirs))
	}
}

func TestExpandCityPacks_MultiplePacksOverlayDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/alpha/pack.toml", `
[pack]
name = "alpha"
schema = 1
`)
	if err := os.MkdirAll(filepath.Join(dir, "packs/alpha/overlay"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "packs/alpha/overlay/a.txt", "from alpha")

	writeFile(t, dir, "packs/beta/pack.toml", `
[pack]
name = "beta"
schema = 1
`)
	if err := os.MkdirAll(filepath.Join(dir, "packs/beta/overlay"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "packs/beta/overlay/b.txt", "from beta")

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/alpha", "packs/beta"}},
	}

	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	if len(cfg.PackOverlayDirs) != 2 {
		t.Fatalf("got %d PackOverlayDirs, want 2", len(cfg.PackOverlayDirs))
	}
}

func TestExpandPacks_RigOverlayDirs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/rig-skills/pack.toml", `
[pack]
name = "rig-skills"
schema = 1

[[agent]]
name = "coder"
`)
	if err := os.MkdirAll(filepath.Join(dir, "packs/rig-skills/overlay"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "packs/rig-skills/overlay/skill.txt", "rig skill")

	cfg := &City{
		Rigs: []Rig{
			{Name: "my-project", Path: "/tmp/project", Includes: []string{"packs/rig-skills"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	if cfg.RigOverlayDirs == nil {
		t.Fatal("RigOverlayDirs is nil")
	}
	dirs := cfg.RigOverlayDirs["my-project"]
	if len(dirs) != 1 {
		t.Fatalf("got %d rig overlay dirs, want 1", len(dirs))
	}
	want := filepath.Join(dir, "packs/rig-skills/overlay")
	if dirs[0] != want {
		t.Errorf("RigOverlayDirs[my-project][0] = %q, want %q", dirs[0], want)
	}
}

func TestExpandPacks_RigNoOverlayDir(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/bare/pack.toml", `
[pack]
name = "bare"
schema = 1

[[agent]]
name = "worker"
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "hw", Path: "/tmp/hw", Includes: []string{"packs/bare"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	if len(cfg.RigOverlayDirs) != 0 {
		t.Errorf("got %d rig overlay dir entries, want 0", len(cfg.RigOverlayDirs))
	}
}

func TestExpandCityPacks_IncludedPackOverlayDirs(t *testing.T) {
	dir := t.TempDir()

	// Child pack with overlay.
	writeFile(t, dir, "packs/child/pack.toml", `
[pack]
name = "child"
schema = 1
`)
	if err := os.MkdirAll(filepath.Join(dir, "packs/child/overlay"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "packs/child/overlay/child.txt", "from child")

	// Parent pack includes child, also has overlay.
	writeFile(t, dir, "packs/parent/pack.toml", `
[pack]
name = "parent"
schema = 1
includes = ["../child"]
`)
	if err := os.MkdirAll(filepath.Join(dir, "packs/parent/overlay"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "packs/parent/overlay/parent.txt", "from parent")

	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/parent"}},
	}

	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}

	// Should have both child and parent overlay dirs.
	if len(cfg.PackOverlayDirs) != 2 {
		t.Fatalf("got %d PackOverlayDirs, want 2", len(cfg.PackOverlayDirs))
	}

	// Child comes first (included packs are lower priority).
	wantChild := filepath.Join(dir, "packs/child/overlay")
	wantParent := filepath.Join(dir, "packs/parent/overlay")
	if cfg.PackOverlayDirs[0] != wantChild {
		t.Errorf("PackOverlayDirs[0] = %q, want %q", cfg.PackOverlayDirs[0], wantChild)
	}
	if cfg.PackOverlayDirs[1] != wantParent {
		t.Errorf("PackOverlayDirs[1] = %q, want %q", cfg.PackOverlayDirs[1], wantParent)
	}
}

func TestPackGlobal_CityLevel(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/theme/pack.toml", `
[pack]
name = "theme"
schema = 1

[global]
session_live = ["echo theme applied"]
`)
	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/theme"}},
		Agents: []Agent{
			{Name: "alpha"},
			{Name: "beta"},
		},
	}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	applyPackGlobals(cfg)

	// Both agents should get the global command appended.
	for _, a := range cfg.Agents {
		if len(a.SessionLive) != 1 || a.SessionLive[0] != "echo theme applied" {
			t.Errorf("agent %q: SessionLive = %v, want [\"echo theme applied\"]", a.Name, a.SessionLive)
		}
	}
}

func TestPackGlobal_RigLevel(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/rig-theme/pack.toml", `
[pack]
name = "rig-theme"
schema = 1

[global]
session_live = ["echo rig theme"]

[[agent]]
name = "worker"
`)
	cfg := &City{
		Agents: []Agent{
			{Name: "city-agent", Dir: ""},
		},
		Rigs: []Rig{
			{Name: "my-rig", Path: "/tmp/rig", Includes: []string{"packs/rig-theme"}},
		},
	}
	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}
	applyPackGlobals(cfg)

	// City agent should NOT get rig-level global.
	for _, a := range cfg.Agents {
		if a.Name == "city-agent" {
			if len(a.SessionLive) != 0 {
				t.Errorf("city-agent should not get rig global, got %v", a.SessionLive)
			}
		}
		// Rig agent should get the global.
		if a.Name == "worker" && a.Dir == "my-rig" {
			if len(a.SessionLive) != 1 || a.SessionLive[0] != "echo rig theme" {
				t.Errorf("rig worker: SessionLive = %v, want [\"echo rig theme\"]", a.SessionLive)
			}
		}
	}
}

func TestPackGlobal_ConfigDirResolution(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/scripts/pack.toml", `
[pack]
name = "scripts"
schema = 1

[global]
session_live = [
    "{{.ConfigDir}}/run.sh {{.Session}} {{.Agent}}",
]
`)
	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/scripts"}},
		Agents:    []Agent{{Name: "test-agent"}},
	}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	applyPackGlobals(cfg)

	// {{.ConfigDir}} should be resolved to pack dir, {{.Session}} and
	// {{.Agent}} should remain as templates.
	packDir := filepath.Join(dir, "packs/scripts")
	want := packDir + "/run.sh {{.Session}} {{.Agent}}"
	for _, a := range cfg.Agents {
		if len(a.SessionLive) != 1 {
			t.Fatalf("agent %q: got %d SessionLive commands, want 1", a.Name, len(a.SessionLive))
		}
		if a.SessionLive[0] != want {
			t.Errorf("agent %q: SessionLive[0] = %q, want %q", a.Name, a.SessionLive[0], want)
		}
	}
}

func TestPackGlobal_MultipleGlobalPacks(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/theme-a/pack.toml", `
[pack]
name = "theme-a"
schema = 1

[global]
session_live = ["echo A"]
`)
	writeFile(t, dir, "packs/theme-b/pack.toml", `
[pack]
name = "theme-b"
schema = 1

[global]
session_live = ["echo B"]
`)
	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/theme-a", "packs/theme-b"}},
		Agents:    []Agent{{Name: "solo"}},
	}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	applyPackGlobals(cfg)

	// Both globals should be appended in order.
	if len(cfg.Agents[0].SessionLive) != 2 {
		t.Fatalf("got %d SessionLive, want 2", len(cfg.Agents[0].SessionLive))
	}
	if cfg.Agents[0].SessionLive[0] != "echo A" {
		t.Errorf("SessionLive[0] = %q, want %q", cfg.Agents[0].SessionLive[0], "echo A")
	}
	if cfg.Agents[0].SessionLive[1] != "echo B" {
		t.Errorf("SessionLive[1] = %q, want %q", cfg.Agents[0].SessionLive[1], "echo B")
	}
}

func TestPackGlobal_EmptyGlobal(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/empty/pack.toml", `
[pack]
name = "empty"
schema = 1

[global]
session_live = []
`)
	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/empty"}},
		Agents:    []Agent{{Name: "untouched", SessionLive: []string{"existing"}}},
	}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	applyPackGlobals(cfg)

	// Empty global should be a no-op.
	if len(cfg.Agents[0].SessionLive) != 1 || cfg.Agents[0].SessionLive[0] != "existing" {
		t.Errorf("SessionLive = %v, want [\"existing\"]", cfg.Agents[0].SessionLive)
	}
}

func TestPackGlobal_OrderingAfterPatches(t *testing.T) {
	dir := t.TempDir()
	// Pack with agent that has own session_live, a patch, and a global.
	writeFile(t, dir, "packs/full/pack.toml", `
[pack]
name = "full"
schema = 1

[[agent]]
name = "worker"
session_live = ["echo own"]

[patches]
[[patches.agent]]
name = "worker"
session_live_append = ["echo patched"]

[global]
session_live = ["echo global"]
`)
	cfg := &City{
		Workspace: Workspace{Includes: []string{"packs/full"}},
	}
	if _, _, _, err := ExpandCityPacks(cfg, fsys.OSFS{}, dir); err != nil {
		t.Fatalf("ExpandCityPacks: %v", err)
	}
	applyPackGlobals(cfg)

	// Order: own < patch < global.
	want := []string{"echo own", "echo patched", "echo global"}
	if len(cfg.Agents) != 1 {
		t.Fatalf("got %d agents, want 1", len(cfg.Agents))
	}
	got := cfg.Agents[0].SessionLive
	if len(got) != len(want) {
		t.Fatalf("SessionLive = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("SessionLive[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestPackDefinesAgent_Found(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "polecat"

[[agent]]
name = "refinery"
`)
	fs := fsys.OSFS{}
	if !PackDefinesAgent(fs, "packs/gastown", dir, "polecat") {
		t.Error("PackDefinesAgent should find polecat")
	}
	if !PackDefinesAgent(fs, "packs/gastown", dir, "refinery") {
		t.Error("PackDefinesAgent should find refinery")
	}
}

func TestPackDefinesAgent_NotFound(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "refinery"
`)
	fs := fsys.OSFS{}
	if PackDefinesAgent(fs, "packs/gastown", dir, "polecat") {
		t.Error("PackDefinesAgent should not find polecat")
	}
}

func TestPackDefinesAgent_RecursiveIncludes(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/base/pack.toml", `
[pack]
name = "base"
schema = 1

[[agent]]
name = "polecat"
`)
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 1
includes = ["../base"]

[[agent]]
name = "refinery"
`)
	fs := fsys.OSFS{}
	// polecat is defined in the included base pack.
	if !PackDefinesAgent(fs, "packs/gastown", dir, "polecat") {
		t.Error("PackDefinesAgent should find polecat via included pack")
	}
}

func TestPackDefinesAgent_CityScoped(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/gastown/pack.toml", `
[pack]
name = "gastown"
schema = 1

[[agent]]
name = "mayor"
scope = "city"

[[agent]]
name = "polecat"
scope = "rig"
`)
	fs := fsys.OSFS{}
	// mayor is city-scoped via scope="city", should NOT be found as rig agent.
	if PackDefinesAgent(fs, "packs/gastown", dir, "mayor") {
		t.Error("PackDefinesAgent should not find city-scoped mayor as rig agent")
	}
	// polecat is rig-scoped, should be found.
	if !PackDefinesAgent(fs, "packs/gastown", dir, "polecat") {
		t.Error("PackDefinesAgent should find rig-scoped polecat")
	}
}

func TestPackDefinesAgent_BadPack(t *testing.T) {
	// Returns false on error (fail-open).
	fs := fsys.OSFS{}
	if PackDefinesAgent(fs, "/nonexistent/pack", "/tmp", "polecat") {
		t.Error("PackDefinesAgent should return false for bad pack")
	}
}

func TestExpandPacks_DependsOnQualified(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "packs/mypack/pack.toml", `
[pack]
name = "mypack"
version = "1.0.0"
schema = 1

[[agent]]
name = "db"

[[agent]]
name = "worker"
depends_on = ["db"]
`)

	cfg := &City{
		Rigs: []Rig{
			{Name: "myrig", Path: "/home/user/myrig", Includes: []string{"packs/mypack"}},
		},
	}

	if err := ExpandPacks(cfg, fsys.OSFS{}, dir, nil); err != nil {
		t.Fatalf("ExpandPacks: %v", err)
	}

	// After stamping, both agents should have Dir = "myrig", so
	// worker's depends_on should be rewritten to "myrig/db".
	var worker *Agent
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == "worker" {
			worker = &cfg.Agents[i]
			break
		}
	}
	if worker == nil {
		t.Fatal("worker agent not found after expansion")
	}
	if len(worker.DependsOn) != 1 {
		t.Fatalf("DependsOn length = %d, want 1", len(worker.DependsOn))
	}
	if worker.DependsOn[0] != "myrig/db" {
		t.Errorf("DependsOn[0] = %q, want %q", worker.DependsOn[0], "myrig/db")
	}

	// Validation should pass since deps are now qualified.
	if err := ValidateAgents(cfg.Agents); err != nil {
		t.Errorf("ValidateAgents failed after pack expansion: %v", err)
	}
}
