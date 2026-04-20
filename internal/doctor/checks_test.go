package doctor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
