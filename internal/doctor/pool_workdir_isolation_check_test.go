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
	rigRoot := filepath.Join(cityPath, "repos", "demo")
	if err := os.MkdirAll(filepath.Join(rigRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "city"},
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Rigs:      []config.Rig{{Name: "demo", Path: rigRoot}},
	}
	config.InjectImplicitAgents(cfg)
	return cityPath, cfg
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
