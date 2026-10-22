package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// poolIsolationCity returns a city path and a config with one rig "demo"
// whose root holds a .git directory, and the implicit agents injected for
// the configured "claude" provider.
func poolIsolationCity(t *testing.T) (string, *config.City) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := poolIsolationConfig(t, cityPath)
	config.InjectImplicitAgents(cfg)
	return cityPath, cfg
}

// poolIsolationConfig returns a config with the "claude" provider and one
// rig "demo" under cityPath whose root holds a .git directory. Implicit
// agents are not injected yet, so callers can adjust the config first.
func poolIsolationConfig(t *testing.T, cityPath string) *config.City {
	t.Helper()
	rigRoot := filepath.Join(cityPath, "repos", "demo")
	if err := os.MkdirAll(filepath.Join(rigRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &config.City{
		Workspace: config.Workspace{Name: "city"},
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Rigs:      []config.Rig{{Name: "demo", Path: rigRoot}},
	}
}

func runPoolIsolationCheck(cityPath string, cfg *config.City) *CheckResult {
	return NewPoolWorkDirIsolationCheck(cfg).Run(&CheckContext{CityPath: cityPath})
}

// AC-1: the default implicit rig agent on a git rig is warned about, with
// its name, the shared rig root and the three remedies.
func TestPoolWorkDirIsolationCheckWarnsOnSharedImplicitRigAgent(t *testing.T) {
	cityPath, cfg := poolIsolationCity(t)
	check := NewPoolWorkDirIsolationCheck(cfg)
	if check.Name() != "pool-workdir-isolation" {
		t.Fatalf("Name() = %q, want pool-workdir-isolation", check.Name())
	}
	if check.CanFix() {
		t.Fatal("CanFix() = true, want false")
	}
	r := runPoolIsolationCheck(cityPath, cfg)
	if r.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; result = %+v", r.Status, r)
	}
	text := r.Message + "\n" + strings.Join(r.Details, "\n") + "\n" + r.FixHint
	for _, want := range []string{
		"demo/claude",
		filepath.Join(cityPath, "repos", "demo"),
		`implicit_agent_isolation = "worktree"`,
		"[[patches.agent]]",
		"max_active_sessions = 1",
		"{{.AgentBase}}",
		"pre_start",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("result does not mention %q:\n%s", want, text)
		}
	}
	// The city-scoped implicit agent is already isolated per instance.
	if strings.Contains(strings.Join(r.Details, "\n"), `"claude"`) {
		t.Errorf("city-scoped implicit agent reported:\n%s", strings.Join(r.Details, "\n"))
	}
}

// AC-1: a linked-worktree rig root (.git file) counts as a git rig.
func TestPoolWorkDirIsolationCheckWarnsOnGitFileRigRoot(t *testing.T) {
	cityPath, cfg := poolIsolationCity(t)
	gitPath := filepath.Join(cfg.Rigs[0].Path, ".git")
	if err := os.Remove(gitPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gitPath, []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := runPoolIsolationCheck(cityPath, cfg); r.Status != StatusWarning {
		t.Fatalf("status = %v, want warning; result = %+v", r.Status, r)
	}
}

// AC-2: isolated, capped, non-git and non-pooled agents are not reported.
func TestPoolWorkDirIsolationCheckIgnoresIsolatedAgents(t *testing.T) {
	one := func() *int { n := 1; return &n }
	three := func() *int { n := 3; return &n }
	capImplicitRigAgent := func(cfg *config.City) {
		for i := range cfg.Agents {
			if cfg.Agents[i].Implicit && cfg.Agents[i].Dir == "demo" {
				cfg.Agents[i].MaxActiveSessions = one()
			}
		}
	}
	tests := []struct {
		name  string
		setup func(t *testing.T, cityPath string) *config.City
	}{
		{"worktree isolation", func(t *testing.T, cityPath string) *config.City {
			coreDir := filepath.Join(cityPath, "packs", "core")
			if err := os.MkdirAll(coreDir, 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := poolIsolationConfig(t, cityPath)
			cfg.Workspace.ImplicitAgentIsolation = config.ImplicitAgentIsolationWorktree
			cfg.PackDirs = []string{coreDir}
			if err := os.WriteFile(filepath.Join(coreDir, "pack.toml"), []byte("[pack]\nname = \"core\"\nschema = 1\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			config.InjectImplicitAgents(cfg)
			return cfg
		}},
		{"explicit pool with per-instance work_dir", func(t *testing.T, cityPath string) *config.City {
			cfg := poolIsolationConfig(t, cityPath)
			cfg.Providers = nil
			cfg.Agents = []config.Agent{{
				Name: "builder", Dir: "demo", MaxActiveSessions: three(),
				WorkDir: ".gc/worktrees/{{.Rig}}/{{.AgentBase}}",
			}}
			return cfg
		}},
		{"implicit agent patched to one session", func(t *testing.T, cityPath string) *config.City {
			cfg := poolIsolationConfig(t, cityPath)
			config.InjectImplicitAgents(cfg)
			capImplicitRigAgent(cfg)
			return cfg
		}},
		{"rig capped to one session", func(t *testing.T, cityPath string) *config.City {
			cfg := poolIsolationConfig(t, cityPath)
			cfg.Rigs[0].MaxActiveSessions = one()
			config.InjectImplicitAgents(cfg)
			return cfg
		}},
		{"workspace capped to one session", func(t *testing.T, cityPath string) *config.City {
			cfg := poolIsolationConfig(t, cityPath)
			cfg.Workspace.MaxActiveSessions = one()
			config.InjectImplicitAgents(cfg)
			return cfg
		}},
		{"rig root without .git", func(t *testing.T, cityPath string) *config.City {
			cfg := poolIsolationConfig(t, cityPath)
			if err := os.RemoveAll(filepath.Join(cfg.Rigs[0].Path, ".git")); err != nil {
				t.Fatal(err)
			}
			config.InjectImplicitAgents(cfg)
			return cfg
		}},
		{"explicit agent without pool signal", func(t *testing.T, cityPath string) *config.City {
			cfg := poolIsolationConfig(t, cityPath)
			cfg.Providers = nil
			cfg.Agents = []config.Agent{{Name: "builder", Dir: "demo"}}
			return cfg
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			cfg := tt.setup(t, cityPath)
			r := runPoolIsolationCheck(cityPath, cfg)
			if r.Status != StatusOK || len(r.Details) != 0 {
				t.Fatalf("status = %v details = %q, want OK with no details; message = %q", r.Status, r.Details, r.Message)
			}
		})
	}
}
