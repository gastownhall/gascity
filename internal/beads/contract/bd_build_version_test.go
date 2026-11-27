package contract

import (
	"runtime/debug"
	"testing"
)

// TestReadBeadsBuild covers the build-info reader behind gc doctor's
// bd-beads-version check. A bd carries beads as its main module, gc as a
// dependency, and the version counts only when it names a beads release.
func TestReadBeadsBuild(t *testing.T) {
	bd := func(version string) *debug.BuildInfo {
		return &debug.BuildInfo{
			Path: beadsModulePath + "/cmd/bd",
			Main: debug.Module{Path: beadsModulePath, Version: version},
			Deps: []*debug.Module{{Path: "github.com/spf13/cobra", Version: "v1.9.1"}},
		}
	}
	gc := func(beads *debug.Module) *debug.BuildInfo {
		return &debug.BuildInfo{
			Main: debug.Module{Path: "github.com/gastownhall/gascity", Version: "(devel)"},
			Deps: []*debug.Module{{Path: "github.com/spf13/cobra", Version: "v1.9.1"}, beads},
		}
	}
	for _, tc := range []struct {
		name string
		info *debug.BuildInfo
		want BeadsBuild
	}{
		{"bd at a release (go install bd@tag)", bd("v1.3.4"), BeadsBuild{Version: "v1.3.4"}},
		{"bd at a prerelease", bd("v1.4.0-rc.2"), BeadsBuild{Version: "v1.4.0-rc.2"}},
		{"bd at a pseudo-version", bd("v1.3.5-0.20261101120000-abcdef123456"), BeadsBuild{Version: "v1.3.5-0.20261101120000-abcdef123456", Unconfirmed: "pseudo-version"}},
		{"bd built from a modified release checkout", bd("v1.3.4+dirty"), BeadsBuild{Version: "v1.3.4+dirty", Unconfirmed: "modified source build"}},
		{"bd built from a modified untagged checkout", bd("v0.0.0-20261107052026-05ebdd78fd3c+dirty"), BeadsBuild{Version: "v0.0.0-20261107052026-05ebdd78fd3c+dirty", Unconfirmed: "modified source build"}},
		{"bd source build without VCS stamping", bd("(devel)"), BeadsBuild{Version: "(devel)", Unconfirmed: "source build"}},
		{"bd with an empty version", bd(""), BeadsBuild{Unconfirmed: "source build"}},
		{"gc linking a release", gc(&debug.Module{Path: beadsModulePath, Version: "v1.3.4"}), BeadsBuild{Version: "v1.3.4"}},
		{"gc linking a versioned replacement", gc(&debug.Module{Path: beadsModulePath, Version: "v1.3.4", Replace: &debug.Module{Path: "github.com/fork/beads", Version: "v1.3.9"}}), BeadsBuild{Version: "v1.3.9", Unconfirmed: "replaced module"}},
		{"gc linking a local checkout", gc(&debug.Module{Path: beadsModulePath, Version: "v1.3.4", Replace: &debug.Module{Path: "../beads"}}), BeadsBuild{Unconfirmed: "source build"}},
		{"no beads module", &debug.BuildInfo{Main: debug.Module{Path: "example.com/bd", Version: "v1.0.0"}, Deps: []*debug.Module{{Path: beadsModulePath + "-extra", Version: "v9.9.9"}}}, BeadsBuild{Unconfirmed: "no beads module"}},
		{"nil build info", nil, BeadsBuild{Unconfirmed: "no beads module"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ReadBeadsBuild(tc.info); got != tc.want {
				t.Fatalf("ReadBeadsBuild() = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestRelateBDVersion pins the version policy gc doctor and the native-store
// preflight share: the linked version, a newer release of its major version
// (gastownhall/gascity#5164) and a prerelease of the same release are
// compatible; an older bd and another major version are not.
func TestRelateBDVersion(t *testing.T) {
	for _, tc := range []struct {
		bd, library string
		want        BDVersionRelation
	}{
		{"v1.3.4", "v1.3.4", BDVersionMatches},
		{"1.3.4", "v1.3.4", BDVersionMatches},
		{"v1.3.5", "v1.3.4", BDVersionNewer},
		{"v1.4.0-rc.2", "v1.3.4", BDVersionNewer},
		{"v1.4.0-rc.2", "v1.4.0-rc.1", BDVersionNewer},
		{"v1.4.0-rc.1", "v1.4.0-rc.2", BDVersionSamePrerelease},
		{"v1.3.3", "v1.3.4", BDVersionOlder},
		{"v1.3.4-rc.1", "v1.3.4", BDVersionOlder},
		{"v0.9.0", "v1.3.4", BDVersionOlder},
		{"v2.0.0", "v1.3.4", BDVersionNewerMajor},
		{"dev", "v1.3.4", BDVersionUnparsed},
	} {
		t.Run(tc.bd+"_vs_"+tc.library, func(t *testing.T) {
			if got := RelateBDVersion(tc.bd, tc.library); got != tc.want {
				t.Fatalf("RelateBDVersion(%q, %q) = %v, want %v", tc.bd, tc.library, got, tc.want)
			}
		})
	}
}

// TestBDVersionRelationCompatible: only the relations the policy accepts are
// compatible.
func TestBDVersionRelationCompatible(t *testing.T) {
	for relation, want := range map[BDVersionRelation]bool{
		BDVersionMatches:        true,
		BDVersionNewer:          true,
		BDVersionSamePrerelease: true,
		BDVersionOlder:          false,
		BDVersionNewerMajor:     false,
		BDVersionUnparsed:       false,
	} {
		if got := relation.Compatible(); got != want {
			t.Errorf("%v.Compatible() = %v, want %v", relation, got, want)
		}
	}
}
