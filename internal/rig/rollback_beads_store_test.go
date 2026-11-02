package rig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

// gas-4cu: a failed rig add must not leave a half-built .beads/ behind. The
// store init is the first step that writes into the rig directory, and when it
// fails partway the directory it created looks initialized to the next attempt
// — which then refuses with "already contains a beads store" and sends the
// operator hand-editing metadata.json.

// provisionWithFailingStoreInit wires a fresh add whose InitStore writes a
// partial store into the rig directory and then fails, the way a real init
// does when it writes metadata.json before the backing server rejects it.
func provisionWithFailingStoreInit(t *testing.T, initStore func(cityPath, dir, prefix string) (bool, error)) (Deps, ProvisionRequest) {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(originalCityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := stubDeps(cityPath)
	deps.NormalizeScopes = func(string, *config.City) error { return nil }
	deps.InitStore = initStore
	return deps, ProvisionRequest{Name: "storerig", Path: t.TempDir()}
}

// writePartialStore simulates the debris a failed init leaves: a .beads/ dir
// holding metadata.json, which is exactly what the fresh-add guard treats as
// an existing store on the retry.
func writePartialStore(t *testing.T, dir string) {
	t.Helper()
	beads := filepath.Join(dir, ".beads")
	if err := os.MkdirAll(beads, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beads, "metadata.json"), []byte(`{"backend":"dolt","dolt_database":"storerig"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProvisionRemovesPartialBeadsStoreWhenInitFails(t *testing.T) {
	initErr := errors.New("managed Dolt server unreachable while inspecting existing store")
	var rigPath string
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		writePartialStore(t, dir)
		return false, initErr
	})
	rigPath = req.Path

	_, _, provErr := Provision(deps, req)
	if provErr == nil {
		t.Fatal("expected the store-init failure to surface")
	}
	if !errors.Is(provErr, initErr) {
		t.Fatalf("error %v does not wrap the InitStore failure", provErr)
	}

	beadsPath := filepath.Join(rigPath, ".beads")
	if _, err := os.Stat(beadsPath); !os.IsNotExist(err) {
		entries, _ := os.ReadDir(beadsPath)
		t.Fatalf("%s survived a failed rig add (stat err = %v, entries = %v); a retry will refuse with \"already contains a beads store\"", beadsPath, err, entries)
	}
	// The rig directory itself is the user's repo — never ours to remove.
	if _, err := os.Stat(rigPath); err != nil {
		t.Fatalf("rig directory must survive the rollback: %v", err)
	}
}

func TestProvisionKeepsPreExistingBeadsStoreWhenInitFails(t *testing.T) {
	initErr := errors.New("boom")
	deps, req := provisionWithFailingStoreInit(t, func(_, _, _ string) (bool, error) {
		return false, initErr
	})
	// A store that was already on disk before the add is not ours to delete,
	// even when the add fails.
	writePartialStore(t, req.Path)
	marker := filepath.Join(req.Path, ".beads", "metadata.json")

	if _, _, provErr := Provision(deps, req); provErr == nil {
		t.Fatal("expected the store-init failure to surface")
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("pre-existing bead store was destroyed by the rollback: %v", err)
	}
}

func TestProvisionKeepsAdoptedBeadsStoreWhenInitFails(t *testing.T) {
	initErr := errors.New("boom")
	deps, req := provisionWithFailingStoreInit(t, func(_, _, _ string) (bool, error) {
		return false, initErr
	})
	req.Adopt = true
	// --adopt only runs store init under the bd store contract; without it the
	// adopt never reaches the failing init this test exercises.
	deps.StoreContract = func(string) bool { return true }
	writePartialStore(t, req.Path)
	if err := os.WriteFile(filepath.Join(req.Path, ".beads", "config.yaml"), []byte("issue_prefix: storerig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(req.Path, ".beads", "metadata.json")

	if _, _, provErr := Provision(deps, req); provErr == nil {
		t.Fatal("expected the store-init failure to surface")
	}

	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("--adopt store was destroyed by the rollback: %v", err)
	}
}

// An --adopt whose store init rewrites the store's scope files and then fails
// must leave them byte-identical, with the mode they had, and must not leave
// behind a dolt-server.port init wrote.
func TestProvisionRestoresAdoptedStoreFilesWhenInitFails(t *testing.T) {
	initErr := errors.New("exec beads init: managed Dolt server unreachable while inspecting existing store 'storerig'")
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		beads := filepath.Join(dir, ".beads")
		if err := os.WriteFile(filepath.Join(beads, "config.yaml"), []byte("issue_prefix: storerig\ndolt.auto-start: false\nexport.auto: false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beads, "metadata.json"), []byte(`{"backend":"dolt","dolt_database":"storerig","dolt_mode":"server"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beads, "dolt-server.port"), []byte("3307\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return false, initErr
	})
	req.Adopt = true
	req.Prefix = "storerig"
	deps.StoreContract = func(string) bool { return true }
	writePartialStore(t, req.Path)
	beads := filepath.Join(req.Path, ".beads")
	const yamlBefore = "# bd template\nissue-prefix: \"storerig\"\n"
	if err := os.WriteFile(filepath.Join(beads, "config.yaml"), []byte(yamlBefore), 0o600); err != nil {
		t.Fatal(err)
	}
	// bd creates both files 0600.
	for _, name := range []string{"config.yaml", "metadata.json"} {
		if err := os.Chmod(filepath.Join(beads, name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	metaBefore, err := os.ReadFile(filepath.Join(beads, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}

	_, _, provErr := Provision(deps, req)
	if !errors.Is(provErr, initErr) {
		t.Fatalf("Provision error = %v, want the init failure", provErr)
	}
	if got, _ := os.ReadFile(filepath.Join(beads, "config.yaml")); string(got) != yamlBefore {
		t.Errorf("config.yaml after failed adopt = %q, want %q", got, yamlBefore)
	}
	if got, _ := os.ReadFile(filepath.Join(beads, "metadata.json")); string(got) != string(metaBefore) {
		t.Errorf("metadata.json after failed adopt = %q, want %q", got, metaBefore)
	}
	if _, err := os.Stat(filepath.Join(beads, "dolt-server.port")); !os.IsNotExist(err) {
		t.Errorf("dolt-server.port the failed init wrote survived (stat err = %v)", err)
	}
	if provErr.Error() != initErr.Error() {
		t.Errorf("error text = %q, want the bare init error %q", provErr, initErr)
	}
	for _, name := range []string{"config.yaml", "metadata.json"} {
		info, err := os.Stat(filepath.Join(beads, name))
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode after restore = %v, want 0600", name, got)
		}
	}
}

// A restore that cannot put a pre-existing store's file back is reported with
// the init error rather than swallowed.
func TestProvisionReportsFailedRestoreOfPreexistingStore(t *testing.T) {
	initErr := errors.New("boom")
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		cfg := filepath.Join(dir, ".beads", "config.yaml")
		if err := os.Remove(cfg); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(cfg, "blocker"), 0o755); err != nil {
			t.Fatal(err)
		}
		return false, initErr
	})
	req.Adopt = true
	req.Prefix = "storerig"
	deps.StoreContract = func(string) bool { return true }
	writePartialStore(t, req.Path)
	if err := os.WriteFile(filepath.Join(req.Path, ".beads", "config.yaml"), []byte("issue_prefix: storerig\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, provErr := Provision(deps, req)
	if !errors.Is(provErr, initErr) {
		t.Fatalf("Provision error = %v, want the init failure", provErr)
	}
	if !strings.Contains(provErr.Error(), "(rollback failed: ") || !strings.Contains(provErr.Error(), "config.yaml") {
		t.Fatalf("failed restore not reported: %v", provErr)
	}
}

// A .beads that is a regular file has no store files to restore, so the init
// error is surfaced bare.
func TestProvisionBeadsFileIsNotAFailedRestore(t *testing.T) {
	initErr := errors.New("boom")
	deps, req := provisionWithFailingStoreInit(t, func(string, string, string) (bool, error) {
		return false, initErr
	})
	if err := os.WriteFile(filepath.Join(req.Path, ".beads"), []byte("not a dir\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _, provErr := Provision(deps, req)
	if provErr == nil || provErr.Error() != initErr.Error() {
		t.Fatalf("Provision error = %v, want the bare init error", provErr)
	}
}

// A .beads/ that held no store before the add (only .beads/formulas, say)
// loses what a failed init added to it — the markers that would make the
// retry refuse it as an existing store, and any other debris — and keeps every
// entry it had, untouched.
func TestProvisionRemovesInitFilesFromNonStoreBeadsDir(t *testing.T) {
	initErr := errors.New("boom")
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		writeInitDebris(t, dir, "storerig")
		if err := os.MkdirAll(filepath.Join(dir, ".beads", "dolt", "storerig"), 0o755); err != nil {
			t.Fatal(err)
		}
		return false, initErr
	})
	beads := filepath.Join(req.Path, ".beads")
	formula := writeNonStoreBeadsDir(t, req.Path)
	readme := filepath.Join(beads, "README.md")
	if err := os.WriteFile(readme, []byte("notes\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, provErr := Provision(deps, req)
	if provErr == nil || provErr.Error() != initErr.Error() {
		t.Fatalf("Provision error = %v, want the bare init error", provErr)
	}
	if got, want := beadsEntryNames(t, req.Path), "README.md,formulas"; got != want {
		t.Errorf(".beads entries after the failed add = %s, want %s", got, want)
	}
	for path, want := range map[string]string{formula: nonStoreFormulaBody, readme: "notes\n"} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("pre-existing .beads content was removed: %v", err)
		}
		if string(got) != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

// A failed init over a non-store .beads/ does not leave the retry refused at
// the fresh-add store guard (the gas-4cu shape).
func TestProvisionRetriesAfterFailedInitInNonStoreBeadsDir(t *testing.T) {
	initErr := errors.New("managed Dolt server unreachable while inspecting existing store")
	fail := true
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, prefix string) (bool, error) {
		writeInitDebris(t, dir, prefix)
		if fail {
			return false, initErr
		}
		return false, nil
	})
	formula := writeNonStoreBeadsDir(t, req.Path)

	if _, _, provErr := Provision(deps, req); !errors.Is(provErr, initErr) {
		t.Fatalf("first Provision error = %v, want the init failure", provErr)
	}
	fail = false
	if _, _, retryErr := Provision(deps, req); retryErr != nil {
		t.Fatalf("retry after a failed init = %v, want success", retryErr)
	}
	if got, err := os.ReadFile(formula); err != nil || string(got) != nonStoreFormulaBody {
		t.Errorf("pre-existing .beads content after the retry = %q, %v; want %q", got, err, nonStoreFormulaBody)
	}
}

// ga-hfly3b.44: a failed init that narrowed a non-store .beads/ puts its mode
// back.
func TestProvisionRestoresNonStoreBeadsDirModeWhenInitFails(t *testing.T) {
	initErr := errors.New("boom")
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		if err := os.Chmod(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		return false, initErr
	})
	writeNonStoreBeadsDir(t, req.Path)
	beads := filepath.Join(req.Path, ".beads")
	if err := os.Chmod(beads, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, _, provErr := Provision(deps, req); !errors.Is(provErr, initErr) {
		t.Fatalf("Provision error = %v, want the init failure", provErr)
	}
	assertDirPerm(t, beads, 0o755)
}

// ga-hfly3b.44: a failed adopt that narrowed the store's .beads/ puts its mode
// back along with the files.
func TestProvisionRestoresAdoptedStoreDirModeWhenInitFails(t *testing.T) {
	initErr := errors.New("boom")
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		if err := os.Chmod(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		return false, initErr
	})
	beads := adoptStoreRequest(t, &deps, &req)
	if _, _, provErr := Provision(deps, req); !errors.Is(provErr, initErr) {
		t.Fatalf("Provision error = %v, want the init failure", provErr)
	}
	assertDirPerm(t, beads, 0o755)
	if got, err := os.ReadFile(filepath.Join(beads, "config.yaml")); err != nil || string(got) != adoptedConfigYAML {
		t.Errorf("config.yaml after failed adopt = %q, %v; want %q", got, err, adoptedConfigYAML)
	}
}

// ga-hfly3b.44: an adopt whose init narrows .beads/ and succeeds, but whose
// guarded write window then fails, rolls the directory's mode back too.
func TestProvisionRollbackRestoresAdoptedStoreDirMode(t *testing.T) {
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		if err := os.Chmod(filepath.Join(dir, ".beads"), 0o700); err != nil {
			t.Fatal(err)
		}
		return false, nil
	})
	routesErr := errors.New("routes rejected")
	deps.WriteRoutes = func(string, *config.City) error { return routesErr }
	beads := adoptStoreRequest(t, &deps, &req)
	if _, _, provErr := Provision(deps, req); !errors.Is(provErr, routesErr) {
		t.Fatalf("Provision error = %v, want the routes failure", provErr)
	}
	assertDirPerm(t, beads, 0o755)
}

