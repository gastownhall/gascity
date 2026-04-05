package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

// helper creates .gc/ and city.toml in a temp dir.
func setupCity(t *testing.T, tomlContent string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(tomlContent), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// --- CityStructureCheck ---

func TestCityStructureCheck_OK(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	c := &CityStructureCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestCityStructureCheck_MissingGC(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace]\nname = \"test\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &CityStructureCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK", r.Status)
	}
}

func TestCityStructureCheck_MissingToml(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := &CityStructureCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
}

// --- CityConfigCheck ---

func TestCityConfigCheck_OK(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	c := &CityConfigCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestCityConfigCheck_ParseError(t *testing.T) {
	dir := setupCity(t, "{{invalid toml")
	c := &CityConfigCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error", r.Status)
	}
}

func TestCityConfigCheck_NoName(t *testing.T) {
	dir := setupCity(t, "[workspace]\n")
	c := &CityConfigCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	// Empty workspace.name with a derived ResolvedWorkspaceName is a warning,
	// not an error — EffectiveHQPrefix falls back to the derived name.
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
}

// --- ConfigValidCheck ---

func TestConfigValidCheck_OK(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "mayor"}},
	}
	c := NewConfigValidCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestConfigValidCheck_BadAgent(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: ""}}, // missing name
	}
	c := NewConfigValidCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error", r.Status)
	}
}

func TestConfigValidCheck_BadRig(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test"},
		Agents:    []config.Agent{{Name: "mayor"}},
		Rigs:      []config.Rig{{Name: "rig1"}}, // missing path
	}
	c := NewConfigValidCheck(cfg)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error", r.Status)
	}
}

// --- ConfigRefsCheck ---

func TestConfigRefsCheck_AllValid(t *testing.T) {
	dir := t.TempDir()
	// Create referenced files.
	if err := os.MkdirAll(filepath.Join(dir, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "prompts", "mayor.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "setup.sh"), []byte("#!/bin/sh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "overlay"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Agents: []config.Agent{
			{
				Name:               "mayor",
				PromptTemplate:     "prompts/mayor.md",
				SessionSetupScript: "setup.sh",
				OverlayDir:         "overlay",
				Provider:           "claude",
			},
		},
	}
	c := NewConfigRefsCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
}

func TestConfigRefsCheck_MissingPromptTemplate(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "mayor", PromptTemplate: "prompts/missing.md"},
		},
	}
	c := NewConfigRefsCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if len(r.Details) != 1 {
		t.Errorf("expected 1 issue, got %d: %v", len(r.Details), r.Details)
	}
}

func TestConfigRefsCheck_MissingSessionSetupScript(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", SessionSetupScript: "scripts/nonexistent.sh"},
		},
	}
	c := NewConfigRefsCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
}

func TestConfigRefsCheck_OverlayDirNotDir(t *testing.T) {
	dir := t.TempDir()
	// Create a file where a directory is expected.
	if err := os.WriteFile(filepath.Join(dir, "not-a-dir"), []byte("file"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", OverlayDir: "not-a-dir"},
		},
	}
	c := NewConfigRefsCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
}

func TestConfigRefsCheck_UndefinedProvider(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Agents: []config.Agent{
			{Name: "worker", Provider: "nonexistent"},
		},
	}
	c := NewConfigRefsCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
}

func TestConfigRefsCheck_NoProvidersDefined(t *testing.T) {
	// When no providers section exists, agent provider refs are not checked.
	dir := t.TempDir()
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Provider: "claude"},
		},
	}
	c := NewConfigRefsCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (no providers defined = skip check); msg = %s", r.Status, r.Message)
	}
}

func TestConfigRefsCheck_MultipleIssues(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Agents: []config.Agent{
			{Name: "a1", PromptTemplate: "missing.md"},
			{Name: "a2", Provider: "bogus"},
		},
	}
	c := NewConfigRefsCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning", r.Status)
	}
	if len(r.Details) != 2 {
		t.Errorf("expected 2 issues, got %d: %v", len(r.Details), r.Details)
	}
}

