package config

import (
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

const bogusImplicitIsolationError = `workspace.implicit_isolation: invalid mode "bogus": want one of worktree, none`

func TestImplicitIsolationRejectsUnknownMode(t *testing.T) {
	_, err := Parse([]byte("[workspace]\nimplicit_isolation = \"bogus\"\n"))
	if err == nil || err.Error() != bogusImplicitIsolationError {
		t.Fatalf("Parse error = %v, want %q", err, bogusImplicitIsolationError)
	}

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
	_, _, err = LoadWithIncludes(fs, "/city/city.toml")
	if err == nil || err.Error() != bogusImplicitIsolationError {
		t.Fatalf("LoadWithIncludes error = %v, want %q", err, bogusImplicitIsolationError)
	}
}

func TestImplicitIsolationGatesPoolSlotMarker(t *testing.T) {
	for _, tc := range []struct {
		name   string
		line   string
		marked bool
	}{
		{"unset", "", true},
		{"worktree", `implicit_isolation = "worktree"`, true},
		{"none", `implicit_isolation = "none"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fs := fsys.NewFake()
			fs.Files["/city/city.toml"] = []byte(`
[workspace]
name = "test"
` + tc.line + `

[providers.claude]

[[rigs]]
name = "app"
path = "/repo/app"

[[agent]]
name = "explicit"
dir = "app"
`)
			cfg, _, err := LoadWithIncludes(fs, "/city/city.toml")
			if err != nil {
				t.Fatalf("LoadWithIncludes: %v", err)
			}
			var sawRig, sawCity bool
			for _, a := range cfg.Agents {
				switch {
				case a.Implicit && a.Dir == "app":
					sawRig = true
					if a.PoolSlotWorktrees != tc.marked {
						t.Errorf("rig-scoped implicit %s PoolSlotWorktrees = %v, want %v", a.QualifiedName(), a.PoolSlotWorktrees, tc.marked)
					}
				case a.Implicit:
					sawCity = true
					if a.PoolSlotWorktrees {
						t.Errorf("city-scoped implicit %s is marked for pool-slot worktrees", a.QualifiedName())
					}
				case a.PoolSlotWorktrees:
					t.Errorf("explicit agent %s is marked for pool-slot worktrees", a.QualifiedName())
				}
			}
			if !sawRig || !sawCity {
				t.Fatalf("implicit agents rig=%v city=%v, want both injected: %+v", sawRig, sawCity, cfg.Agents)
			}
		})
	}
}