// removeFailingFS fails every Remove under prefix.
type removeFailingFS struct {
	fsys.FS
	prefix string
}

func (f removeFailingFS) Remove(name string) error {
	if strings.HasPrefix(name, f.prefix) {
		return &os.PathError{Op: "remove", Path: name, Err: syscall.EACCES}
	}
	return f.FS.Remove(name)
}

// A cleanup of a non-store .beads/ that cannot remove what init added is
// reported with the init error rather than swallowed.
func TestProvisionReportsFailedCleanupOfNonStoreBeadsDir(t *testing.T) {
	initErr := errors.New("boom")
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, prefix string) (bool, error) {
		writeInitDebris(t, dir, prefix)
		return false, initErr
	})
	writeNonStoreBeadsDir(t, req.Path)
	deps.FS = removeFailingFS{FS: fsys.OSFS{}, prefix: filepath.Join(req.Path, ".beads") + string(filepath.Separator)}

	_, _, provErr := Provision(deps, req)
	if !errors.Is(provErr, initErr) {
		t.Fatalf("Provision error = %v, want the init failure", provErr)
	}
	if !strings.Contains(provErr.Error(), "(rollback failed: ") || !strings.Contains(provErr.Error(), "metadata.json") {
		t.Fatalf("failed cleanup not reported: %v", provErr)
	}
}