// --- BuiltinPackFamilyCheck ---

func TestBuiltinPackFamilyCheck_Unmodified(t *testing.T) {
	c := NewBuiltinPackFamilyCheck(&config.City{}, t.TempDir())
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestBuiltinPackFamilyCheck_FullOverrideOK(t *testing.T) {
	dir := t.TempDir()
	bdDir := filepath.Join(dir, "packs", "bd")
	doltDir := filepath.Join(dir, "packs", "dolt")
	for _, tc := range []struct {
		dir  string
		name string
	}{
		{dir: bdDir, name: "bd"},
		{dir: doltDir, name: "dolt"},
	} {
		if err := os.MkdirAll(tc.dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tc.dir, "pack.toml"), []byte("[pack]\nname = \""+tc.name+"\"\nschema = 1\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := &config.City{PackDirs: []string{bdDir, doltDir}}
	c := NewBuiltinPackFamilyCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestBuiltinPackFamilyCheck_PartialOverrideFails(t *testing.T) {
	dir := t.TempDir()
	doltDir := filepath.Join(dir, "packs", "dolt")
	if err := os.MkdirAll(doltDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDir, "pack.toml"), []byte("[pack]\nname = \"dolt\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{PackDirs: []string{doltDir}}
	c := NewBuiltinPackFamilyCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "\"bd\"") {
		t.Fatalf("message = %q, want missing bd", r.Message)
	}
}

func TestBuiltinPackFamilyCheck_IgnoresSystemPacks(t *testing.T) {
	dir := t.TempDir()
	systemDolt := filepath.Join(dir, ".gc", "system", "packs", "dolt")
	if err := os.MkdirAll(systemDolt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(systemDolt, "pack.toml"), []byte("[pack]\nname = \"dolt\"\nschema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{PackDirs: []string{systemDolt}}
	c := NewBuiltinPackFamilyCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

// --- BinaryCheck ---

func TestBinaryCheck_Found(t *testing.T) {
	c := NewBinaryCheck("tmux", "", func(_ string) (string, error) {
		return "/usr/bin/tmux", nil
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestBinaryCheck_NotFound(t *testing.T) {
	c := NewBinaryCheck("tmux", "", func(_ string) (string, error) {
		return "", fmt.Errorf("not found")
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error", r.Status)
	}
}

func TestBinaryCheck_Skipped(t *testing.T) {
	c := NewBinaryCheck("bd", "skipped (GC_BEADS=file)", nil)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (skipped)", r.Status)
	}
	if r.Message != "skipped (GC_BEADS=file)" {
		t.Errorf("message = %q, want skip message", r.Message)
	}
}

func TestBinaryCheck_VersionOK(t *testing.T) {
	c := NewVersionedBinaryCheck("bd", "", func(_ string) (string, error) {
		return "/usr/local/bin/bd", nil
	}, "0.57.0", func() (string, error) {
		return "0.58.0", nil
	}, "go install github.com/gastownhall/beads/cmd/bd@latest")
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "v0.58.0") {
		t.Errorf("message should contain version, got: %s", r.Message)
	}
}

func TestBinaryCheck_VersionTooOld(t *testing.T) {
	c := NewVersionedBinaryCheck("dolt", "", func(_ string) (string, error) {
		return "/usr/local/bin/dolt", nil
	}, "1.83.1", func() (string, error) {
		return "1.82.0", nil
	}, "https://github.com/dolthub/dolt#installation")
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "too old") {
		t.Errorf("message should say too old, got: %s", r.Message)
	}
	if !strings.Contains(r.FixHint, "1.83.1") {
		t.Errorf("fix hint should reference min version, got: %s", r.FixHint)
	}
}

func TestBinaryCheck_VersionUnknown(t *testing.T) {
	c := NewVersionedBinaryCheck("bd", "", func(_ string) (string, error) {
		return "/usr/local/bin/bd", nil
	}, "0.57.0", func() (string, error) {
		return "", fmt.Errorf("parse error")
	}, "")
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
}

func TestBinaryCheck_VersionNotFoundStillError(t *testing.T) {
	c := NewVersionedBinaryCheck("dolt", "", func(_ string) (string, error) {
		return "", fmt.Errorf("not found")
	}, "1.83.1", func() (string, error) {
		return "1.83.1", nil
	}, "https://github.com/dolthub/dolt#installation")
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error (not found)", r.Status)
	}
	if !strings.Contains(r.FixHint, "dolthub") {
		t.Errorf("fix hint should contain install URL, got: %s", r.FixHint)
	}
}

// --- AgentSessionsCheck ---

func TestAgentSessionsCheck_AllRunning(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	c := NewAgentSessionsCheck(cfg, "test", "", sp)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestAgentSessionsCheck_Missing(t *testing.T) {
	sp := runtime.NewFake()
	// Don't start any sessions.

	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	c := NewAgentSessionsCheck(cfg, "test", "", sp)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
}

func TestAgentSessionsCheck_SkipsSuspended(t *testing.T) {
	sp := runtime.NewFake()
	// Suspended agent has no session — that's fine.

	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker", Suspended: true}},
	}
	c := NewAgentSessionsCheck(cfg, "test", "", sp)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (suspended skipped); msg = %s", r.Status, r.Message)
	}
}

// --- ZombieSessionsCheck ---

func TestZombieSessionsCheck_NoZombies(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor", ProcessNames: []string{"claude"}}},
	}
	c := NewZombieSessionsCheck(cfg, "test", "", sp)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestZombieSessionsCheck_Found(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	sp.Zombies["mayor"] = true

	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor", ProcessNames: []string{"claude"}}},
	}
	c := NewZombieSessionsCheck(cfg, "test", "", sp)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
}

func TestZombieSessionsCheck_Fix(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	sp.Zombies["mayor"] = true

	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor", ProcessNames: []string{"claude"}}},
	}
	c := NewZombieSessionsCheck(cfg, "test", "", sp)
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}
	// After fix, session should be stopped.
	if sp.IsRunning("mayor") {
		t.Error("zombie session still running after fix")
	}
}

func TestZombieSessionsCheck_SkipsNoProcessNames(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	sp.Zombies["mayor"] = true // zombie but no process_names to check

	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}}, // no ProcessNames
	}
	c := NewZombieSessionsCheck(cfg, "test", "", sp)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (no process_names = skip zombie check); msg = %s", r.Status, r.Message)
	}
}

