package rig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
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

// A .beads/ that held no store before the add (only .beads/formulas, say) is
// not restored: what init wrote there stays, because a git_url teardown reads
// the database init minted from that metadata.json.
func TestProvisionLeavesInitFilesInNonStoreBeadsDir(t *testing.T) {
	initErr := errors.New("boom")
	deps, req := provisionWithFailingStoreInit(t, func(_, dir, _ string) (bool, error) {
		writePartialStore(t, dir)
		return false, initErr
	})
	formula := filepath.Join(req.Path, ".beads", "formulas", "keep.toml")
	if err := os.MkdirAll(filepath.Dir(formula), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(formula, []byte("formula = \"keep\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	_, _, provErr := Provision(deps, req)
	if provErr == nil || provErr.Error() != initErr.Error() {
		t.Fatalf("Provision error = %v, want the bare init error", provErr)
	}
	if _, err := os.Stat(formula); err != nil {
		t.Fatalf("pre-existing .beads content was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(req.Path, ".beads", "metadata.json")); err != nil {
		t.Fatalf("metadata.json init wrote into a non-store .beads/ was removed: %v", err)
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
