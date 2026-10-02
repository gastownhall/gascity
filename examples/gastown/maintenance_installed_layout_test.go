package gastown_test

import (
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/examples/bd/dolt"
	"github.com/gastownhall/gascity/internal/bootstrap/packs/core"
	"github.com/gastownhall/gascity/internal/builtinpacks"
)

// installedPackLayout materializes the bundled core and dolt packs the way a
// city receives them and returns the directories the maintenance orders run
// from.
type installedPackLayout struct {
	name     string
	prepare  func(t *testing.T, cityDir string) (coreDir, doltDir string)
	describe string
}

func installedPackLayouts() []installedPackLayout {
	return []installedPackLayout{
		{
			name:     "synthetic repo cache",
			describe: "builtinpacks.MaterializeSyntheticRepo, the layout gc installs today",
			prepare: func(t *testing.T, _ string) (string, string) {
				t.Helper()
				repo := filepath.Join(t.TempDir(), "gascity-cache")
				if err := builtinpacks.MaterializeSyntheticRepo(repo, builtinpacks.Repository, "installed-layout-test"); err != nil {
					t.Fatalf("MaterializeSyntheticRepo: %v", err)
				}
				return filepath.Join(repo, "internal", "bootstrap", "packs", "core"),
					filepath.Join(repo, "examples", "bd", "dolt")
			},
		},
		{
			name:     "legacy system packs",
			describe: "<city>/.gc/system/packs/{core,dolt}, kept by cities with legacy refs",
			prepare: func(t *testing.T, cityDir string) (string, string) {
				t.Helper()
				coreDir := filepath.Join(cityDir, ".gc", "system", "packs", "core")
				doltDir := filepath.Join(cityDir, ".gc", "system", "packs", "dolt")
				copyPackFS(t, core.PackFS, coreDir)
				copyPackFS(t, dolt.PackFS, doltDir)
				return coreDir, doltDir
			},
		},
	}
}

func copyPackFS(t *testing.T, src fs.FS, dst string) {
	t.Helper()
	err := fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dst, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, builtinpacks.MaterializedFileMode(path))
	})
	if err != nil {
		t.Fatalf("copy pack to %s: %v", dst, err)
	}
}

// TestMaintenanceOrdersRunFromInstalledPackLayouts runs the reaper,
// jsonl-export and backup orders from the installed pack trees (not the
// source tree), so a packaging change that drops a helper they source fails
// here instead of silently disabling maintenance in real cities.
func TestMaintenanceOrdersRunFromInstalledPackLayouts(t *testing.T) {
	for _, layout := range installedPackLayouts() {
		t.Run(layout.name, func(t *testing.T) {
			f := newScopeFixture(t, "")
			f.env["FAKE_RIG_LIST_JSON"] = `{"rigs":[{"name":"api","hq":false}]}`
			f.env["FAKE_SCOPE_DBS"] = "rig:api=apidb"
			jsonlScopeEnv(t, f, filepath.Join(f.cityDir, "archive"))
			f.env["GC_SYSTEM_PACKS_DIR"] = filepath.Join(f.cityDir, ".gc", "system", "packs")
			coreDir, doltDir := layout.prepare(t, f.cityDir)

			for _, script := range []struct {
				path string
				env  map[string]string
				want string
			}{
				{filepath.Join(coreDir, "assets", "scripts", "reaper.sh"), nil, "--rig api purge --wisps-plane"},
				{filepath.Join(coreDir, "assets", "scripts", "jsonl-export.sh"), nil, "--rig api export --all -o "},
				{filepath.Join(doltDir, "assets", "scripts", "mol-dog-backup.sh"), map[string]string{"GC_PACK_DIR": doltDir}, "--rig api backup sync --json"},
			} {
				env := map[string]string{}
				for k, v := range f.env {
					env[k] = v
				}
				for k, v := range script.env {
					env[k] = v
				}
				cmd := exec.Command(script.path)
				cmd.Env = mergeTestEnv(env)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("%s (%s) failed: %v\n%s", script.path, layout.describe, err, out)
				}
				if log := f.read(t, f.gcLog); !strings.Contains(log, script.want) {
					t.Fatalf("%s did not reach the rig scope through gc bd (%q):\n%s", filepath.Base(script.path), script.want, log)
				}
			}
			if direct := f.read(t, f.doltLog); strings.Contains(direct, "DIRECT DOLT CALL") {
				t.Fatalf("an installed order dialed Dolt directly:\n%s", direct)
			}
		})
	}
}

// TestInstalledReaperFailsLoudlyWithoutItsScopeHelper pins that a packaging
// change dropping scope_bd.sh breaks the order visibly instead of quietly
// reaping nothing.
func TestInstalledReaperFailsLoudlyWithoutItsScopeHelper(t *testing.T) {
	layout := installedPackLayouts()[0]
	f := newScopeFixture(t, "")
	coreDir, _ := layout.prepare(t, f.cityDir)
	if err := os.Remove(filepath.Join(coreDir, "assets", "scripts", "scope_bd.sh")); err != nil {
		t.Fatalf("remove scope_bd.sh: %v", err)
	}
	cmd := exec.Command(filepath.Join(coreDir, "assets", "scripts", "reaper.sh"))
	cmd.Env = mergeTestEnv(f.env)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("reaper without scope_bd.sh exited 0:\n%s", out)
	}
	if !strings.Contains(string(out), "scope_bd.sh") {
		t.Fatalf("failure does not name the missing helper:\n%s", out)
	}
}
