package workdir

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// findImplicitRigAgent returns the injected rig-scoped implicit agent for
// the named provider, or nil when injection did not create one.
func findImplicitRigAgent(cfg *config.City, rigName, provider string) *config.Agent {
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if a.Implicit && a.Dir == rigName && a.Name == provider {
			return a
		}
	}
	return nil
}

// TestImplicitRigAgentInstancesResolveToRigRootInSharedMode pins #1181's
// compatibility floor at the resolution layer: with isolation unset (or
// "shared"), every numbered instance of a rig-scoped implicit pool resolves
// to the rig root — byte-identical to the pre-key behavior.
func TestImplicitRigAgentInstancesResolveToRigRootInSharedMode(t *testing.T) {
	cityPath := t.TempDir()
	rigs := []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Rigs:      rigs,
	}
	config.InjectImplicitAgents(cfg)

	implicit := findImplicitRigAgent(cfg, "demo", "claude")
	if implicit == nil {
		t.Fatalf("no implicit rig-scoped claude agent injected; agents = %+v", cfg.Agents)
	}
	first, err := ResolveWorkDirPathStrict(cityPath, "city", "demo/claude-1", *implicit, rigs)
	if err != nil {
		t.Fatalf("resolve demo/claude-1: %v", err)
	}
	second, err := ResolveWorkDirPathStrict(cityPath, "city", "demo/claude-2", *implicit, rigs)
	if err != nil {
		t.Fatalf("resolve demo/claude-2: %v", err)
	}
	want := rigs[0].Path
	if first != want || second != want {
		t.Fatalf("shared mode must keep the rig root %q; got -1=%q -2=%q", want, first, second)
	}
}
