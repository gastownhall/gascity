package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
)

type partialListDoctorProvider struct {
	*runtime.Fake
	listErr error
}

func (p *partialListDoctorProvider) ListRunning(prefix string) ([]string, error) {
	names, _ := p.Fake.ListRunning(prefix)
	return names, p.listErr
}

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
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestCityConfigCheck_SiteBoundName(t *testing.T) {
	dir := setupCity(t, "[workspace]\n")
	if err := os.WriteFile(filepath.Join(dir, ".gc", "site.toml"), []byte("workspace_name = \"site-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	c := &CityConfigCheck{}
	r := c.Run(&CheckContext{CityPath: dir})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
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
		Rigs:      []config.Rig{{}}, // missing name
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

func TestConfigRefsCheck_BuiltinProviderNotFlagged(t *testing.T) {
	// Builtin providers (e.g. "claude") should not be flagged as undefined
	// even when custom providers are declared in [providers].
	dir := t.TempDir()
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{"ollama-local": {}},
		Agents: []config.Agent{
			{Name: "worker", Provider: "claude"},
			{Name: "coder", Provider: "codex"},
		},
	}
	c := NewConfigRefsCheck(cfg, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (builtin providers are implicitly valid); details = %v", r.Status, r.Details)
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

// Regression for schema=2 packs: convention-discovered agents store
// prompt_template / session_setup_script / overlay_dir as absolute paths.
// The check must stat them directly instead of joining against cityPath,
// which doubles the root prefix and makes every file "not found".
func TestConfigRefsCheck_AbsolutePaths(t *testing.T) {
	cases := []struct {
		name         string
		createFiles  bool
		overlayIsDir bool // only applies when createFiles=true
		wantStatus   CheckStatus
		wantIssues   int
	}{
		{"existing_files", true, true, StatusOK, 0},
		{"missing_files", false, false, StatusWarning, 3},
		{"overlay_is_file_not_dir", true, false, StatusWarning, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cityDir := t.TempDir()
			packDir := t.TempDir()
			absPrompt := filepath.Join(packDir, "agents", "mayor", "prompt.template.md")
			absScript := filepath.Join(packDir, "agents", "mayor", "setup.sh")
			absOverlay := filepath.Join(packDir, "agents", "mayor", "overlay")
			if tc.createFiles {
				if err := os.MkdirAll(filepath.Dir(absPrompt), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(absPrompt, []byte("hi"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(absScript, []byte("#!/bin/sh"), 0o755); err != nil {
					t.Fatal(err)
				}
				if tc.overlayIsDir {
					if err := os.MkdirAll(absOverlay, 0o755); err != nil {
						t.Fatal(err)
					}
				} else {
					if err := os.WriteFile(absOverlay, []byte("file-not-dir"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}

			cfg := &config.City{
				Agents: []config.Agent{{
					Name:               "mayor",
					PromptTemplate:     absPrompt,
					SessionSetupScript: absScript,
					OverlayDir:         absOverlay,
				}},
			}
			c := NewConfigRefsCheck(cfg, cityDir)
			r := c.Run(&CheckContext{})
			if r.Status != tc.wantStatus {
				t.Fatalf("status = %d, want %d; msg = %s; details = %v",
					r.Status, tc.wantStatus, r.Message, r.Details)
			}
			if len(r.Details) != tc.wantIssues {
				t.Errorf("got %d issues, want %d: %v", len(r.Details), tc.wantIssues, r.Details)
			}
		})
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

func TestBuiltinPackFamilyCheck_GCBeadsFileOverrideSkipsRequirement(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_BEADS", "file")
	doltDir := filepath.Join(dir, "packs", "dolt")
	if err := os.MkdirAll(doltDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDir, "pack.toml"), []byte(`[pack]
name = "dolt"
schema = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewBuiltinPackFamilyCheck(&config.City{PackDirs: []string{doltDir}}, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "not required") {
		t.Fatalf("message = %q, want non-bd skip message", r.Message)
	}
}

func TestBuiltinPackFamilyCheck_ExecGcBeadsBdOverrideStillRequiresFamily(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("GC_BEADS", "exec:/tmp/gc-beads-bd")
	doltDir := filepath.Join(dir, "packs", "dolt")
	if err := os.MkdirAll(doltDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDir, "pack.toml"), []byte(`[pack]
name = "dolt"
schema = 1
`), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewBuiltinPackFamilyCheck(&config.City{Beads: config.BeadsConfig{Provider: "file"}, PackDirs: []string{doltDir}}, dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, `"bd"`) {
		t.Fatalf("message = %q, want missing bd pack under exec override", r.Message)
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

func TestOrphanSessionsCheck_PartialListWarns(t *testing.T) {
	sp := &partialListDoctorProvider{
		Fake:    runtime.NewFake(),
		listErr: &runtime.PartialListError{Err: errors.New("remote backend down")},
	}
	if err := sp.Start(context.Background(), "stale-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	c := NewOrphanSessionsCheck(cfg, "test", "", sp)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "partially failed") {
		t.Fatalf("message = %q, want partial failure warning", r.Message)
	}
}

func TestOrphanSessionsCheck_FixFailsOnPartialList(t *testing.T) {
	sp := &partialListDoctorProvider{
		Fake:    runtime.NewFake(),
		listErr: &runtime.PartialListError{Err: errors.New("remote backend down")},
	}
	if err := sp.Start(context.Background(), "stale-worker", runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{}
	c := NewOrphanSessionsCheck(cfg, "test", "", sp)
	if err := c.Fix(&CheckContext{}); err == nil {
		t.Fatal("Fix() error = nil, want partial list failure")
	}
}

// --- BeadsStoreCheck ---

func TestBeadsStoreCheck_OK(t *testing.T) {
	dir := t.TempDir()
	// Create a file store so Ping can verify accessibility.
	if _, err := beads.OpenFileStore(fsys.OSFS{}, filepath.Join(dir, "beads.json")); err != nil {
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

func TestBeadsStoreCheck_UsesPing(t *testing.T) {
	// The check should call Ping() to verify accessibility without loading data.
	pinged := false
	spy := &spyPingStore{
		pingFunc: func() error {
			pinged = true
			return nil
		},
	}
	c := NewBeadsStoreCheck(t.TempDir(), func(_ string) (beads.Store, error) {
		return spy, nil
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !pinged {
		t.Error("Ping was not called")
	}
}

func TestBeadsStoreCheck_FileProviderSkipsDoltPreflight(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n\n[beads]\nprovider = \"file\"\n")
	pinged := false
	spy := &spyPingStore{
		pingFunc: func() error {
			pinged = true
			return nil
		},
	}
	c := NewBeadsStoreCheck(dir, func(_ string) (beads.Store, error) {
		return spy, nil
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !pinged {
		t.Fatal("Ping should run for file provider stores")
	}
}

// --- BDSplitStoreCheck ---

func TestBDSplitStoreCheck_ServerActiveWarnsWhenEmbeddedStoreHasRepos(t *testing.T) {
	dir := t.TempDir()
	fs := fsys.OSFS{}
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")
	writeDoltRepoMarker(t, filepath.Join(dir, ".beads", "dolt", "hq"))
	writeDoltRepoMarker(t, filepath.Join(dir, ".beads", "embeddeddolt", "legacy"))

	c := NewBDSplitStoreCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	for _, want := range []string{"legacy split store", ".beads/embeddeddolt", "1 Dolt repo"} {
		if !strings.Contains(r.Message, want) {
			t.Fatalf("message = %q, want %q", r.Message, want)
		}
	}
	if !strings.Contains(r.FixHint, "bd import --dry-run") {
		t.Fatalf("fix hint = %q, want import dry-run guidance", r.FixHint)
	}
}

func TestBDSplitStoreCheck_EmbeddedActiveWarnsWhenServerStoreHasRepos(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"database":"dolt","backend":"dolt","dolt_mode":"embedded","dolt_database":"legacy"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "embeddeddolt", "legacy"))
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "dolt", "hq"))

	c := NewBDSplitStoreCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	for _, want := range []string{"legacy split store", ".beads/dolt", "metadata.json dolt_mode=embedded"} {
		if !strings.Contains(r.Message, want) {
			t.Fatalf("message = %q, want %q", r.Message, want)
		}
	}
}

func TestBDSplitStoreCheck_BothDirsButInactiveEmptyIsOK(t *testing.T) {
	dir := t.TempDir()
	fs := fsys.OSFS{}
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")
	writeDoltRepoMarker(t, filepath.Join(dir, ".beads", "dolt", "hq"))
	if err := os.MkdirAll(filepath.Join(dir, ".beads", "embeddeddolt"), 0o700); err != nil {
		t.Fatal(err)
	}

	c := NewBDSplitStoreCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestBDSplitStoreCheck_ExternalCityTreatsLocalReposAsLegacy(t *testing.T) {
	dir := t.TempDir()
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")
	writeDoltRepoMarker(t, filepath.Join(dir, ".beads", "dolt", "hq"))
	if err := os.MkdirAll(filepath.Join(dir, ".beads", "embeddeddolt"), 0o700); err != nil {
		t.Fatal(err)
	}

	c := NewBDSplitStoreCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no active local store") {
		t.Fatalf("message = %q, want no active local store warning", r.Message)
	}
}

func TestBDSplitStoreCheck_InvalidExternalCityConfigUsesNeutralGuidance(t *testing.T) {
	dir := t.TempDir()
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")
	writeDoltRepoMarker(t, filepath.Join(dir, ".beads", "dolt", "hq"))
	writeDoltRepoMarker(t, filepath.Join(dir, ".beads", "embeddeddolt", "legacy"))

	c := NewBDSplitStoreCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no active local store") {
		t.Fatalf("message = %q, want no active local store warning", r.Message)
	}
	if strings.Contains(r.Message, "active .beads/dolt") {
		t.Fatalf("message = %q, should not trust metadata when canonical external config is invalid", r.Message)
	}
	if strings.Contains(r.FixHint, "inactive store") {
		t.Fatalf("fix hint = %q, should not reference inactive store when active local store is unknown", r.FixHint)
	}
}

func TestBDSplitStoreCheck_FileProviderUsesNeutralRecoveryGuidance(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	dir := t.TempDir()
	fs := fsys.OSFS{}
	if err := os.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")
	writeDoltRepoMarker(t, filepath.Join(dir, ".beads", "dolt", "hq"))
	writeDoltRepoMarker(t, filepath.Join(dir, ".beads", "embeddeddolt", "legacy"))

	c := NewBDSplitStoreCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no active local store") {
		t.Fatalf("message = %q, want no active local store warning", r.Message)
	}
	if strings.Contains(r.FixHint, "inactive store") {
		t.Fatalf("fix hint = %q, should not reference inactive store under file provider", r.FixHint)
	}
}

func TestBDSplitStoreCheck_ManagedCityUsesCanonicalSourceInMessage(t *testing.T) {
	dir := t.TempDir()
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")
	writeDoltRepoMarker(t, filepath.Join(dir, ".beads", "dolt", "hq"))
	writeDoltRepoMarker(t, filepath.Join(dir, ".beads", "embeddeddolt", "legacy"))

	c := NewBDSplitStoreCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if strings.Contains(r.Message, "metadata.json dolt_mode=managed_city") {
		t.Fatalf("message = %q, should not label endpoint origin as metadata dolt_mode", r.Message)
	}
	if !strings.Contains(r.Message, "canonical endpoint_origin=managed_city") {
		t.Fatalf("message = %q, want canonical endpoint source", r.Message)
	}
}

func TestRigBDSplitStoreCheck_InheritedRigTreatsLocalReposAsLegacy(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, cityDir, "hq")
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")
	writeDoltRepoMarker(t, filepath.Join(rigDir, ".beads", "dolt", "de"))
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads", "embeddeddolt"), 0o700); err != nil {
		t.Fatal(err)
	}

	c := NewRigBDSplitStoreCheck(cityDir, config.Rig{Name: "demo", Path: rigDir})
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no active local store") {
		t.Fatalf("message = %q, want no active local store warning", r.Message)
	}
}

func TestRigBDSplitStoreCheck_BDBackedRigUnderFileCityUsesRigMetadata(t *testing.T) {
	cityDir := setupCity(t, `[workspace]
name = "demo"

[beads]
provider = "file"
`)
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")
	writeDoltRepoMarker(t, filepath.Join(rigDir, ".beads", "dolt", "de"))
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads", "embeddeddolt"), 0o700); err != nil {
		t.Fatal(err)
	}

	c := NewRigBDSplitStoreCheck(cityDir, config.Rig{Name: "demo", Path: rigDir})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, "inactive store is empty") {
		t.Fatalf("message = %q, want inactive-store-empty OK", r.Message)
	}
}

func TestRigBDSplitStoreCheck_ManagedExecProviderScriptUsesBDStore(t *testing.T) {
	cityDir := setupCity(t, `[workspace]
name = "demo"

[beads]
provider = "file"
`)
	t.Setenv("GC_BEADS", "exec:"+filepath.Join(cityDir, ".gc", "system", "packs", "bd", "assets", "scripts", "gc-beads-bd.sh"))
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")
	writeDoltRepoMarker(t, filepath.Join(rigDir, ".beads", "dolt", "de"))
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads", "embeddeddolt"), 0o700); err != nil {
		t.Fatal(err)
	}

	c := NewRigBDSplitStoreCheck(cityDir, config.Rig{Name: "demo", Path: rigDir})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s; details = %v", r.Status, r.Message, r.Details)
	}
	if !strings.Contains(r.Message, "inactive store is empty") {
		t.Fatalf("message = %q, want inactive-store-empty OK", r.Message)
	}
}

func TestRigBDSplitStoreCheck_InvalidExternalCityConfigUsesNeutralGuidance(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
	})
	writeDoctorCanonicalMetadata(t, fs, cityDir, "hq")
	if err := os.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")
	writeDoltRepoMarker(t, filepath.Join(rigDir, ".beads", "dolt", "de"))
	writeDoltRepoMarker(t, filepath.Join(rigDir, ".beads", "embeddeddolt", "legacy"))

	c := NewRigBDSplitStoreCheck(cityDir, config.Rig{Name: "demo", Path: rigDir})
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no active local store") {
		t.Fatalf("message = %q, want no active local store warning", r.Message)
	}
	if strings.Contains(r.Message, "active .beads/dolt") {
		t.Fatalf("message = %q, should not trust rig metadata when city external config is invalid", r.Message)
	}
	if strings.Contains(r.FixHint, "inactive store") {
		t.Fatalf("fix hint = %q, should not reference inactive store when active local store is unknown", r.FixHint)
	}
}

func TestBDSplitStoreCheck_UnknownActiveUsesNeutralRecoveryGuidance(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "dolt", "hq"))
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "embeddeddolt", "legacy"))

	c := NewBDSplitStoreCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if strings.Contains(r.FixHint, "inactive store") {
		t.Fatalf("fix hint = %q, should not reference an inactive store when active store is unknown", r.FixHint)
	}
	if !strings.Contains(r.FixHint, "current or intended active store") {
		t.Fatalf("fix hint = %q, want neutral active-store guidance", r.FixHint)
	}
}

func TestBDSplitStoreCheck_NonDoltLocalModeUsesNeutralRecoveryGuidance(t *testing.T) {
	dir := t.TempDir()
	beadsDir := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beadsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"database":"sqlite","backend":"sqlite","dolt_mode":"local"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "dolt", "hq"))
	writeDoltRepoMarker(t, filepath.Join(beadsDir, "embeddeddolt", "legacy"))

	c := NewBDSplitStoreCheck(dir)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if strings.Contains(r.Message, "active .beads/embeddeddolt") {
		t.Fatalf("message = %q, should not treat non-Dolt local mode as active embeddeddolt", r.Message)
	}
	if strings.Contains(r.FixHint, "inactive store") {
		t.Fatalf("fix hint = %q, should not reference inactive store for non-Dolt local mode", r.FixHint)
	}
}

func TestDoltReposUnderSkipsDetectedRepoWorktree(t *testing.T) {
	root := t.TempDir()
	writeDoltRepoMarker(t, filepath.Join(root, "hq"))
	writeDoltRepoMarker(t, filepath.Join(root, "hq", "nested"))

	repos, err := doltReposUnder(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(repos, ","), "hq"; got != want {
		t.Fatalf("repos = %q, want %q", got, want)
	}
}

func writeDoltRepoMarker(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".dolt", "noms"), 0o700); err != nil {
		t.Fatal(err)
	}
}

// spyPingStore is a minimal Store that records Ping calls.
type spyPingStore struct {
	beads.MemStore
	pingFunc func() error
}

func (s *spyPingStore) Ping() error {
	if s.pingFunc != nil {
		return s.pingFunc()
	}
	return nil
}

// --- DoltServerCheck ---

func TestDoltServerCheck_ManagedCityUsesRuntimeState(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	writeDoctorRuntimeState(t, fs, dir, port)

	c := NewDoltServerCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "127.0.0.1:"+port) {
		t.Fatalf("message = %q, want runtime port %s", r.Message, port)
	}
}

func TestDoltServerCheck_ManagedCityReportsStartHint(t *testing.T) {
	dir := t.TempDir()
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	writeDoctorRuntimeState(t, fs, dir, port)

	c := NewDoltServerCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "resolve dolt target") {
		t.Fatalf("message = %q, want resolve failure when runtime state is unavailable", r.Message)
	}
	if !strings.Contains(r.FixHint, "gc start") {
		t.Fatalf("fix hint = %q, want gc start hint", r.FixHint)
	}
}

func TestDoltServerCheck_ManagedCityRejectsInvalidRuntimeStateEvenWhenPortReachable(t *testing.T) {
	dir := t.TempDir()
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	runtimeDir := filepath.Join(dir, ".gc", "runtime", "packs", "dolt")
	if err := fs.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := fmt.Sprintf(`{"running":true,"pid":%d,"port":%s,"data_dir":%q}`, os.Getpid(), port, filepath.Join(dir, ".beads", "wrong"))
	if err := fs.WriteFile(filepath.Join(runtimeDir, "dolt-state.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewDoltServerCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "resolve dolt target") {
		t.Fatalf("message = %q, want resolve failure for invalid runtime state", r.Message)
	}
	if strings.Contains(r.Message, "127.0.0.1:"+port) {
		t.Fatalf("message = %q, want invalid runtime state to fail before TCP fallback", r.Message)
	}
}

func TestDoltServerCheck_ExternalCityUsesCanonicalTarget(t *testing.T) {
	dir := t.TempDir()
	fs := fsys.OSFS{}
	port := strconv.Itoa(4411)

	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       port,
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")

	c := NewDoltServerCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "db.example.com:"+port) {
		t.Fatalf("message = %q, want canonical external target %s", r.Message, port)
	}
	if strings.Contains(r.Message, "127.0.0.1:") {
		t.Fatalf("message = %q, want canonical host not localhost heuristic", r.Message)
	}
	if !strings.Contains(r.FixHint, "external") {
		t.Fatalf("fix hint = %q, want external endpoint hint", r.FixHint)
	}
	if strings.Contains(r.FixHint, "gc start") {
		t.Fatalf("fix hint = %q, want external hint instead of gc start", r.FixHint)
	}
}

func TestDoltServerCheck_LegacyExternalCityUsesExternalHint(t *testing.T) {
	dir := t.TempDir()
	fs := fsys.OSFS{}
	port := strconv.Itoa(4412)

	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix: "gc",
		DoltHost:    "db.example.com",
		DoltPort:    port,
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")

	c := NewDoltServerCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "db.example.com:"+port) {
		t.Fatalf("message = %q, want legacy external target %s", r.Message, port)
	}
	if !strings.Contains(r.FixHint, "external") {
		t.Fatalf("fix hint = %q, want external endpoint hint", r.FixHint)
	}
	if strings.Contains(r.FixHint, "gc start") {
		t.Fatalf("fix hint = %q, want external hint instead of gc start", r.FixHint)
	}
}

func TestDoltServerCheck_InvalidCityExplicitOriginFailsResolution(t *testing.T) {
	dir := t.TempDir()
	fs := fsys.OSFS{}

	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "4411",
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")

	c := NewDoltServerCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "invalid for city scope") {
		t.Fatalf("message = %q, want city-scope origin rejection", r.Message)
	}
}

func TestDoltServerCheck_Skipped(t *testing.T) {
	c := NewDoltServerCheck("/tmp", true)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK (skipped)", r.Status)
	}
}

func TestRigDoltServerCheck_ExplicitRigUsesCanonicalTarget(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}

	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, cityDir, "hq")
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "rig-db.example.com",
		DoltPort:       "4406",
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

	c := NewRigDoltServerCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "rig-db.example.com:4406") {
		t.Fatalf("message = %q, want explicit rig target", r.Message)
	}
	if strings.Contains(r.FixHint, "gc start") {
		t.Fatalf("fix hint = %q, want external hint instead of gc start", r.FixHint)
	}
}

func TestRigHasExplicitEndpointConfigLegacyExplicitRig(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}

	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
	})
	writeDoctorCanonicalMetadata(t, fs, cityDir, "hq")

	if err := fs.MkdirAll(filepath.Join(rigDir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rigDir, ".beads", "config.yaml"), []byte(`issue_prefix: de
dolt.host: rig-db.example.com
dolt.port: 4406
`), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

	explicit, err := contract.ScopeUsesExplicitEndpoint(fs, cityDir, rigDir)
	if err != nil {
		t.Fatalf("ScopeUsesExplicitEndpoint() error = %v", err)
	}
	if !explicit {
		t.Fatal("ScopeUsesExplicitEndpoint() = false, want true for legacy explicit rig")
	}
}

func TestRigDoltServerCheck_LegacyExplicitRigUsesDerivedTarget(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}

	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
	})
	writeDoctorCanonicalMetadata(t, fs, cityDir, "hq")
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix: "de",
		DoltHost:    "rig-db.example.com",
		DoltPort:    "4406",
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

	c := NewRigDoltServerCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "rig-db.example.com:4406") {
		t.Fatalf("message = %q, want derived explicit rig target", r.Message)
	}
	if strings.Contains(r.FixHint, "gc start") {
		t.Fatalf("fix hint = %q, want external hint instead of gc start", r.FixHint)
	}
}

func TestRigDoltServerCheck_ExplicitRigReachable(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, cityDir, "hq")
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginExplicit,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "127.0.0.1",
		DoltPort:       port,
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

	c := NewRigDoltServerCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "127.0.0.1:"+port) {
		t.Fatalf("message = %q, want reachable explicit rig target", r.Message)
	}
}

func TestRigDoltServerCheck_InheritedRigIsSkipped(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}

	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
	})
	writeDoctorCanonicalMetadata(t, fs, cityDir, "hq")
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

	c := NewRigDoltServerCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "inherits city") {
		t.Fatalf("message = %q, want inherited-city skip message", r.Message)
	}
}

func TestRigDoltServerCheck_InheritedRigDriftIsError(t *testing.T) {
	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "demo")
	fs := fsys.OSFS{}

	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "db.example.com",
		DoltPort:       "3307",
	})
	writeDoctorCanonicalMetadata(t, fs, cityDir, "hq")
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
		DoltHost:       "stale.example.com",
		DoltPort:       "5507",
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

	c := NewRigDoltServerCheck(cityDir, config.Rig{Name: "demo", Path: rigDir}, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "inherited city endpoint drift") {
		t.Fatalf("message = %q, want inherited drift message", r.Message)
	}
	if !strings.Contains(r.FixHint, "inherited city endpoint mirror") {
		t.Fatalf("fix hint = %q, want inherited mirror reconciliation", r.FixHint)
	}
}

func TestBeadsStoreCheck_ManagedCityMissingRuntimeStateFailsBeforePing(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")

	pinged := false
	spy := &spyPingStore{pingFunc: func() error {
		pinged = true
		return nil
	}}
	c := NewBeadsStoreCheck(dir, func(_ string) (beads.Store, error) { return spy, nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "resolve dolt target") {
		t.Fatalf("message = %q, want resolve error", r.Message)
	}
	if !strings.Contains(r.FixHint, "gc start") {
		t.Fatalf("fix hint = %q, want gc start hint", r.FixHint)
	}
	if pinged {
		t.Fatal("Ping should not run when managed runtime state is missing")
	}
}

func TestBeadsStoreCheck_ExternalCityUnavailableFailsBeforePing(t *testing.T) {
	dir := setupCity(t, "[workspace]\nname = \"test\"\n")
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusUnverified,
		DoltHost:       "127.0.0.1",
		DoltPort:       "4416",
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")

	pinged := false
	spy := &spyPingStore{pingFunc: func() error {
		pinged = true
		return nil
	}}
	c := NewBeadsStoreCheck(dir, func(_ string) (beads.Store, error) { return spy, nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "dolt server not reachable at 127.0.0.1:4416") {
		t.Fatalf("message = %q, want normalized reachability error", r.Message)
	}
	if !strings.Contains(r.FixHint, "external") {
		t.Fatalf("fix hint = %q, want external endpoint hint", r.FixHint)
	}
	if pinged {
		t.Fatal("Ping should not run when external endpoint is unreachable")
	}
}

func TestBeadsStoreCheck_ExecGcBeadsBdExternalCityUnavailableFailsBeforePing(t *testing.T) {
	dir := setupCity(t, `[workspace]
name = "test"
[beads]
provider = "exec:/tmp/gc-beads-bd"
`)
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusUnverified,
		DoltHost:       "127.0.0.1",
		DoltPort:       "4417",
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")

	pinged := false
	spy := &spyPingStore{pingFunc: func() error {
		pinged = true
		return nil
	}}
	c := NewBeadsStoreCheck(dir, func(_ string) (beads.Store, error) { return spy, nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "dolt server not reachable at 127.0.0.1:4417") {
		t.Fatalf("message = %q, want normalized reachability error", r.Message)
	}
	if !strings.Contains(r.FixHint, "external") {
		t.Fatalf("fix hint = %q, want external endpoint hint", r.FixHint)
	}
	if pinged {
		t.Fatal("Ping should not run when exec:gc-beads-bd external endpoint is unreachable")
	}
}

func TestBeadsStoreCheck_GCBeadsExecOverrideExternalCityUnavailableFailsBeforePing(t *testing.T) {
	dir := setupCity(t, `[workspace]
name = "test"
[beads]
provider = "file"
`)
	t.Setenv("GC_BEADS", "exec:/tmp/gc-beads-bd")
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusUnverified,
		DoltHost:       "127.0.0.1",
		DoltPort:       "4418",
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")

	pinged := false
	spy := &spyPingStore{pingFunc: func() error {
		pinged = true
		return nil
	}}
	c := NewBeadsStoreCheck(dir, func(_ string) (beads.Store, error) { return spy, nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "dolt server not reachable at 127.0.0.1:4418") {
		t.Fatalf("message = %q, want normalized reachability error", r.Message)
	}
	if !strings.Contains(r.FixHint, "external") {
		t.Fatalf("fix hint = %q, want external endpoint hint", r.FixHint)
	}
	if pinged {
		t.Fatal("Ping should not run when GC_BEADS exec override makes the city bd-backed")
	}
}

func TestBeadsStoreCheck_GCBeadsFileOverrideSkipsBdPreflight(t *testing.T) {
	dir := setupCity(t, `[workspace]
name = "test"
`)
	t.Setenv("GC_BEADS", "file")
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginCityCanonical,
		EndpointStatus: contract.EndpointStatusUnverified,
		DoltHost:       "127.0.0.1",
		DoltPort:       "4419",
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")

	pinged := false
	spy := &spyPingStore{pingFunc: func() error {
		pinged = true
		return nil
	}}
	c := NewBeadsStoreCheck(dir, func(_ string) (beads.Store, error) { return spy, nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !pinged {
		t.Fatal("Ping should run when GC_BEADS=file overrides bd-backed defaults")
	}
}

func TestRigBeadsCheck_ManagedInheritedMissingRuntimeStateFailsBeforePing(t *testing.T) {
	cityDir := setupCity(t, "[workspace]\nname = \"test\"\n")
	rigDir := filepath.Join(cityDir, "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, cityDir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, cityDir, "hq")
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "fr",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "fr")

	pinged := false
	spy := &spyPingStore{pingFunc: func() error {
		pinged = true
		return nil
	}}
	c := NewRigBeadsCheck(cityDir, config.Rig{Name: "frontend", Path: rigDir}, func(_ string) (beads.Store, error) { return spy, nil })
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "resolve dolt target") {
		t.Fatalf("message = %q, want resolve error", r.Message)
	}
	if !strings.Contains(r.FixHint, "gc start") {
		t.Fatalf("fix hint = %q, want gc start hint", r.FixHint)
	}
	if pinged {
		t.Fatal("Ping should not run when inherited managed runtime state is missing")
	}
}

//nolint:unparam // helper keeps FS explicit in tests
func writeDoctorCanonicalConfig(t *testing.T, fs fsys.FS, dir string, state contract.ConfigState) {
	t.Helper()
	if err := fs.MkdirAll(filepath.Join(dir, ".beads"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := contract.EnsureCanonicalConfig(fs, filepath.Join(dir, ".beads", "config.yaml"), state); err != nil {
		t.Fatal(err)
	}
}

//nolint:unparam // helper keeps FS explicit in tests
func writeDoctorCanonicalMetadata(t *testing.T, fs fsys.FS, dir, db string) {
	t.Helper()
	if _, err := contract.EnsureCanonicalMetadata(fs, filepath.Join(dir, ".beads", "metadata.json"), contract.MetadataState{
		Database:     "dolt",
		Backend:      "dolt",
		DoltMode:     "server",
		DoltDatabase: db,
	}); err != nil {
		t.Fatal(err)
	}
}

func writeDoctorRuntimeState(t *testing.T, fs fsys.FS, dir, port string) {
	t.Helper()
	runtimeDir := filepath.Join(dir, ".gc", "runtime", "packs", "dolt")
	if err := fs.MkdirAll(runtimeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	state := fmt.Sprintf(`{"running":true,"pid":%d,"port":%s,"data_dir":%q}`,
		os.Getpid(),
		port,
		filepath.Join(dir, ".beads", "dolt"),
	)
	if err := fs.WriteFile(filepath.Join(runtimeDir, "dolt-state.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
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
	c := NewRigBeadsCheck(dir, config.Rig{Name: "myrig", Path: dir}, func(rigPath string) (beads.Store, error) {
		return beads.OpenFileStore(fsys.OSFS{}, filepath.Join(rigPath, "beads.json"))
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Errorf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestRigBeadsCheck_Error(t *testing.T) {
	c := NewRigBeadsCheck(t.TempDir(), config.Rig{Name: "myrig", Path: "/nonexistent"}, func(_ string) (beads.Store, error) {
		return nil, fmt.Errorf("store failed")
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Errorf("status = %d, want Error", r.Status)
	}
}

func TestRigBeadsCheck_UsesPing(t *testing.T) {
	pinged := false
	spy := &spyPingStore{
		pingFunc: func() error {
			pinged = true
			return nil
		},
	}
	rigDir := t.TempDir()
	c := NewRigBeadsCheck(rigDir, config.Rig{Name: "myrig", Path: rigDir}, func(_ string) (beads.Store, error) {
		return spy, nil
	})
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !pinged {
		t.Error("Ping was not called")
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

// --- DoltNomsSizeCheck ---

// setupManagedDoltCity creates a minimal managed-bd/Dolt city in a temp dir
// and returns its path. Runtime state is written for the pinned database.
func setupManagedDoltCity(t *testing.T) string {
	t.Helper()
	t.Setenv("GC_DOLT_DATA_DIR", "")
	t.Setenv("GC_DOLT_CONFIG_FILE", "")
	const db = "hq"
	dir := t.TempDir()
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	if err := os.MkdirAll(filepath.Join(dir, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDoctorCanonicalMetadata(t, fs, dir, db)

	// Provide a reachable runtime state so ResolveDoltConnectionTarget
	// returns a valid target.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	writeDoctorRuntimeState(t, fs, dir, port)
	return dir
}

func setupFreshManagedDoltCity(t *testing.T) string {
	t.Helper()
	t.Setenv("GC_DOLT_DATA_DIR", "")
	t.Setenv("GC_DOLT_CONFIG_FILE", "")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// writeFakeFile creates a file at path of exactly size bytes (zero-filled).
func writeFakeFile(t *testing.T, path string, size int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path) //nolint:gosec // test helper
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test helper
	if size > 0 {
		if err := f.Truncate(size); err != nil {
			t.Fatal(err)
		}
	}
}

func newTestDoltNomsSizeCheck(cityPath string, skip bool) *DoltNomsSizeCheck {
	c := NewDoltNomsSizeCheck(cityPath, skip)
	c.measureDir = sumDirBytes
	return c
}

func TestDoltNomsSizeCheck_Skipped(t *testing.T) {
	c := newTestDoltNomsSizeCheck(t.TempDir(), true)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK (skipped); msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "skipped") {
		t.Errorf("message = %q, want skipped", r.Message)
	}
}

func TestDoltNomsSizeCheck_NoDataYet(t *testing.T) {
	dir := setupManagedDoltCity(t)
	// No .beads/dolt/hq/.dolt on disk.
	c := newTestDoltNomsSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "no dolt data yet") {
		t.Errorf("message = %q, want no-data message", r.Message)
	}
}

func TestDoltNomsSizeCheck_SkipsExternalTargets(t *testing.T) {
	t.Run("external city", func(t *testing.T) {
		dir := setupCity(t, "[workspace]\nname = \"test\"\n")
		fs := fsys.OSFS{}
		writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
			IssuePrefix:    "gc",
			EndpointOrigin: contract.EndpointOriginCityCanonical,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltHost:       "db.example.com",
			DoltPort:       "4411",
		})
		writeDoctorCanonicalMetadata(t, fs, dir, "hq")

		c := newTestDoltNomsSizeCheck(dir, false)
		r := c.Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK skip; msg = %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "skipped") {
			t.Fatalf("message = %q, want skipped", r.Message)
		}
	})

	t.Run("external rig", func(t *testing.T) {
		dir := setupCity(t, `[workspace]
name = "test"

[beads]
provider = "file"

[[rigs]]
name = "demo"
path = "demo"
`)
		rigDir := filepath.Join(dir, "demo")
		fs := fsys.OSFS{}
		writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
			IssuePrefix:    "de",
			EndpointOrigin: contract.EndpointOriginExplicit,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltHost:       "rig-db.example.com",
			DoltPort:       "4406",
		})
		writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

		c := newTestDoltNomsSizeCheck(dir, false)
		r := c.Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK skip; msg = %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "skipped") {
			t.Fatalf("message = %q, want skipped", r.Message)
		}
	})

	t.Run("inherited external city", func(t *testing.T) {
		dir := setupCity(t, `[workspace]
name = "test"

[[rigs]]
name = "demo"
path = "demo"
`)
		rigDir := filepath.Join(dir, "demo")
		fs := fsys.OSFS{}
		writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
			IssuePrefix:    "gc",
			EndpointOrigin: contract.EndpointOriginCityCanonical,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltHost:       "db.example.com",
			DoltPort:       "4411",
		})
		writeDoctorCanonicalMetadata(t, fs, dir, "hq")
		writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
			IssuePrefix:    "de",
			EndpointOrigin: contract.EndpointOriginInheritedCity,
			EndpointStatus: contract.EndpointStatusVerified,
		})
		writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

		if ManagedLocalDoltChecksApplicable(dir) {
			t.Fatal("ManagedLocalDoltChecksApplicable() = true, want false for inherited external city endpoint")
		}
		c := newTestDoltNomsSizeCheck(dir, false)
		r := c.Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK skip; msg = %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "skipped") {
			t.Fatalf("message = %q, want skipped", r.Message)
		}
	})
}

func TestDoltNomsSizeCheck_OKUnderThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	// 1 MB of data — well under 2 GB warn.
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "hq", ".dolt", "noms", "file1"), 1024*1024)
	c := newTestDoltNomsSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "footprint") {
		t.Errorf("message = %q, want footprint description", r.Message)
	}
}

func TestDuDirBytes_NonSparseFile(t *testing.T) {
	dir := t.TempDir()
	data := make([]byte, 64*1024)
	for i := range data {
		data[i] = 'x'
	}
	if err := os.WriteFile(filepath.Join(dir, "chunk"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	total, exists, err := duDirBytes(dir)
	if err != nil {
		t.Fatalf("duDirBytes: %v", err)
	}
	if !exists {
		t.Fatal("duDirBytes exists = false, want true")
	}
	if total < int64(len(data)) {
		t.Fatalf("duDirBytes total = %d, want at least %d", total, len(data))
	}
}

func TestDoltNomsSizeCheck_WarnAtThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	// 3 GB — above warn (2 GB), below error (20 GB). Sparse file (Truncate)
	// does not actually allocate disk, but reported size is 3 GB which
	// is what our sum uses.
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "hq", ".dolt", "noms", "big"), 3*1024*1024*1024)
	c := newTestDoltNomsSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "approaching threshold") {
		t.Errorf("message = %q, want approaching-threshold text", r.Message)
	}
	if !strings.Contains(r.FixHint, "dolt-bloat-recovery") {
		t.Errorf("fix hint = %q, want bloat-recovery doc reference", r.FixHint)
	}
}

func TestDoltNomsSizeCheck_ErrorAtThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	// 21 GB — above error (20 GB).
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "hq", ".dolt", "noms", "huge"), 21*1024*1024*1024)
	c := newTestDoltNomsSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "excessive") {
		t.Errorf("message = %q, want excessive text", r.Message)
	}
}

func TestDoltNomsSizeCheck_RigDatabaseWarnAtThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	rigDir := filepath.Join(dir, "demo")
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[[rigs]]
name = "demo"
path = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "de", ".dolt", "noms", "big"), 3*1024*1024*1024)

	c := newTestDoltNomsSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "de") {
		t.Fatalf("message = %q, want rig database name", r.Message)
	}
}

func TestDoltNomsSizeCheck_ConfigErrorScansManagedRigMetadata(t *testing.T) {
	dir := setupManagedDoltCity(t)
	rigDir := filepath.Join(dir, "demo")
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

	c := NewDoltNomsSizeCheckForConfig(dir, false, nil, os.ErrInvalid)
	c.measureDir = func(path string) (int64, bool, error) {
		if strings.Contains(path, string(filepath.Separator)+"de"+string(filepath.Separator)+".dolt") {
			return 3 * 1024 * 1024 * 1024, true, nil
		}
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "de") {
		t.Fatalf("message = %q, want filesystem-discovered rig database name", r.Message)
	}
}

func TestManagedLocalDoltChecksApplicable_ConfigErrorScansManagedRigMetadata(t *testing.T) {
	dir := setupManagedDoltCity(t)
	rigDir := filepath.Join(dir, "demo")
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

	if !ManagedLocalDoltChecksApplicable(dir) {
		t.Fatal("ManagedLocalDoltChecksApplicable() = false, want true for filesystem-discovered managed rig metadata")
	}
}

func TestDoltNomsSizeCheck_AggregateWarnAtThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	rigDir := filepath.Join(dir, "demo")
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(`[workspace]
name = "demo"

[beads]
provider = "bd"

[[rigs]]
name = "demo"
path = "demo"
`), 0o644); err != nil {
		t.Fatal(err)
	}

	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
		IssuePrefix:    "de",
		EndpointOrigin: contract.EndpointOriginInheritedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, rigDir, "de")
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "hq", ".dolt", "noms", "one"), 1536*1024*1024)
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "de", ".dolt", "noms", "two"), 1536*1024*1024)

	c := newTestDoltNomsSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "aggregate") {
		t.Fatalf("message = %q, want aggregate threshold warning", r.Message)
	}
}

func TestDoltNomsSizeCheck_OrphanDatabaseWarnAtThreshold(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeFakeFile(t, filepath.Join(dir, ".beads", "dolt", "orphan-db", ".dolt", "noms", "big"), 3*1024*1024*1024)

	c := newTestDoltNomsSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "orphan database orphan-db") {
		t.Fatalf("message = %q, want orphan database name", r.Message)
	}
}

func TestDoltNomsSizeCheck_SkipsSystemDatabaseMetadata(t *testing.T) {
	dir := setupManagedDoltCity(t)
	if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(`{"dolt_database":"mysql"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".beads", "dolt", "mysql", ".dolt"), 0o755); err != nil {
		t.Fatal(err)
	}

	c := NewDoltNomsSizeCheck(dir, false)
	c.measureDir = func(path string) (int64, bool, error) {
		if strings.Contains(path, string(filepath.Separator)+"mysql"+string(filepath.Separator)) {
			t.Fatalf("measureDir called for system database path %s", path)
		}
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestDoltNomsSizeCheck_SkipsInvalidDatabaseMetadata(t *testing.T) {
	dir := setupManagedDoltCity(t)
	if err := os.WriteFile(filepath.Join(dir, ".beads", "metadata.json"), []byte(`{"dolt_database":"../../outside"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewDoltNomsSizeCheck(dir, false)
	c.measureDir = func(path string) (int64, bool, error) {
		if strings.Contains(path, "outside") {
			t.Fatalf("measureDir called for invalid database path %s", path)
		}
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
}

func TestDoltNomsSizeCheck_LegacyManagedDataDirWarnAtThreshold(t *testing.T) {
	dir := t.TempDir()
	fs := fsys.OSFS{}
	writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
		IssuePrefix:    "gc",
		EndpointOrigin: contract.EndpointOriginManagedCity,
		EndpointStatus: contract.EndpointStatusVerified,
	})
	writeDoctorCanonicalMetadata(t, fs, dir, "hq")
	if err := os.MkdirAll(filepath.Join(dir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFakeFile(t, filepath.Join(dir, ".gc", "dolt-data", "hq", ".dolt", "noms", "big"), 3*1024*1024*1024)

	c := newTestDoltNomsSizeCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "hq") {
		t.Fatalf("message = %q, want legacy managed database name", r.Message)
	}
}

func TestDoltNomsSizeCheck_IgnoresAmbientDataDirOverride(t *testing.T) {
	dir := setupManagedDoltCity(t)
	t.Setenv("GC_DOLT_DATA_DIR", filepath.Join(t.TempDir(), "wrong-dolt-data"))

	c := NewDoltNomsSizeCheck(dir, false)
	c.measureDir = func(path string) (int64, bool, error) {
		if strings.Contains(path, "wrong-dolt-data") {
			t.Fatalf("measureDir used ambient data override path %s", path)
		}
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK ignoring ambient data override; msg = %s", r.Status, r.Message)
	}
}

func TestDoltNomsSizeCheck_UsesPublishedRuntimeDataDir(t *testing.T) {
	dir := setupManagedDoltCity(t)
	dataDir := filepath.Join(t.TempDir(), "relocated-dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	port := ln.Addr().(*net.TCPAddr).Port
	statePath := filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	state := fmt.Sprintf(`{"running":true,"pid":%d,"port":%d,"data_dir":%q}`, os.Getpid(), port, dataDir)
	if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewDoltNomsSizeCheck(dir, false)
	c.measureDir = func(path string) (int64, bool, error) {
		if strings.Contains(path, "relocated-dolt-data") {
			return 3 * 1024 * 1024 * 1024, true, nil
		}
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning for published relocated data dir; msg = %s", r.Status, r.Message)
	}
}

func TestDoltNomsSizeCheck_IgnoresPublishedRuntimeDataDirWithUnreachablePort(t *testing.T) {
	dir := setupManagedDoltCity(t)
	dataDir := filepath.Join(t.TempDir(), "unreachable-port-dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if err := ln.Close(); err != nil {
		t.Fatalf("Close listener: %v", err)
	}
	statePath := filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	state := fmt.Sprintf(`{"running":true,"pid":%d,"port":%d,"data_dir":%q}`, os.Getpid(), port, dataDir)
	if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewDoltNomsSizeCheck(dir, false)
	c.measureDir = func(path string) (int64, bool, error) {
		if strings.Contains(path, "unreachable-port-dolt-data") {
			t.Fatalf("measureDir used stale running data dir %s", path)
		}
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK after ignoring unreachable published state; msg = %s", r.Status, r.Message)
	}
}

func TestDoltNomsSizeCheck_UsesStoppedPublishedRuntimeDataDir(t *testing.T) {
	dir := setupManagedDoltCity(t)
	if err := os.Remove(filepath.Join(dir, ".beads", "dolt")); err != nil {
		t.Fatal(err)
	}
	dataDir := filepath.Join(t.TempDir(), "relocated-stopped-dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	state := fmt.Sprintf(`{"running":false,"pid":0,"port":0,"data_dir":%q}`, dataDir)
	if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewDoltNomsSizeCheck(dir, false)
	c.measureDir = func(path string) (int64, bool, error) {
		if strings.Contains(path, "relocated-stopped-dolt-data") {
			return 3 * 1024 * 1024 * 1024, true, nil
		}
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning for stopped published data dir; msg = %s", r.Status, r.Message)
	}
}

func TestDoltNomsSizeCheck_IgnoresStaleStoppedPublishedRuntimeDataDir(t *testing.T) {
	dir := setupManagedDoltCity(t)
	dataDir := filepath.Join(t.TempDir(), "stale-stopped-dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	state := fmt.Sprintf(`{"running":false,"pid":0,"port":0,"data_dir":%q}`, dataDir)
	if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewDoltNomsSizeCheck(dir, false)
	c.measureDir = func(path string) (int64, bool, error) {
		if strings.Contains(path, "stale-stopped-dolt-data") {
			t.Fatalf("measureDir used stale stopped data dir %s", path)
		}
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK after ignoring stale stopped state; msg = %s", r.Status, r.Message)
	}
}

func TestDoltNomsSizeCheck_IgnoresMissingPublishedRuntimeDataDir(t *testing.T) {
	dir := setupManagedDoltCity(t)
	dataDir := filepath.Join(t.TempDir(), "missing-relocated-dolt-data")
	statePath := filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	state := fmt.Sprintf(`{"running":false,"pid":0,"port":0,"data_dir":%q}`, dataDir)
	if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewDoltNomsSizeCheck(dir, false)
	c.measureDir = func(path string) (int64, bool, error) {
		if strings.Contains(path, "missing-relocated-dolt-data") {
			t.Fatalf("measureDir used missing published data dir %s", path)
		}
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK after falling back from missing data dir; msg = %s", r.Status, r.Message)
	}
}

func TestDoltNomsSizeCheck_IgnoresStaleRunningPublishedRuntimeDataDir(t *testing.T) {
	dir := setupManagedDoltCity(t)
	dataDir := filepath.Join(t.TempDir(), "stale-running-dolt-data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(dir, ".gc", "runtime", "packs", "dolt", "dolt-state.json")
	state := fmt.Sprintf(`{"running":true,"pid":99999999,"port":3307,"data_dir":%q}`, dataDir)
	if err := os.WriteFile(statePath, []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}

	c := NewDoltNomsSizeCheck(dir, false)
	c.measureDir = func(path string) (int64, bool, error) {
		if strings.Contains(path, "stale-running-dolt-data") {
			t.Fatalf("measureDir used stale running data dir %s", path)
		}
		return 0, false, nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK after ignoring stale running state; msg = %s", r.Status, r.Message)
	}
}

func TestDoltNomsSizeCheck_CanFixFalse(t *testing.T) {
	c := newTestDoltNomsSizeCheck(t.TempDir(), false)
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
}

// --- DoltConfigCheck ---

// writeDoctorManagedDoltConfig writes a config.yaml at the canonical managed
// path under city. values overrides individual key defaults; any value set to
// the sentinel string "__missing__" is omitted entirely.
func writeDoctorManagedDoltConfig(t *testing.T, cityPath string, overrides map[string]any) {
	t.Helper()
	defaults := map[string]any{
		"log_level": "warning",
		"listener": map[string]any{
			"port":                           "3307",
			"host":                           "127.0.0.1",
			"max_connections":                1000,
			"back_log":                       50,
			"max_connections_timeout_millis": 5000,
			"read_timeout_millis":            300000,
			"write_timeout_millis":           300000,
		},
		"data_dir": filepath.Join(cityPath, ".beads", "dolt"),
		"behavior": map[string]any{
			"auto_gc_behavior": map[string]any{
				"enable":        true,
				"archive_level": 1,
			},
		},
	}
	for k, v := range overrides {
		// Dotted override paths write into the nested map.
		parts := strings.Split(k, ".")
		cur := defaults
		for i, p := range parts {
			if i == len(parts)-1 {
				if v == "__missing__" {
					delete(cur, p)
				} else {
					cur[p] = v
				}
				break
			}
			next, ok := cur[p].(map[string]any)
			if !ok {
				next = map[string]any{}
				cur[p] = next
			}
			cur = next
		}
	}

	packStateDir := filepath.Dir(resolveManagedDoltConfigPath(cityPath))
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Using yaml.v3 via the doctor package's import is not re-exported;
	// hand-render instead.
	var b strings.Builder
	renderDoctorTestYAML(&b, defaults, 0)
	if err := os.WriteFile(resolveManagedDoltConfigPath(cityPath), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// renderDoctorTestYAML hand-renders a nested map[string]any as YAML. Kept
// minimal; sufficient for dolt-config.yaml test fixtures.
func renderDoctorTestYAML(b *strings.Builder, m map[string]any, indent int) {
	// Sort keys for determinism.
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	pad := strings.Repeat(" ", indent)
	for _, k := range keys {
		v := m[k]
		switch vv := v.(type) {
		case map[string]any:
			fmt.Fprintf(b, "%s%s:\n", pad, k)
			renderDoctorTestYAML(b, vv, indent+2)
		case string:
			fmt.Fprintf(b, "%s%s: %q\n", pad, k, vv)
		case bool:
			fmt.Fprintf(b, "%s%s: %t\n", pad, k, vv)
		case int:
			fmt.Fprintf(b, "%s%s: %d\n", pad, k, vv)
		default:
			fmt.Fprintf(b, "%s%s: %v\n", pad, k, vv)
		}
	}
}

func TestDoltConfigCheck_Skipped(t *testing.T) {
	c := NewDoltConfigCheck(t.TempDir(), true)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "skipped") {
		t.Errorf("message = %q, want skipped", r.Message)
	}
}

func TestDoltConfigCheck_MissingFile(t *testing.T) {
	dir := setupManagedDoltCity(t)
	c := NewDoltConfigCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "not found") {
		t.Errorf("message = %q, want not-found", r.Message)
	}
	if !strings.Contains(r.FixHint, "gc start") {
		t.Errorf("fix hint = %q, want gc start reference", r.FixHint)
	}
}

func TestDoltConfigCheck_FreshManagedCityNotYetGenerated(t *testing.T) {
	dir := setupFreshManagedDoltCity(t)
	if workspaceHasLocalManagedDoltTarget(dir) {
		t.Fatal("workspaceHasLocalManagedDoltTarget() = true, want false for fresh managed city")
	}

	c := NewDoltConfigCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "not yet generated") {
		t.Fatalf("message = %q, want not-yet-generated text", r.Message)
	}
	if strings.Contains(r.Message, "skipped") {
		t.Fatalf("message = %q, want explicit fresh-managed-city status", r.Message)
	}
}

func TestDoltConfigCheck_OK(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDoctorManagedDoltConfig(t, dir, nil)
	c := NewDoltConfigCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "OK") {
		t.Errorf("message = %q, want OK text", r.Message)
	}
}

func TestDoltConfigCheck_UsesTrustedCityRuntimeDir(t *testing.T) {
	dir := setupManagedDoltCity(t)
	customRuntimeDir := filepath.Join(t.TempDir(), "runtime-root")
	t.Setenv("GC_CITY_PATH", dir)
	t.Setenv("GC_CITY_RUNTIME_DIR", customRuntimeDir)
	packStateDir := doctorDoltPackStateDir(dir)
	if err := os.MkdirAll(packStateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	state := fmt.Sprintf(`{"running":true,"pid":%d,"port":%d,"data_dir":%q}`,
		os.Getpid(),
		ln.Addr().(*net.TCPAddr).Port,
		filepath.Join(dir, ".beads", "dolt"),
	)
	if err := os.WriteFile(filepath.Join(packStateDir, "dolt-state.json"), []byte(state), 0o644); err != nil {
		t.Fatal(err)
	}
	writeDoctorManagedDoltConfig(t, dir, nil)

	c := NewDoltConfigCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK with trusted city runtime dir; msg = %s", r.Status, r.Message)
	}
}

func TestDoltConfigCheck_AcceptsSymlinkEquivalentDataDir(t *testing.T) {
	dir := setupManagedDoltCity(t)
	linkPath := filepath.Join(dir, "dolt-data-link")
	if err := os.Symlink(filepath.Join(dir, ".beads", "dolt"), linkPath); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	writeDoctorManagedDoltConfig(t, dir, map[string]any{
		"data_dir": linkPath,
	})
	c := NewDoltConfigCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK for symlink-equivalent data_dir; msg = %s", r.Status, r.Message)
	}
}

func TestDoltConfigCheck_IgnoresAmbientConfigOverride(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDoctorManagedDoltConfig(t, dir, nil)
	t.Setenv("GC_DOLT_CONFIG_FILE", filepath.Join(t.TempDir(), "missing-dolt-config.yaml"))

	c := NewDoltConfigCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK ignoring ambient config override; msg = %s", r.Status, r.Message)
	}
}

func TestDoltConfigCheck_MissingKey(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDoctorManagedDoltConfig(t, dir, map[string]any{
		"listener.back_log": "__missing__",
	})
	c := NewDoltConfigCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "listener.back_log") {
		t.Errorf("message = %q, want listener.back_log mention", r.Message)
	}
	if !strings.Contains(r.FixHint, "gc dolt stop") {
		t.Errorf("fix hint = %q, want stop/restart hint", r.FixHint)
	}
}

func TestDoltConfigCheck_WrongValue(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDoctorManagedDoltConfig(t, dir, map[string]any{
		"listener.max_connections": 500,
	})
	c := NewDoltConfigCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "max_connections") {
		t.Errorf("message = %q, want max_connections mention", r.Message)
	}
}

func TestDoltConfigCheck_WrongDataDir(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDoctorManagedDoltConfig(t, dir, map[string]any{
		"data_dir": filepath.Join(t.TempDir(), "wrong-dolt-data"),
	})
	c := NewDoltConfigCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "data_dir") {
		t.Errorf("message = %q, want data_dir mention", r.Message)
	}
}

func TestDoltConfigCheck_AutoGCDisabled(t *testing.T) {
	dir := setupManagedDoltCity(t)
	writeDoctorManagedDoltConfig(t, dir, map[string]any{
		"behavior.auto_gc_behavior.enable": false,
	})
	c := NewDoltConfigCheck(dir, false)
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "auto_gc_behavior.enable") {
		t.Errorf("message = %q, want auto_gc_behavior.enable mention", r.Message)
	}
}

func TestDoltConfigCheck_CanFixFalse(t *testing.T) {
	c := NewDoltConfigCheck(t.TempDir(), false)
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
}

func TestDoltConfigCheck_SkipsExternalTargets(t *testing.T) {
	t.Run("external city", func(t *testing.T) {
		dir := setupCity(t, "[workspace]\nname = \"test\"\n")
		fs := fsys.OSFS{}
		writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
			IssuePrefix:    "gc",
			EndpointOrigin: contract.EndpointOriginCityCanonical,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltHost:       "db.example.com",
			DoltPort:       "4411",
		})
		writeDoctorCanonicalMetadata(t, fs, dir, "hq")

		c := NewDoltConfigCheck(dir, false)
		r := c.Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK skip; msg = %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "skipped") {
			t.Fatalf("message = %q, want skipped", r.Message)
		}
	})

	t.Run("external rig", func(t *testing.T) {
		dir := setupCity(t, `[workspace]
name = "test"

[beads]
provider = "file"

[[rigs]]
name = "demo"
path = "demo"
`)
		rigDir := filepath.Join(dir, "demo")
		fs := fsys.OSFS{}
		writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
			IssuePrefix:    "de",
			EndpointOrigin: contract.EndpointOriginExplicit,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltHost:       "rig-db.example.com",
			DoltPort:       "4406",
		})
		writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

		c := NewDoltConfigCheck(dir, false)
		r := c.Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK skip; msg = %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "skipped") {
			t.Fatalf("message = %q, want skipped", r.Message)
		}
	})
}

func TestManagedDoltChecksSkipInvalidCityConfig(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	sizeCheck := NewDoltNomsSizeCheck(dir, false)
	sizeResult := sizeCheck.Run(&CheckContext{})
	if sizeResult.Status != StatusOK || !strings.Contains(sizeResult.Message, "skipped") {
		t.Fatalf("dolt-noms-size status=%d message=%q, want skipped OK", sizeResult.Status, sizeResult.Message)
	}

	configCheck := NewDoltConfigCheck(dir, false)
	configResult := configCheck.Run(&CheckContext{})
	if configResult.Status != StatusOK || !strings.Contains(configResult.Message, "skipped") {
		t.Fatalf("dolt-config status=%d message=%q, want skipped OK", configResult.Status, configResult.Message)
	}

	versionCheck := NewScopedDoltVersionCheck(dir)
	versionCheck.versionOutput = func() (string, error) {
		t.Fatal("versionOutput should not run when city.toml is invalid")
		return "", nil
	}
	versionResult := versionCheck.Run(&CheckContext{})
	if versionResult.Status != StatusOK || !strings.Contains(versionResult.Message, "skipped") {
		t.Fatalf("dolt-version status=%d message=%q, want skipped OK", versionResult.Status, versionResult.Message)
	}
}

// --- DoltVersionCheck ---

func TestParseDoltVersion(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		wantMaj int
		wantMin int
		wantPat int
		wantErr bool
	}{
		{"plain", "dolt version 1.75.2", 1, 75, 2, false},
		{"with_warning", "dolt version 1.75.2\nWarning: some deprecation", 1, 75, 2, false},
		{"no_prefix", "1.50.0", 1, 50, 0, false},
		{"with_v_prefix", "v1.50.0", 1, 50, 0, false},
		{"prerelease", "dolt version 1.76.0-rc1", 1, 76, 0, false},
		{"build_suffix", "dolt version 1.76.0+build.5", 1, 76, 0, false},
		{"empty", "", 0, 0, 0, true},
		{"garbage", "hello world", 0, 0, 0, true},
		{"too_few_parts", "dolt version 1.50", 0, 0, 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseDoltVersion(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseDoltVersion(%q) = %+v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseDoltVersion(%q) error: %v", tc.in, err)
			}
			if got.Major != tc.wantMaj || got.Minor != tc.wantMin || got.Patch != tc.wantPat {
				t.Errorf("parseDoltVersion(%q) = %d.%d.%d, want %d.%d.%d",
					tc.in, got.Major, got.Minor, got.Patch, tc.wantMaj, tc.wantMin, tc.wantPat)
			}
		})
	}
}

func TestCompareDoltVersion(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.50.0", "1.50.0", 0},
		{"1.50.0", "1.75.0", -1},
		{"1.75.0", "1.50.0", 1},
		{"2.0.0", "1.99.99", 1},
		{"1.75.1", "1.75.0", 1},
	}
	for _, tc := range cases {
		av, _ := parseDoltVersion(tc.a)
		bv, _ := parseDoltVersion(tc.b)
		if got := compareDoltVersion(av, bv); got != tc.want {
			t.Errorf("compareDoltVersion(%s, %s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDoltVersionCheck_OK(t *testing.T) {
	c := NewDoltVersionCheck()
	c.versionOutput = func() (string, error) { return "dolt version 1.86.2\n", nil }
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "1.86.2") {
		t.Errorf("message = %q, want version in message", r.Message)
	}
}

func TestDoltVersionCheck_OK_AtMinimum(t *testing.T) {
	c := NewDoltVersionCheck()
	c.versionOutput = func() (string, error) { return "dolt version 1.86.1\n", nil }
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "1.86.1") {
		t.Errorf("message = %q, want version in message", r.Message)
	}
}

func TestDoltVersionCheck_Error_BelowManagedConfigFloor(t *testing.T) {
	c := NewDoltVersionCheck()
	c.versionOutput = func() (string, error) { return "dolt version 1.75.2\n", nil }
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "below minimum") {
		t.Errorf("message = %q, want below-minimum text", r.Message)
	}
}

func TestDoltVersionCheck_Error_BelowMinimum(t *testing.T) {
	c := NewDoltVersionCheck()
	c.versionOutput = func() (string, error) { return "dolt version 1.86.0\n", nil }
	r := c.Run(&CheckContext{})
	if r.Status != StatusError {
		t.Fatalf("status = %d, want Error; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "below minimum") {
		t.Errorf("message = %q, want below-minimum text", r.Message)
	}
}

func TestDoltVersionCheck_NotInstalled(t *testing.T) {
	c := NewDoltVersionCheck()
	c.versionOutput = func() (string, error) { return "", exec.ErrNotFound }
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "not in PATH") {
		t.Errorf("message = %q, want not-in-PATH text", r.Message)
	}
}

func TestDoltVersionCheck_Skipped(t *testing.T) {
	c := NewDoltVersionCheck(true)
	c.versionOutput = func() (string, error) {
		t.Fatal("versionOutput should not run when skipped")
		return "", nil
	}
	r := c.Run(&CheckContext{})
	if r.Status != StatusOK {
		t.Fatalf("status = %d, want OK", r.Status)
	}
	if !strings.Contains(r.Message, "skipped") {
		t.Fatalf("message = %q, want skipped", r.Message)
	}
}

func TestDoltVersionCheck_SkipsExternalTargets(t *testing.T) {
	t.Run("external city", func(t *testing.T) {
		dir := setupCity(t, "[workspace]\nname = \"test\"\n")
		fs := fsys.OSFS{}
		writeDoctorCanonicalConfig(t, fs, dir, contract.ConfigState{
			IssuePrefix:    "gc",
			EndpointOrigin: contract.EndpointOriginCityCanonical,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltHost:       "db.example.com",
			DoltPort:       "4411",
		})
		writeDoctorCanonicalMetadata(t, fs, dir, "hq")

		c := NewScopedDoltVersionCheck(dir)
		c.versionOutput = func() (string, error) {
			t.Fatal("versionOutput should not run for external target")
			return "", nil
		}
		r := c.Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK skip; msg = %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "skipped") {
			t.Fatalf("message = %q, want skipped", r.Message)
		}
	})

	t.Run("external rig", func(t *testing.T) {
		dir := setupCity(t, `[workspace]
name = "test"

[beads]
provider = "file"

[[rigs]]
name = "demo"
path = "demo"
`)
		rigDir := filepath.Join(dir, "demo")
		fs := fsys.OSFS{}
		writeDoctorCanonicalConfig(t, fs, rigDir, contract.ConfigState{
			IssuePrefix:    "de",
			EndpointOrigin: contract.EndpointOriginExplicit,
			EndpointStatus: contract.EndpointStatusVerified,
			DoltHost:       "rig-db.example.com",
			DoltPort:       "4406",
		})
		writeDoctorCanonicalMetadata(t, fs, rigDir, "de")

		c := NewScopedDoltVersionCheck(dir)
		c.versionOutput = func() (string, error) {
			t.Fatal("versionOutput should not run for external target")
			return "", nil
		}
		r := c.Run(&CheckContext{})
		if r.Status != StatusOK {
			t.Fatalf("status = %d, want OK skip; msg = %s", r.Status, r.Message)
		}
		if !strings.Contains(r.Message, "skipped") {
			t.Fatalf("message = %q, want skipped", r.Message)
		}
	})
}

func TestDoltVersionCheck_FreshManagedCityStillChecksLocalBinary(t *testing.T) {
	dir := setupFreshManagedDoltCity(t)
	c := NewScopedDoltVersionCheck(dir)
	called := false
	c.versionOutput = func() (string, error) {
		called = true
		return "", exec.ErrNotFound
	}
	r := c.Run(&CheckContext{})
	if !called {
		t.Fatal("versionOutput was not invoked for fresh managed city")
	}
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "not in PATH") {
		t.Fatalf("message = %q, want not-in-PATH text", r.Message)
	}
}

func TestDoltVersionCheck_Timeout(t *testing.T) {
	oldTimeout := doltVersionCommandTimeout
	doltVersionCommandTimeout = 50 * time.Millisecond
	t.Cleanup(func() { doltVersionCommandTimeout = oldTimeout })

	binDir := t.TempDir()
	doltPath := filepath.Join(binDir, "dolt")
	sleepPath, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("LookPath(sleep): %v", err)
	}
	if err := os.WriteFile(doltPath, []byte("#!/bin/sh\n"+sleepPath+" 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)

	c := NewDoltVersionCheck()
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning; msg = %s", r.Status, r.Message)
	}
	if !strings.Contains(r.Message, "timed out") {
		t.Fatalf("message = %q, want timeout warning", r.Message)
	}
}

func TestDoltVersionCheck_ParseError(t *testing.T) {
	c := NewDoltVersionCheck()
	c.versionOutput = func() (string, error) { return "not-a-version\n", nil }
	r := c.Run(&CheckContext{})
	if r.Status != StatusWarning {
		t.Fatalf("status = %d, want Warning on parse error; msg = %s", r.Status, r.Message)
	}
}

func TestDoltVersionCheck_CanFixFalse(t *testing.T) {
	c := NewDoltVersionCheck()
	if c.CanFix() {
		t.Error("CanFix() = true, want false")
	}
}
