package packlint

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/workdir"
)

// poolIsolationScanDirs are the repo-root-relative trees whose shipped packs
// must give every concurrently active pool instance its own work_dir.
var poolIsolationScanDirs = []string{
	"examples",
	"contrib",
	"internal/bootstrap/packs",
}

// poolIsolationMustCover names shipped pool agents the walk has to reach, so a
// layout change that hides a pack from the scan fails here instead of leaving
// the table silently vacuous.
var poolIsolationMustCover = []string{
	"contrib/k8s:coder",
	"examples/hyperscale/packs/hyperscale:worker",
	"examples/lifecycle/packs/lifecycle:polecat",
	"examples/swarm/packs/swarm:coder",
}

// TestShippedPacksIsolatePoolWorkDirs walks every shipped pack under examples/,
// contrib/ and internal/bootstrap/packs/ and runs its agents through
// workdir.ValidatePoolWorkDirIsolation, the same check the live gates run. A
// pool agent whose work_dir does not vary per instance lets two concurrent
// sessions share one checkout.
func TestShippedPacksIsolatePoolWorkDirs(t *testing.T) {
	root := repoRoot()
	cityPath := t.TempDir()
	rigs := []config.Rig{{Name: "demo", Path: filepath.Join(cityPath, "repos", "demo")}}

	covered := map[string]bool{}
	for _, dir := range poolIsolationScanDirs {
		packDirs := findPackDirs(t, filepath.Join(root, dir))
		for _, packDir := range packDirs {
			agents, err := config.DiscoverPackAgents(fsys.OSFS{}, packDir, "", nil)
			if err != nil {
				t.Fatalf("DiscoverPackAgents(%s): %v", packDir, err)
			}
			rel, err := filepath.Rel(root, packDir)
			if err != nil {
				t.Fatalf("Rel(%s): %v", packDir, err)
			}
			rel = filepath.ToSlash(rel)
			for _, a := range agents {
				if a.Scope == "rig" {
					// Pack expansion binds a rig-scoped agent to its rig.
					a.Dir = "demo"
				}
				covered[rel+":"+a.Name] = true
				t.Run(rel+"/"+a.Name, func(t *testing.T) {
					if err := workdir.ValidatePoolWorkDirIsolation(cityPath, "demo-city", []config.Agent{a}, rigs); err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}

	for _, want := range poolIsolationMustCover {
		if !covered[want] {
			t.Errorf("scan never reached shipped pool agent %s; the walk is not covering it", want)
		}
	}
}

// findPackDirs returns every directory under dir that holds a pack.toml.
func findPackDirs(t *testing.T, dir string) []string {
	t.Helper()
	var packs []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Name() != "pack.toml" {
			return nil
		}
		packs = append(packs, filepath.Dir(path))
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			t.Fatalf("scan dir %s missing: add it to the test's data deps", dir)
		}
		t.Fatalf("walking %s: %v", dir, err)
	}
	sort.Strings(packs)
	return packs
}