// --- OrphanSessionsCheck ---

func TestOrphanSessionsCheck_NoOrphans(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	c := NewOrphanSessionsCheck(cfg, "test", "", sp)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestOrphanSessionsCheck_Found(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "stale-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	c := NewOrphanSessionsCheck(cfg, "test", "", sp)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
}

func TestOrphanSessionsCheck_Fix(t *testing.T) {
	sp := runtime.NewFake()
	if err := sp.Start(context.Background(), "mayor", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	if err := sp.Start(context.Background(), "stale-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Agents: []config.Agent{{Name: "mayor"}},
	}
	c := NewOrphanSessionsCheck(cfg, "test", "", sp)
	if err := c.Fix(&CheckContext{}); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}
	if sp.IsRunning("stale-worker") {
		t.Error("orphan session still running after fix")
	}
	if !sp.IsRunning("mayor") {
		t.Error("legitimate session was killed by fix")
	}
}

// --- BeadsStoreCheck ---

func TestBeadsStoreCheck_OK(t *testing.T) {
	dir := t.TempDir()
	// Create a file store.
	store, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(dir, "beads.json"))
	if err != nil {
		t.Fatal(err)
	}
	// Create a bead so List returns something.
	if _, err := store.Create(beads.Bead{Title: "test"}); err != nil {
		t.Fatal(err)
	}

	c := NewBeadsStoreCheck(dir, func(cityPath string) (beads.Store, error) {
		return beads.OpenFileStore(fsys.OSFS{}, filepath.Join(cityPath, "beads.json"))
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestBeadsStoreCheck_OpenError(t *testing.T) {
	c := NewBeadsStoreCheck("/nonexistent", func(_ string) (beads.Store, error) {
		return nil, fmt.Errorf("open failed")
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error", r.Status)
	}
}

func TestBeadsStoreCheck_ListPassesOpenFilter(t *testing.T) {
	// The check should call List(Status:"open") to avoid unbounded queries
	// that time out on large stores.
	var gotQuery beads.ListQuery
	spy := &spyListStore{
		listFunc: func(query beads.ListQuery) ([]beads.Bead, error) {
			gotQuery = query
			return []beads.Bead{{Title: "x", Status: "open"}}, nil
		},
	}
	c := NewBeadsStoreCheck(t.TempDir(), func(_ string) (beads.Store, error) {
		return spy, nil
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if gotQuery.Status != "open" {
		t.Errorf("List called with status %q, want %q", gotQuery.Status, "open")
	}
	if gotQuery.AllowScan {
		t.Error("List called with AllowScan=true, want false")
	}
}

// spyListStore is a minimal Store that records List calls.
type spyListStore struct {
	beads.MemStore
	listFunc func(query beads.ListQuery) ([]beads.Bead, error)
}

func (s *spyListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if s.listFunc != nil {
		return s.listFunc(query)
	}
	return s.MemStore.List(query)
}

// --- DoltServerCheck ---

func TestDoltServerCheck_Skipped(t *testing.T) {
	c := NewDoltServerCheck("/tmp", true)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (skipped)", r.Status)
	}
}

// --- EventsLogCheck ---

func TestEventsLogCheck_OK(t *testing.T) {
	dir := t.TempDir()
	gcDir := filepath.Join(dir, ".gc")
	if err := os.MkdirAll(gcDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gcDir, "events.jsonl"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &EventsLogCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestEventsLogCheck_Missing(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := &EventsLogCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
}

// --- ControllerCheck ---

func TestControllerCheck_Running(t *testing.T) {
	c := NewControllerCheck("/tmp", true)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK", r.Status)
	}
	if r.Message != "controller running (sessions managed)" {
		t.Errorf("message = %q", r.Message)
	}
}

func TestControllerCheck_NotRunning(t *testing.T) {
	c := NewControllerCheck("/tmp", false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK", r.Status)
	}
	if r.Message != "controller not running (one-shot mode)" {
		t.Errorf("message = %q", r.Message)
	}
}

// --- RigPathCheck ---

func TestRigPathCheck_OK(t *testing.T) {
	dir := t.TempDir()
	c := NewRigPathCheck(config.Rig{Name: "myrig", Path: dir})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestRigPathCheck_Missing(t *testing.T) {
	c := NewRigPathCheck(config.Rig{Name: "myrig", Path: "/nonexistent/path"})
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error", r.Status)
	}
}

// --- RigGitCheck ---

func TestRigGitCheck_OK(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := NewRigGitCheck(config.Rig{Name: "myrig", Path: dir})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestRigGitCheck_NotGit(t *testing.T) {
	dir := t.TempDir()
	c := NewRigGitCheck(config.Rig{Name: "myrig", Path: dir})
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Errorf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
}

// --- RigBeadsCheck ---

func TestRigBeadsCheck_OK(t *testing.T) {
	dir := t.TempDir()
	c := NewRigBeadsCheck(config.Rig{Name: "myrig", Path: dir}, func(rigPath string) (beads.Store, error) {
		return beads.OpenFileStore(fsys.OSFS{}, filepath.Join(rigPath, "beads.json"))
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestRigBeadsCheck_Error(t *testing.T) {
	c := NewRigBeadsCheck(config.Rig{Name: "myrig", Path: "/nonexistent"}, func(_ string) (beads.Store, error) {
		return nil, fmt.Errorf("store failed")
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error", r.Status)
	}
}

func TestRigBeadsCheck_ListPassesOpenFilter(t *testing.T) {
	var gotQuery beads.ListQuery
	spy := &spyListStore{
		listFunc: func(query beads.ListQuery) ([]beads.Bead, error) {
			gotQuery = query
			return []beads.Bead{{Title: "x", Status: "open"}}, nil
		},
	}
	c := NewRigBeadsCheck(config.Rig{Name: "myrig", Path: t.TempDir()}, func(_ string) (beads.Store, error) {
		return spy, nil
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if gotQuery.Status != "open" {
		t.Errorf("List called with status %q, want %q", gotQuery.Status, "open")
	}
	if gotQuery.AllowScan {
		t.Error("List called with AllowScan=true, want false")
	}
}

// --- IsControllerRunning ---

func TestIsControllerRunning_NoLockFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// No lock file, no controller.
	if IsControllerRunning(dir) {
		t.Error("expected false when no lock exists")
	}
}

func TestIsControllerRunning_UnlockedFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create lock file but don't hold the lock.
	if err := os.WriteFile(filepath.Join(dir, ".gc", "controller.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if IsControllerRunning(dir) {
		t.Error("expected false when lock file exists but not locked")
	}
}

// --- PackCacheCheck ---

func TestPackCacheCheck_OK(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, ".gc", "cache", "packs", "gastown")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "pack.toml"), []byte("[pack]\nname=\"gastown\"\nschema=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewPackCacheCheck(map[string]config.PackSource{
		"gastown": {Source: "https://example.com/gastown"},
	}, dir)
	ctx := &CheckContext{CityPath: dir}
	r := c.Run(ctx)
	if r.Status != StatusOK {
		t.Errorf("status = %v, want OK: %s", r.Status, r.Message)
	}
}

func TestPackCacheCheck_Missing(t *testing.T) {
	dir := t.TempDir()
	// No cache created.

	c := NewPackCacheCheck(map[string]config.PackSource{
		"gastown": {Source: "https://example.com/gastown"},
	}, dir)
	ctx := &CheckContext{CityPath: dir}
	r := c.Run(ctx)
	if r.Status != StatusError {
		t.Errorf("status = %v, want Error: %s", r.Status, r.Message)
	}
	if r.FixHint == "" {
		t.Error("expected fix hint")
	}
}

func TestPackCacheCheck_WithPath(t *testing.T) {
	dir := t.TempDir()
	cacheDir := filepath.Join(dir, ".gc", "cache", "packs", "mono", "packages", "topo")
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cacheDir, "pack.toml"), []byte("[pack]\nname=\"mono\"\nschema=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewPackCacheCheck(map[string]config.PackSource{
		"mono": {Source: "https://example.com/mono", Path: "packages/topo"},
	}, dir)
	ctx := &CheckContext{CityPath: dir}
	r := c.Run(ctx)
	if r.Status != StatusOK {
		t.Errorf("status = %v, want OK: %s", r.Status, r.Message)
	}
}

// --- SystemFormulasCheck ---

func TestSystemFormulasCheckOK(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	formulasDir := filepath.Join(dir, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulasDir, "hello.formula.toml"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &SystemFormulasCheck{
		CityPath:        dir,
		Expected:        []string{"hello.formula.toml"},
		ExpectedContent: map[string][]byte{"hello.formula.toml": []byte("hello")},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestSystemFormulasCheckOrdersOK(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	ordersDir := filepath.Join(dir, "orders", "health")
	if err := os.MkdirAll(ordersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ordersDir, "order.toml"), []byte("health"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &SystemFormulasCheck{
		CityPath:        dir,
		Expected:        []string{"orders/health/order.toml"},
		ExpectedContent: map[string][]byte{"orders/health/order.toml": []byte("health")},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestSystemFormulasCheckMissing(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	// No formulas/ directory, no files.

	c := &SystemFormulasCheck{
		CityPath: dir,
		Expected: []string{"hello.formula.toml"},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
}

func TestSystemFormulasCheckStale(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	formulasDir := filepath.Join(dir, "formulas")
	if err := os.MkdirAll(formulasDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(formulasDir, "hello.formula.toml"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &SystemFormulasCheck{
		CityPath:        dir,
		Expected:        []string{"hello.formula.toml"},
		ExpectedContent: map[string][]byte{"hello.formula.toml": []byte("new")},
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
}

func TestSystemFormulasCheckFix(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")

	fixed := false
	c := &SystemFormulasCheck{
		CityPath: dir,
		Expected: []string{"hello.formula.toml"},
		FixFn: func() error {
			formulasDir := filepath.Join(dir, "formulas")
			if err := os.MkdirAll(formulasDir, 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(formulasDir, "hello.formula.toml"), []byte("hello"), 0o644); err != nil {
				return err
			}
			fixed = true
			return nil
		},
		ExpectedContent: map[string][]byte{"hello.formula.toml": []byte("hello")},
	}

	// Verify it fails first.
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error before fix", r.Status)
	}

	// Fix should succeed.
	if !c.CanFix() {
		t.Fatal("CanFix() = false, want true")
	}
	if err := c.Fix(&CheckContext{CityPath: dir}); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}
	if !fixed {
		t.Error("FixFn was not called")
	}

	// Re-run should be OK.
	r = c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK after fix; msg = %s", r.Status, r.Message)
	}
}

func TestSystemFormulasCheckNoExpected(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	c := &SystemFormulasCheck{
		CityPath: dir,
		Expected: nil,
	}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

// --- WorktreeCheck ---

func TestWorktreeCheckNoWorktrees(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	c := &WorktreeCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestWorktreeCheckAllValid(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")

	// Create a worktree dir with a valid .git file pointing to a real target.
	wtDir := filepath.Join(dir, ".gc", "worktrees", "myrig", "agent1")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create a real target directory that the gitdir points to.
	gitTarget := filepath.Join(dir, ".git", "worktrees", "agent1")
	if err := os.MkdirAll(gitTarget, 0o755); err != nil {
		t.Fatal(err)
	}
	// Write .git file (this is how git worktrees work: .git is a file, not a dir).
	gitContent := fmt.Sprintf("gitdir: %s\n", gitTarget)
	if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte(gitContent), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &WorktreeCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestWorktreeCheckBroken(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")

	// Create a worktree with a .git file pointing to a nonexistent path.
	wtDir := filepath.Join(dir, ".gc", "worktrees", "myrig", "agent1")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitContent := "gitdir: /nonexistent/.git/worktrees/agent1\n"
	if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte(gitContent), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &WorktreeCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if len(r.Details) != 1 {
		t.Errorf("details = %v, want 1 broken entry", r.Details)
	}
}

func TestWorktreeCheckFix(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")

	// Create a broken worktree.
	wtDir := filepath.Join(dir, ".gc", "worktrees", "myrig", "agent1")
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	gitContent := "gitdir: /nonexistent/.git/worktrees/agent1\n"
	if err := os.WriteFile(filepath.Join(wtDir, ".git"), []byte(gitContent), 0o644); err != nil {
		t.Fatal(err)
	}

	c := &WorktreeCheck{}
	ctx := &CheckContext{CityPath: dir}

	// Verify it's broken first.
	r := c.Run(ctx)
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error before fix", r.Status)
	}

	// Fix should remove the broken directory.
	if err := c.Fix(ctx); err != nil {
		t.Fatalf("Fix() error: %v", err)
	}

	// After fix, the worktree dir should be gone.
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Errorf("worktree dir still exists after Fix()")
	}

	// Re-run should be OK.
	r = c.Run(ctx)
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK after fix; msg = %s", r.Status, r.Message)
	}
}
