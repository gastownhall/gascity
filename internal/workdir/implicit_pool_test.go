package workdir

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestImplicitRigPoolAgentsResolveDistinctWorkDirs(t *testing.T) {
	// End-to-end path resolution for #1181: after InjectImplicitAgents,
	// two pool instance identities must not share the rig root.
	cityPath := t.TempDir()
	rigRoot := filepath.Join(cityPath, "repos", "demo")
	cfg := &config.City{
		Daemon:    config.DaemonConfig{FormulaV2: true},
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Rigs:      []config.Rig{{Name: "demo", Path: rigRoot}},
	}
	config.InjectImplicitAgents(cfg)

	var agent config.Agent
	found := false
	for _, a := range cfg.Agents {
		if a.Implicit && a.Dir == "demo" && a.Name == "claude" {
			agent = a
			found = true
			break
		}
	}
	if !found {
		t.Fatal("missing implicit rig-scoped claude")
	}

	cityName := "city"
	path1 := ResolveWorkDirPath(cityPath, cityName, "demo/claude-1", agent, cfg.Rigs)
	path2 := ResolveWorkDirPath(cityPath, cityName, "demo/claude-2", agent, cfg.Rigs)
	if path1 == path2 {
		t.Fatalf("parallel instances share work dir %q", path1)
	}
	if path1 == rigRoot || path2 == rigRoot {
		t.Fatalf("must not use rig root; got %q and %q", path1, path2)
	}
	want1 := filepath.Join(cityPath, ".gc", "worktrees", "demo", "claude-1")
	want2 := filepath.Join(cityPath, ".gc", "worktrees", "demo", "claude-2")
	if path1 != want1 || path2 != want2 {
		t.Fatalf("got %q, %q; want %q, %q", path1, path2, want1, want2)
	}
}