// A non-store .beads/ record never joins a post-init rollback: an init that
// succeeded leaves its store there when the guarded write window fails, as
// for a fresh add, so a teardown can still read it.
func TestProvisionWriteWindowRollbackKeepsStoreInitCreatedInNonStoreBeadsDir(t *testing.T) {
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, prefix string) (bool, error) {
		writeInitDebris(t, dir, prefix)
		return false, nil
	})
	routesErr := errors.New("routes rejected")
	deps.WriteRoutes = func(string, *config.City) error { return routesErr }
	writeNonStoreBeadsDir(t, req.Path)
	if _, _, provErr := Provision(deps, req); !errors.Is(provErr, routesErr) {
		t.Fatalf("Provision error = %v, want the routes failure", provErr)
	}
	if _, err := os.Stat(filepath.Join(req.Path, ".beads", "metadata.json")); err != nil {
		t.Errorf("metadata.json a successful init wrote was removed by the write-window rollback: %v", err)
	}
}

const (
	nonStoreFormulaBody = "formula = \"keep\"\n"
	adoptedConfigYAML   = "issue_prefix: storerig\n"
)

// writeNonStoreBeadsDir gives the rig a .beads/ holding only formulas/keep.toml
// and returns that file's path.
func writeNonStoreBeadsDir(t *testing.T, rigPath string) string {
	t.Helper()
	formula := filepath.Join(rigPath, ".beads", "formulas", "keep.toml")
	if err := os.MkdirAll(filepath.Dir(formula), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formula, []byte(nonStoreFormulaBody), 0o644); err != nil {
		t.Fatal(err)
	}
	return formula
}

