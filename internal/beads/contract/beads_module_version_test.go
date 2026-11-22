package contract

import (
	"runtime/debug"
	"testing"
)

// TestBeadsModuleVersion pins the reader behind the bd-beads-version doctor
// check: which build-info shapes name the beads module a Go binary was built
// from, and which cannot be compared at all. A bd built by `go install …@pin`
// carries beads as its main module; gc carries it as a dependency; anything
// replaced, source-built or absent answers unknown rather than a version that
// would fake certainty.
func TestBeadsModuleVersion(t *testing.T) {
	cases := []struct {
		name    string
		info    *debug.BuildInfo
		version string
		ok      bool
	}{
		{
			name: "main module at a release tag",
			info: &debug.BuildInfo{
				Path: "github.com/steveyegge/beads/cmd/bd",
				Main: debug.Module{Path: "github.com/steveyegge/beads", Version: "v1.3.4"},
			},
			version: "v1.3.4", ok: true,
		},
		{
			name: "main module at a pseudo-version",
			info: &debug.BuildInfo{
				Path: "github.com/steveyegge/beads/cmd/bd",
				Main: debug.Module{Path: "github.com/steveyegge/beads", Version: "v1.3.3-0.20261101000000-abcdef123456"},
			},
			version: "v1.3.3-0.20261101000000-abcdef123456", ok: true,
		},
		{
			name: "beads as a dependency (the gc shape)",
			info: &debug.BuildInfo{
				Path: "github.com/gastownhall/gascity/cmd/gc",
				Main: debug.Module{Path: "github.com/gastownhall/gascity", Version: "v0.9.0"},
				Deps: []*debug.Module{
					{Path: "github.com/spf13/cobra", Version: "v1.10.0"},
					{Path: "github.com/steveyegge/beads", Version: "v1.3.4"},
				},
			},
			version: "v1.3.4", ok: true,
		},
		{
			name: "dependency replaced elsewhere",
			info: &debug.BuildInfo{
				Path: "github.com/gastownhall/gascity/cmd/gc",
				Main: debug.Module{Path: "github.com/gastownhall/gascity", Version: "v0.9.0"},
				Deps: []*debug.Module{
					{Path: "github.com/steveyegge/beads", Version: "v1.3.4", Replace: &debug.Module{Path: "../beads", Version: "v0.0.0"}},
				},
			},
		},
		{
			name: "main module replaced",
			info: &debug.BuildInfo{
				Path: "github.com/steveyegge/beads/cmd/bd",
				Main: debug.Module{Path: "github.com/steveyegge/beads", Version: "v0.0.0", Replace: &debug.Module{Path: "../beads", Version: "v0.0.0-20260101000000-abcdefabcdef"}},
			},
		},
		{
			name: "source build reporting (devel)",
			info: &debug.BuildInfo{
				Path: "github.com/steveyegge/beads/cmd/bd",
				Main: debug.Module{Path: "github.com/steveyegge/beads", Version: "(devel)"},
			},
		},
		{
			name: "dependency with an empty version",
			info: &debug.BuildInfo{
				Path: "github.com/gastownhall/gascity/cmd/gc",
				Main: debug.Module{Path: "github.com/gastownhall/gascity", Version: "v0.9.0"},
				Deps: []*debug.Module{{Path: "github.com/steveyegge/beads"}},
			},
		},
		{
			name: "no beads module at all",
			info: &debug.BuildInfo{
				Path: "example.com/other",
				Main: debug.Module{Path: "example.com/other", Version: "v1.0.0"},
			},
		},
		{
			name: "nil build info",
			info: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			version, ok := BeadsModuleVersion(tc.info)
			if ok != tc.ok || version != tc.version {
				t.Fatalf("BeadsModuleVersion(%+v) = (%q, %v), want (%q, %v)", tc.info, version, ok, tc.version, tc.ok)
			}
		})
	}
}
