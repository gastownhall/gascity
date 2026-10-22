package workdir

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// implicitRigCity returns a city whose only rig, app, is a git checkout (a
// .git entry is all the resolver looks at) and the rig-scoped implicit agent
// InjectImplicitAgents mints for provider claude.
func implicitRigCity(t *testing.T) (string, string, config.Agent, []config.Rig) {
	t.Helper()
	city := t.TempDir()
	rig := filepath.Join(city, "app")
	if err := os.MkdirAll(filepath.Join(rig, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Providers: map[string]config.ProviderSpec{"claude": {}},
		Rigs:      []config.Rig{{Name: "app", Path: rig}},
	}
	config.InjectImplicitAgents(cfg)
	for _, a := range cfg.Agents {
		if a.Dir == "app" && a.Name == "claude" {
			return city, rig, a, cfg.Rigs
		}
	}
	t.Fatalf("InjectImplicitAgents minted no rig-scoped claude agent: %+v", cfg.Agents)
	return "", "", config.Agent{}, nil
}

func TestImplicitRigPoolInstancesGetDistinctWorkDirs(t *testing.T) {
	city, rig, a, rigs := implicitRigCity(t)

	for _, n := range []string{"1", "2"} {
		got, err := ResolveWorkDirPathStrict(city, "city", "app/claude-"+n, a, rigs)
		if err != nil {
			t.Fatalf("ResolveWorkDirPathStrict(app/claude-%s): %v", n, err)
		}
		want := filepath.Join(city, ".gc", "worktrees", "app", "claude-"+n)
		if got != want {
			t.Errorf("app/claude-%s work dir = %q, want its own slot %q", n, got, want)
		}
	}
	got, err := ResolveWorkDirPathStrict(city, "city", "app/claude", a, rigs)
	if err != nil {
		t.Fatalf("ResolveWorkDirPathStrict(app/claude): %v", err)
	}
	if got != rig {
		t.Errorf("canonical app/claude work dir = %q, want the rig root %q", got, rig)
	}
}

func TestImplicitSlotForNamesSlotBranch(t *testing.T) {
	city, rig, a, rigs := implicitRigCity(t)

	slot, ok := ImplicitSlotFor(city, "app/claude-3", a, rigs)
	if !ok {
		t.Fatal("ImplicitSlotFor(app/claude-3) did not match a marked rig-scoped implicit agent")
	}
	if slot.RigRoot != rig {
		t.Errorf("RigRoot = %q, want %q", slot.RigRoot, rig)
	}
	if want := filepath.Join(city, ".gc", "worktrees", "app", "claude-3"); slot.Path != want {
		t.Errorf("Path = %q, want %q", slot.Path, want)
	}
	other, _ := ImplicitSlotFor(city, "app/claude-4", a, rigs)
	if slot.Branch == other.Branch {
		t.Errorf("slots 3 and 4 share branch %q", slot.Branch)
	}
	if len(slot.Branch) != len("gc-claude-3-")+12 || slot.Branch[:len("gc-claude-3-")] != "gc-claude-3-" {
		t.Errorf("Branch = %q, want gc-claude-3-<12 hex>", slot.Branch)
	}
	again, _ := ImplicitSlotFor(city, "app/claude-3", a, rigs)
	if again.Branch != slot.Branch {
		t.Errorf("Branch is not stable: %q then %q", slot.Branch, again.Branch)
	}
}
