package bd_test

import (
	"os"
	"testing"

	"golang.org/x/mod/modfile"
	"golang.org/x/mod/semver"
)

const beadsModule = "github.com/steveyegge/beads"

func parseGoMod(t *testing.T, path string) *modfile.File {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	f, err := modfile.Parse(path, data, nil)
	if err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
	return f
}

func requiredVersions(f *modfile.File) map[string]string {
	versions := map[string]string{}
	for _, r := range f.Require {
		versions[r.Mod.Path] = r.Mod.Version
	}
	return versions
}

// TestModuleRequirementsAgreeWithRoot keeps the bd module and the root module
// gazelle.go.work joins at identical versions for every module both require.
// go_deps resolves them as one graph (highest version wins), so a module this
// one requires at a higher version than the root go.mod would silently change
// what the gc build links under Bazel but not under `go build`; a lower one
// would mean bd is not built from the versions its go.sum was tidied against.
func TestModuleRequirementsAgreeWithRoot(t *testing.T) {
	root := parseGoMod(t, "../../go.mod")
	bd := parseGoMod(t, "go.mod")
	rootVersions := requiredVersions(root)
	for _, r := range bd.Require {
		want, ok := rootVersions[r.Mod.Path]
		if !ok {
			continue
		}
		if semver.Compare(r.Mod.Version, want) != 0 {
			t.Errorf("tools/bd/go.mod requires %s %s; the root go.mod requires %s (align them)", r.Mod.Path, r.Mod.Version, want)
		}
	}
	if root.Go.Version != bd.Go.Version {
		t.Errorf("tools/bd/go.mod go %s; root go.mod go %s", bd.Go.Version, root.Go.Version)
	}
}

// TestBuildsBdFromTheBeadsGCLinks pins the bd this module builds to the beads
// gc links: a bd from any other beads commit could disagree with gc about the
// schema or the wire, which is the skew the source-built bd exists to remove.
func TestBuildsBdFromTheBeadsGCLinks(t *testing.T) {
	root := parseGoMod(t, "../../go.mod")
	bd := parseGoMod(t, "go.mod")
	want, ok := requiredVersions(root)[beadsModule]
	if !ok {
		t.Fatalf("the root go.mod does not require %s", beadsModule)
	}
	if got := requiredVersions(bd)[beadsModule]; got != want {
		t.Errorf("tools/bd/go.mod requires %s %q; the root go.mod requires %q", beadsModule, got, want)
	}
	var tool bool
	for _, tl := range bd.Tool {
		if tl.Path == beadsModule+"/cmd/bd" {
			tool = true
		}
	}
	if !tool {
		t.Errorf("tools/bd/go.mod has no `tool %s/cmd/bd` directive; without it go mod tidy drops the modules //tools/bd:bd needs", beadsModule)
	}
}
