package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// resultText flattens a check result into the text a user sees, so
// substring assertions cover the message, the details and the fix hint
// together.
func resultText(r *CheckResult) string {
	return r.Message + "\n" + strings.Join(r.Details, "\n") + "\n" + r.FixHint
}

func mkGitBackedRig(t *testing.T, city string) (string, config.Rig) {
	t.Helper()
	rigRoot := filepath.Join(city, "rigs", "demo")
	if err := os.MkdirAll(filepath.Join(rigRoot, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	return rigRoot, config.Rig{Name: "demo", Path: rigRoot}
}

// TestPoolWorkDirIsolationCheck_WarnsOnSharedGitRigRoot covers the default
// implicit pool shape from #1181: a git-backed rig, a configured provider,
// no isolation setting. Every implicit pool instance resolves to the rig
// root, so the check must warn — naming the agent, the shared path and the
// three remedies — without ever escalating to an error.
func TestPoolWorkDirIsolationCheck_WarnsOnSharedGitRigRoot(t *testing.T) {
	city := t.TempDir()
	rigRoot, rig := mkGitBackedRig(t, city)
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Rigs:      []config.Rig{rig},
	}
	config.InjectImplicitAgents(cfg)

	res := NewPoolWorkDirIsolationCheck(cfg).Run(&CheckContext{CityPath: city})
	if res.Status != StatusWarning {
		t.Fatalf("Status = %v, want StatusWarning", res.Status)
	}
	blob := resultText(res)
	for _, want := range []string{
		"demo/claude",
		rigRoot,
		`implicit_agent_isolation = "worktree"`,
		"max_active_sessions = 1",
		"{{.AgentBase}}",
	} {
		if !strings.Contains(blob, want) {
			t.Errorf("result missing %q:\n%s", want, blob)
		}
	}
	if res.FixHint == "" {
		t.Error("FixHint is empty; the warning must offer the remedies")
	}
}

// TestPoolWorkDirIsolationCheck_NoWarningWhenIsolated covers every shape
// that must stay silent: worktree mode, per-instance work_dir templates,
// pools capped to one session, non-git rig roots, agents with no pool
// signal, and city-scoped agents (isolated by their own fallback path).
func TestPoolWorkDirIsolationCheck_NoWarningWhenIsolated(t *testing.T) {
	t.Run("worktree mode", func(t *testing.T) {
		city := t.TempDir()
		_, rig := mkGitBackedRig(t, city)
		cfg := &config.City{
			Providers: map[string]config.ProviderSpec{"claude": {}},
			Rigs:      []config.Rig{rig},
		}
		cfg.Workspace.ImplicitAgentIsolation = "worktree"
		config.InjectImplicitAgents(cfg)

		res := NewPoolWorkDirIsolationCheck(cfg).Run(&CheckContext{CityPath: city})
		if res.Status != StatusOK {
			t.Fatalf("Status = %v (%s), want StatusOK in worktree mode", res.Status, resultText(res))
		}
	})

	t.Run("explicit pool with per-instance work_dir", func(t *testing.T) {
		city := t.TempDir()
		_, rig := mkGitBackedRig(t, city)
		cfg := &config.City{
			Rigs: []config.Rig{rig},
			Agents: []config.Agent{{
				Name:              "worker",
				Dir:               rig.Name,
				MaxActiveSessions: intPtr(-1),
				WorkDir:           "wt/{{.AgentBase}}",
			}},
		}
		res := NewPoolWorkDirIsolationCheck(cfg).Run(&CheckContext{CityPath: city})
		if res.Status != StatusOK {
			t.Fatalf("Status = %v (%s), want StatusOK", res.Status, resultText(res))
		}
	})
	t.Run("implicit pool capped at one session by rig", func(t *testing.T) {
		city := t.TempDir()
		_, rig := mkGitBackedRig(t, city)
		rig.MaxActiveSessions = intPtr(1)
		cfg := &config.City{
			Providers: map[string]config.ProviderSpec{"claude": {}},
			Rigs:      []config.Rig{rig},
		}
		config.InjectImplicitAgents(cfg)

		res := NewPoolWorkDirIsolationCheck(cfg).Run(&CheckContext{CityPath: city})
		if res.Status != StatusOK {
			t.Fatalf("Status = %v (%s), want StatusOK for a rig-capped pool", res.Status, resultText(res))
		}
	})
	t.Run("implicit pool capped at one session by workspace", func(t *testing.T) {
		city := t.TempDir()
		_, rig := mkGitBackedRig(t, city)
		cfg := &config.City{
			Providers: map[string]config.ProviderSpec{"claude": {}},
			Rigs:      []config.Rig{rig},
		}
		cfg.Workspace.MaxActiveSessions = intPtr(1)
		config.InjectImplicitAgents(cfg)

		res := NewPoolWorkDirIsolationCheck(cfg).Run(&CheckContext{CityPath: city})
		if res.Status != StatusOK {
			t.Fatalf("Status = %v (%s), want StatusOK for a workspace-capped pool", res.Status, resultText(res))
		}
	})
	t.Run("rig root is not a git repo", func(t *testing.T) {
		city := t.TempDir()
		rigRoot := filepath.Join(city, "rigs", "demo")
		if err := os.MkdirAll(rigRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		rig := config.Rig{Name: "demo", Path: rigRoot}
		cfg := &config.City{
			Providers: map[string]config.ProviderSpec{"claude": {}},
			Rigs:      []config.Rig{rig},
		}
		config.InjectImplicitAgents(cfg)

		res := NewPoolWorkDirIsolationCheck(cfg).Run(&CheckContext{CityPath: city})
		if res.Status != StatusOK {
			t.Fatalf("Status = %v (%s), want StatusOK without a .git entry at the rig root", res.Status, resultText(res))
		}
	})
	t.Run("linked-worktree rig root counts as a git repo", func(t *testing.T) {
		city := t.TempDir()
		rigRoot := filepath.Join(city, "rigs", "demo")
		if err := os.MkdirAll(rigRoot, 0o755); err != nil {
			t.Fatal(err)
		}
		// A .git file (linked worktree) instead of a .git directory.
		if err := os.WriteFile(filepath.Join(rigRoot, ".git"), []byte("gitdir: /tmp/elsewhere"), 0o644); err != nil {
			t.Fatal(err)
		}
		rig := config.Rig{Name: "demo", Path: rigRoot}
		cfg := &config.City{
			Providers: map[string]config.ProviderSpec{"claude": {}},
			Rigs:      []config.Rig{rig},
		}
		config.InjectImplicitAgents(cfg)

		res := NewPoolWorkDirIsolationCheck(cfg).Run(&CheckContext{CityPath: city})
		if res.Status != StatusWarning {
			t.Fatalf("Status = %v, want StatusWarning; a .git file marks a linked-worktree rig root", res.Status)
		}
	})
	t.Run("explicit agent with no pool signal", func(t *testing.T) {
		city := t.TempDir()
		_, rig := mkGitBackedRig(t, city)
		cfg := &config.City{
			Rigs:   []config.Rig{rig},
			Agents: []config.Agent{{Name: "worker", Dir: rig.Name}},
		}
		res := NewPoolWorkDirIsolationCheck(cfg).Run(&CheckContext{CityPath: city})
		if res.Status != StatusOK {
			t.Fatalf("Status = %v (%s), want StatusOK for an agent with no pool signal", res.Status, resultText(res))
		}
	})
	t.Run("city-scoped implicit agent", func(t *testing.T) {
		city := t.TempDir()
		_, rig := mkGitBackedRig(t, city)
		cfg := &config.City{
			Providers: map[string]config.ProviderSpec{"claude": {}},
			Rigs:      []config.Rig{rig},
		}
		config.InjectImplicitAgents(cfg)

		var cityScoped int
		for _, a := range cfg.Agents {
			if a.Implicit && a.Dir == "" {
				cityScoped++
			}
		}
		if cityScoped == 0 {
			t.Fatal("no city-scoped implicit agent injected")
		}
		// Only the rig-scoped implicit pool may warn; drop it so the check
		// cannot trip on it and the city-scoped agent is exercised alone.
		var agents []config.Agent
		for _, a := range cfg.Agents {
			if !a.Implicit || a.Dir == "" {
				agents = append(agents, a)
			}
		}
		cfg.Agents = agents
		res := NewPoolWorkDirIsolationCheck(cfg).Run(&CheckContext{CityPath: city})
		if res.Status != StatusOK {
			t.Fatalf("Status = %v (%s), want StatusOK; city-scoped agents have their own isolation fallback", res.Status, resultText(res))
		}
	})
}

// TestPoolWorkDirIsolationCheck_NoConfig pins the nil-config behavior so the
// check never panics when doctor runs without a loadable city.
func TestPoolWorkDirIsolationCheck_NoConfig(t *testing.T) {
	res := NewPoolWorkDirIsolationCheck(nil).Run(nil)
	if res.Status != StatusOK {
		t.Fatalf("Status = %v, want StatusOK with no config loaded", res.Status)
	}
}
