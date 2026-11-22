package contract

import (
	"runtime/debug"
	"testing"
)

func TestBeadsModuleVersion(t *testing.T) {
	beadsDep := func(version string, replace *debug.Module) *debug.Module {
		return &debug.Module{Path: beadsModulePath, Version: version, Replace: replace}
	}
	cobra := &debug.Module{Path: "github.com/spf13/cobra", Version: "v1.9.1"}
	for _, tc := range []struct {
		name   string
		info   *debug.BuildInfo
		want   string
		wantOK bool
	}{
		{
			name:   "main module at a tag (go install bd@pin)",
			info:   &debug.BuildInfo{Path: beadsModulePath + "/cmd/bd", Main: debug.Module{Path: beadsModulePath, Version: "v1.3.4"}, Deps: []*debug.Module{cobra}},
			want:   "v1.3.4",
			wantOK: true,
		},
		{
			name:   "main module at a pseudo-version",
			info:   &debug.BuildInfo{Main: debug.Module{Path: beadsModulePath, Version: "v1.3.5-0.20261101120000-abcdef123456"}},
			want:   "v1.3.5-0.20261101120000-abcdef123456",
			wantOK: true,
		},
		{
			name:   "beads as a dependency (gc)",
			info:   &debug.BuildInfo{Main: debug.Module{Path: "github.com/gastownhall/gascity", Version: "(devel)"}, Deps: []*debug.Module{cobra, beadsDep("v1.3.4", nil)}},
			want:   "v1.3.4",
			wantOK: true,
		},
		{
			name: "replaced dependency",
			info: &debug.BuildInfo{Main: debug.Module{Path: "github.com/gastownhall/gascity"}, Deps: []*debug.Module{beadsDep("v1.3.4", &debug.Module{Path: "../beads"})}},
		},
		{
			name: "replaced dependency with a versioned replacement",
			info: &debug.BuildInfo{Main: debug.Module{Path: "github.com/gastownhall/gascity"}, Deps: []*debug.Module{beadsDep("v1.3.4", &debug.Module{Path: "github.com/fork/beads", Version: "v1.3.9"})}},
		},
		{
			name: "replaced main module",
			info: &debug.BuildInfo{Main: debug.Module{Path: beadsModulePath, Version: "v1.3.4", Replace: &debug.Module{Path: "../beads"}}},
		},
		{
			name: "main module (devel) source build",
			info: &debug.BuildInfo{Main: debug.Module{Path: beadsModulePath, Version: "(devel)"}},
		},
		{
			name: "main module with an empty version",
			info: &debug.BuildInfo{Main: debug.Module{Path: beadsModulePath}},
		},
		{
			name: "dependency at (devel)",
			info: &debug.BuildInfo{Deps: []*debug.Module{beadsDep("(devel)", nil)}},
		},
		{
			name: "nil info",
		},
		{
			name: "no beads module",
			info: &debug.BuildInfo{Main: debug.Module{Path: "example.com/other", Version: "v1.0.0"}, Deps: []*debug.Module{cobra, {Path: beadsModulePath + "-extra", Version: "v9.9.9"}}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := BeadsModuleVersion(tc.info)
			if got != tc.want || ok != tc.wantOK {
				t.Fatalf("BeadsModuleVersion = (%q, %v), want (%q, %v)", got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
