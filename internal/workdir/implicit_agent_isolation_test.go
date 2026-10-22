package workdir

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// implicitRigAgent injects implicit agents into cfg and returns the
// rig-scoped "claude" one for rig "demo".
func implicitRigAgent(t *testing.T, cfg *config.City) config.Agent {
	t.Helper()
	config.InjectImplicitAgents(cfg)
	for _, a := range cfg.Agents {
		if a.Implicit && a.Dir == "demo" && a.Name == "claude" {
			return a
		}
	}
	t.Fatalf("no implicit rig-scoped claude agent injected; agents = %+v", cfg.Agents)
	return config.Agent{}
}

func resolveInstances(t *testing.T, cityPath string, a config.Agent, rigs []config.Rig) (string, string) {
	t.Helper()
	first, err := ResolveWorkDirPathStrict(cityPath, "city", a.QualifiedInstanceName("claude-1"), a, rigs)
	if err != nil {
		t.Fatalf("resolve claude-1: %v", err)
	}
	second, err := ResolveWorkDirPathStrict(cityPath, "city", a.QualifiedInstanceName("claude-2"), a, rigs)
	if err != nil {
		t.Fatalf("resolve claude-2: %v", err)
	}
	return first, second
}

// AC-3: in the default "shared" mode every implicit rig instance keeps
// running in the rig root.
func TestImplicitRigAgentSharedIsolationResolvesToRigRoot(t *testing.T) {
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "repos", "demo")
	rigs := []config.Rig{{Name: "demo", Path: rigRoot}}
	for _, isolation := range []string{"", config.ImplicitAgentIsolationShared} {
		cfg := &config.City{
			Workspace: config.Workspace{ImplicitAgentIsolation: isolation},
			Providers: map[string]config.ProviderSpec{"claude": {}},
			Rigs:      rigs,
		}
		a := implicitRigAgent(t, cfg)
		first, second := resolveInstances(t, cityPath, a, rigs)
		if first != rigRoot || second != rigRoot {
			t.Fatalf("isolation %q: instances resolve to %q and %q, want rig root %q", isolation, first, second, rigRoot)
		}
	}
}