// writeInitDebris writes what a store init leaves in .beads/ before it fails:
// the store markers and a runtime port file.
func writeInitDebris(t *testing.T, rigPath, prefix string) {
	t.Helper()
	beads := filepath.Join(rigPath, ".beads")
	for name, data := range map[string]string{
		"metadata.json":    `{"backend":"dolt","dolt_database":"` + prefix + `"}`,
		"config.yaml":      "issue_prefix: " + prefix + "\n",
		"dolt-server.port": "3307\n",
	} {
		if err := os.WriteFile(filepath.Join(beads, name), []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// adoptStoreRequest turns req into an --adopt of a store at 0755 holding
// metadata.json and config.yaml, and returns its .beads/ path.
func adoptStoreRequest(t *testing.T, deps *Deps, req *ProvisionRequest) string {
	t.Helper()
	req.Adopt = true
	req.Prefix = "storerig"
	deps.StoreContract = func(string) bool { return true }
	writePartialStore(t, req.Path)
	beads := filepath.Join(req.Path, ".beads")
	if err := os.WriteFile(filepath.Join(beads, "config.yaml"), []byte(adoptedConfigYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(beads, 0o755); err != nil {
		t.Fatal(err)
	}
	return beads
}

func beadsEntryNames(t *testing.T, rigPath string) string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(rigPath, ".beads"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return strings.Join(names, ",")
}

func assertDirPerm(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Errorf("%s mode = %v, want %v", path, got, want)
	}
}

// An adopt whose init succeeds but whose guarded write window fails rolls the
// adopted store back to its pre-init bytes, not the ones init left.
func TestProvisionRollbackRestoresAdoptedStoreToPreInitBytes(t *testing.T) {
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		if err := os.WriteFile(filepath.Join(dir, ".beads", "config.yaml"), []byte("issue_prefix: storerig\ndolt.auto-start: false\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		return false, nil
	})
	routesErr := errors.New("routes rejected")
	deps.WriteRoutes = func(string, *config.City) error { return routesErr }
	req.Adopt = true
	req.Prefix = "storerig"
	deps.StoreContract = func(string) bool { return true }
	writePartialStore(t, req.Path)
	cfgPath := filepath.Join(req.Path, ".beads", "config.yaml")
	const yamlBefore = "issue-prefix: \"storerig\"\n"
	if err := os.WriteFile(cfgPath, []byte(yamlBefore), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, provErr := Provision(deps, req)
	if !errors.Is(provErr, routesErr) {
		t.Fatalf("Provision error = %v, want the routes failure", provErr)
	}
	got, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != yamlBefore {
		t.Errorf("config.yaml after rollback = %q, want %q", got, yamlBefore)
	}
	info, err := os.Stat(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("config.yaml mode after rollback = %v, want 0600", got)
	}
	cityTOML, err := os.ReadFile(filepath.Join(deps.CityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if string(cityTOML) != originalCityTOML {
		t.Errorf("city.toml after rollback = %q, want it restored", cityTOML)
	}
}

// An adopt whose init rewrites the store and succeeds, but whose topology
// snapshot then fails, returns the adopted store to its pre-init bytes and
// modes: the rollback of the guarded write window is not installed yet, so the
// snapshot failure must restore the store itself.
func TestProvisionRestoresAdoptedStoreWhenTopologySnapshotFails(t *testing.T) {
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		beads := filepath.Join(dir, ".beads")
		if err := os.WriteFile(filepath.Join(beads, "config.yaml"), []byte("issue_prefix: storerig\ndolt.auto-start: false\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beads, "metadata.json"), []byte(`{"backend":"dolt","dolt_database":"storerig","dolt_mode":"server"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(beads, "dolt-server.port"), []byte("3307\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return false, nil
	})
	req.Adopt = true
	req.Prefix = "storerig"
	deps.StoreContract = func(string) bool { return true }
	// A city scope file the topology snapshot cannot read.
	if err := os.MkdirAll(filepath.Join(deps.CityPath, ".beads", "config.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	writePartialStore(t, req.Path)
	beads := filepath.Join(req.Path, ".beads")
	const yamlBefore = "issue-prefix: \"storerig\"\n"
	if err := os.WriteFile(filepath.Join(beads, "config.yaml"), []byte(yamlBefore), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(beads, "metadata.json"), 0o600); err != nil {
		t.Fatal(err)
	}
	metaBefore, err := os.ReadFile(filepath.Join(beads, "metadata.json"))
	if err != nil {
		t.Fatal(err)
	}

	_, _, provErr := Provision(deps, req)
	if !errors.Is(provErr, syscall.EISDIR) || !strings.HasPrefix(provErr.Error(), "snapshot canonical files: ") {
		t.Fatalf("Provision error = %v, want the topology snapshot failure", provErr)
	}
	if strings.Contains(provErr.Error(), "rollback failed") {
		t.Errorf("a clean restore was reported as failed: %v", provErr)
	}
	for name, want := range map[string]string{"config.yaml": yamlBefore, "metadata.json": string(metaBefore)} {
		path := filepath.Join(beads, name)
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("%s after failed adopt = %q, want %q", name, got, want)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			t.Errorf("%s mode after restore = %v, want 0600", name, got)
		}
	}
	if _, err := os.Stat(filepath.Join(beads, "dolt-server.port")); !os.IsNotExist(err) {
		t.Errorf("dolt-server.port init wrote survived (stat err = %v)", err)
	}
}

// A restore that cannot put the adopted store's file back after the topology
// snapshot failed is reported with the snapshot error rather than swallowed.
func TestProvisionReportsFailedRestoreWhenTopologySnapshotFails(t *testing.T) {
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		// The topology snapshot fails to read config.yaml, and the restore
		// cannot write over it.
		cfg := filepath.Join(dir, ".beads", "config.yaml")
		if err := os.Remove(cfg); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(cfg, "blocker"), 0o755); err != nil {
			t.Fatal(err)
		}
		return false, nil
	})
	req.Adopt = true
	req.Prefix = "storerig"
	deps.StoreContract = func(string) bool { return true }
	writePartialStore(t, req.Path)
	if err := os.WriteFile(filepath.Join(req.Path, ".beads", "config.yaml"), []byte("issue_prefix: storerig\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, provErr := Provision(deps, req)
	if !errors.Is(provErr, syscall.EISDIR) || !strings.HasPrefix(provErr.Error(), "snapshot canonical files: ") {
		t.Fatalf("Provision error = %v, want the topology snapshot failure", provErr)
	}
	if !strings.Contains(provErr.Error(), "(rollback failed: ") {
		t.Fatalf("failed restore not reported: %v", provErr)
	}
}
