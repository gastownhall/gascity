package config

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestImplicitIsolationModeValidation pins the load-time enum: a typo in
// workspace.implicit_isolation must fail the load naming the field and the
// allowed set, not silently pick a mode, and the valid spellings plus unset
// must load cleanly.
func TestImplicitIsolationModeValidation(t *testing.T) {
	_, err := Parse([]byte("[workspace]\nname = \"city\"\nimplicit_isolation = \"bogus\"\n"))
	if err == nil {
		t.Fatal("Parse accepted workspace.implicit_isolation = \"bogus\"; want a load error")
	}
	want := `workspace.implicit_isolation: invalid mode "bogus": want one of worktree, none`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("Parse error = %q, want it to contain %q", err.Error(), want)
	}

	for _, mode := range []string{"worktree", "none", ""} {
		doc := "[workspace]\nname = \"city\"\n"
		if mode != "" {
			doc += "implicit_isolation = \"" + mode + "\"\n"
		}
		cfg, err := Parse([]byte(doc))
		if err != nil {
			t.Fatalf("Parse with implicit_isolation = %q: %v", mode, err)
		}
		if got := cfg.Workspace.ImplicitIsolation; got != mode {
			t.Errorf("implicit_isolation = %q decoded as %q, want it verbatim", mode, got)
		}
	}
}

// TestImplicitIsolationComposedRootValidation mirrors the beads-mode rule:
// a fragment can supply workspace.implicit_isolation, so the composed root
// must re-validate what any single layer parsed. A fragment carrying a bogus
// value must fail LoadWithIncludes even when the root is clean.
func TestImplicitIsolationComposedRootValidation(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[workspace]
implicit_isolation = "bogus"
`)
	_, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err == nil {
		t.Fatal("LoadWithIncludes accepted a fragment's bogus implicit_isolation; want a load error")
	}
	want := `workspace.implicit_isolation: invalid mode "bogus": want one of worktree, none`
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("LoadWithIncludes error = %q, want it to contain %q", err.Error(), want)
	}
}

// TestImplicitIsolationFragmentOverrideWins covers the mergeWorkspace wiring:
// a fragment that defines implicit_isolation overrides the root's value.
func TestImplicitIsolationFragmentOverrideWins(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte(`
include = ["fragment.toml"]

[workspace]
name = "test"
implicit_isolation = "worktree"
`)
	fs.Files["/city/fragment.toml"] = []byte(`
[workspace]
implicit_isolation = "none"
`)
	cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("LoadWithIncludes: %v", err)
	}
	if cfg.Workspace.ImplicitIsolation != "none" {
		t.Fatalf("composed implicit_isolation = %q, want the fragment's none to win", cfg.Workspace.ImplicitIsolation)
	}
}

// TestEffectiveImplicitIsolationDefault pins the default-on accessor: unset
// means "worktree" so implicit pool isolation is the out-of-the-box behavior,
// and only an explicit "none" opts out.
func TestEffectiveImplicitIsolationDefault(t *testing.T) {
	if got := (Workspace{}).EffectiveImplicitIsolation(); got != ImplicitIsolationWorktree {
		t.Fatalf("zero-value EffectiveImplicitIsolation = %q, want %q", got, ImplicitIsolationWorktree)
	}
	if got := (Workspace{ImplicitIsolation: "none"}).EffectiveImplicitIsolation(); got != ImplicitIsolationNone {
		t.Fatalf("EffectiveImplicitIsolation(none) = %q, want %q", got, ImplicitIsolationNone)
	}
	if got := (Workspace{ImplicitIsolation: "worktree"}).EffectiveImplicitIsolation(); got != ImplicitIsolationWorktree {
		t.Fatalf("EffectiveImplicitIsolation(worktree) = %q, want %q", got, ImplicitIsolationWorktree)
	}
}

// TestImplicitIsolationGatesInjectImplicitAgents pins the marker gate:
// default and "worktree" mark rig-scoped implicit agents for slot worktrees;
// "none" restores the pre-isolation behavior for every implicit agent. Only
// the effective mode decides — a bogus value reaching injection must fail
// closed to unmarked, not half-activate.
func TestImplicitIsolationGatesInjectImplicitAgents(t *testing.T) {
	build := func(mode string) *City {
		cfg := &City{
			Workspace: Workspace{Name: "city"},
			Providers: map[string]ProviderSpec{"claude": BuiltinProviderAlias("claude")},
			Rigs:      []Rig{{Name: "app", Path: "/rigs/app"}},
		}
		cfg.Workspace.ImplicitIsolation = mode
		InjectImplicitAgents(cfg)
		return cfg
	}

	for _, mode := range []string{"", "worktree"} {
		cfg := build(mode)
		for _, a := range cfg.Agents {
			if a.Dir == "app" && !a.PoolSlotWorktrees {
				t.Errorf("implicit_isolation %q: rig-scoped implicit agent not marked for slot worktrees", mode)
			}
			if a.Dir == "" && a.PoolSlotWorktrees {
				t.Errorf("implicit_isolation %q: city-scoped implicit agent marked for slot worktrees", mode)
			}
		}
	}

	for _, mode := range []string{"none", "bogus"} {
		cfg := build(mode)
		for _, a := range cfg.Agents {
			if a.PoolSlotWorktrees {
				t.Errorf("implicit_isolation %q: agent %q/%q marked for slot worktrees; want the mode to disable marking", mode, a.Dir, a.Name)
			}
		}
	}
}
