package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/rig"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/ssrf"
)

func externalCloneFixture(t *testing.T, clone func(context.Context, string, string, git.CloneOptions) error) (*controllerState, string) {
	t.Helper()
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname=\"clone-city\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cs := newControllerState(context.Background(), &config.City{Workspace: config.Workspace{Name: "clone-city"}}, runtime.NewFake(), events.NewFake(), "clone-city", city)
	originalResolver, originalClone := ssrf.HostResolver, rigCloneGit
	ssrf.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("140.82.112.3")}, nil }
	rigCloneGit = clone
	t.Cleanup(func() { ssrf.HostResolver = originalResolver; rigCloneGit = originalClone })
	return cs, filepath.Join(t.TempDir(), "project")
}

// Reflection keeps this test-only regression compile-valid on the old manifest
// type: the isolated RED reaches the existing absolute-clone rejection first.
func externalCloneIdentity(m api.RigProvisionManifest) string {
	field := reflect.ValueOf(m).FieldByName("CreatedDirIdentity")
	if !field.IsValid() {
		return ""
	}
	return field.String()
}

func TestExternalRigCloneAbsoluteReachesCloneWithOwnedEmptyDirectory(t *testing.T) {
	failure := errors.New("bounded clone fixture failure")
	called := false
	cs, destination := externalCloneFixture(t, func(_ context.Context, _ string, path string, _ git.CloneOptions) error {
		called = true
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("clone did not receive owned physical directory: %v %v", info, err)
		}
		entries, err := os.ReadDir(path)
		if err != nil || len(entries) != 0 {
			t.Fatalf("reserved destination was not empty: %v %v", entries, err)
		}
		return failure
	})
	var manifests []api.RigProvisionManifest
	_, err := cs.ProvisionRigFromGit(context.Background(), config.Rig{Name: "project", Path: destination}, "https://example.com/repo.git", nil, func(m api.RigProvisionManifest) error { manifests = append(manifests, m); return nil })
	if !called || !errors.Is(err, rig.ErrCloneFailed) || !errors.Is(err, failure) {
		t.Fatalf("absolute destination never reached normal clone: called=%v err=%v", called, err)
	}
	if len(manifests) != 2 || manifests[0].CreatedDir != destination || externalCloneIdentity(manifests[0]) != "" || externalCloneIdentity(manifests[1]) == "" {
		t.Fatalf("intent/physical ownership not recorded before clone: %+v", manifests)
	}
}

func TestExternalRigClonePublishesOriginalAbsolutePath(t *testing.T) {
	cs, destination := externalCloneFixture(t, func(_ context.Context, _ string, path string, _ git.CloneOptions) error {
		return os.WriteFile(filepath.Join(path, "README.md"), []byte("cloned project bytes"), 0o600)
	})
	var last api.RigProvisionManifest
	got, err := cs.ProvisionRigFromGit(context.Background(), config.Rig{Name: "project", Path: destination, Prefix: "app", DefaultBranch: "main"}, "https://example.com/repo.git", nil, func(m api.RigProvisionManifest) error { last = m; return nil })
	if err != nil {
		t.Fatal(err)
	}
	if got.Path != destination || got.Name != "project" || externalCloneIdentity(last) == "" {
		t.Fatalf("clone identity/path=%+v manifest=%+v", got, last)
	}
	cfg := cs.Config()
	if len(cfg.Rigs) != 1 || cfg.Rigs[0].Path != destination {
		t.Fatalf("published wrong rig: %+v", cfg.Rigs)
	}
	if data, err := os.ReadFile(filepath.Join(destination, "README.md")); err != nil || string(data) != "cloned project bytes" {
		t.Fatalf("cloned contents changed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(cs.cityPath, "rigs", "project")); !os.IsNotExist(err) {
		t.Fatalf("created a relocated city copy: %v", err)
	}
}

func TestExternalRigCloneRefusesEveryPreexistingDestination(t *testing.T) {
	for _, kind := range []string{"empty", "repository", "nonempty"} {
		t.Run(kind, func(t *testing.T) {
			called := false
			cs, dest := externalCloneFixture(t, func(context.Context, string, string, git.CloneOptions) error { called = true; return nil })
			if err := os.Mkdir(dest, 0o700); err != nil {
				t.Fatal(err)
			}
			var sentinel string
			switch kind {
			case "repository":
				sentinel = filepath.Join(dest, ".git")
			case "nonempty":
				sentinel = filepath.Join(dest, "user-file")
			}
			if sentinel != "" {
				if err := os.WriteFile(sentinel, []byte("existing data"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			manifests := 0
			_, err := cs.ProvisionRigFromGit(context.Background(), config.Rig{Name: "project", Path: dest}, "https://example.com/repo.git", nil, func(api.RigProvisionManifest) error { manifests++; return nil })
			if !errors.Is(err, configedit.ErrValidation) || called || manifests != 0 {
				t.Fatalf("preexisting destination touched: err=%v clone=%v manifests=%d", err, called, manifests)
			}
			if sentinel != "" {
				if data, err := os.ReadFile(sentinel); err != nil || string(data) != "existing data" {
					t.Fatalf("preexisting data changed: %q %v", data, err)
				}
			}
		})
	}
}

func TestExternalRigCloneRefusesSymlinksAndMissingParent(t *testing.T) {
	for _, kind := range []string{"leaf", "dangling leaf", "parent", "missing parent"} {
		t.Run(kind, func(t *testing.T) {
			called := false
			cs, dest := externalCloneFixture(t, func(context.Context, string, string, git.CloneOptions) error { called = true; return nil })
			switch kind {
			case "leaf":
				if err := os.Symlink(t.TempDir(), dest); err != nil {
					t.Fatal(err)
				}
			case "dangling leaf":
				if err := os.Symlink(filepath.Join(t.TempDir(), "absent"), dest); err != nil {
					t.Fatal(err)
				}
			case "parent":
				link := filepath.Join(filepath.Dir(dest), "link")
				if err := os.Symlink(t.TempDir(), link); err != nil {
					t.Fatal(err)
				}
				dest = filepath.Join(link, "project")
			case "missing parent":
				dest = filepath.Join(filepath.Dir(dest), "missing", "project")
			}
			_, err := cs.ProvisionRigFromGit(context.Background(), config.Rig{Name: "project", Path: dest}, "https://example.com/repo.git", nil, func(api.RigProvisionManifest) error { return nil })
			if !errors.Is(err, configedit.ErrValidation) || called {
				t.Fatalf("unsafe destination accepted: clone=%v err=%v", called, err)
			}
		})
	}
}

func TestExternalRigCloneFailureCleanupAndRetryUsePhysicalOwnership(t *testing.T) {
	failure := errors.New("clone interrupted")
	cs, dest := externalCloneFixture(t, func(_ context.Context, _ string, path string, _ git.CloneOptions) error {
		if err := os.WriteFile(filepath.Join(path, "partial"), []byte("owned partial clone"), 0o600); err != nil {
			t.Fatal(err)
		}
		return failure
	})
	var first api.RigProvisionManifest
	_, err := cs.ProvisionRigFromGit(context.Background(), config.Rig{Name: "project", Path: dest}, "https://example.com/repo.git", nil, func(m api.RigProvisionManifest) error { first = m; return nil })
	if !errors.Is(err, rig.ErrCloneFailed) || externalCloneIdentity(first) == "" {
		t.Fatalf("missing failure ownership: %v %+v", err, first)
	}
	if err := cs.TeardownPartialRig(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("owned partial clone retained unexpectedly: %v", err)
	}
	if err := cs.TeardownPartialRig(context.Background(), first); err != nil {
		t.Fatalf("same cleanup replay not idempotent: %v", err)
	}
	var second api.RigProvisionManifest
	_, err = cs.ProvisionRigFromGit(context.Background(), config.Rig{Name: "project", Path: dest}, "https://example.com/repo.git", nil, func(m api.RigProvisionManifest) error { second = m; return nil })
	if !errors.Is(err, rig.ErrCloneFailed) || externalCloneIdentity(second) == "" {
		t.Fatalf("retry did not reach clone: %v %+v", err, second)
	}
	if err := cs.TeardownPartialRig(context.Background(), second); err != nil {
		t.Fatal(err)
	}
}

func TestExternalRigCloneCleanupRefusesReplacedDirectoryOrSymlink(t *testing.T) {
	for _, symlink := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory", true: "symlink"}[symlink], func(t *testing.T) {
			cs, dest := externalCloneFixture(t, func(context.Context, string, string, git.CloneOptions) error { return errors.New("clone failed") })
			var m api.RigProvisionManifest
			_, err := cs.ProvisionRigFromGit(context.Background(), config.Rig{Name: "project", Path: dest}, "https://example.com/repo.git", nil, func(row api.RigProvisionManifest) error { m = row; return nil })
			if !errors.Is(err, rig.ErrCloneFailed) {
				t.Fatal(err)
			}
			if err := os.Rename(dest, dest+"-retained"); err != nil {
				t.Fatal(err)
			}
			victim := dest
			if symlink {
				victim = t.TempDir()
				if err := os.Symlink(victim, dest); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(dest, 0o700); err != nil {
				t.Fatal(err)
			}
			sentinel := filepath.Join(victim, "user-file")
			if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := cs.TeardownPartialRig(context.Background(), m); err == nil {
				t.Fatal("stale ownership deleted replacement destination")
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserve" {
				t.Fatalf("replacement data changed: %q %v", data, err)
			}
		})
	}
}

func TestExternalRigCloneOwnershipPersistFailureRetainsUnprovenDirectory(t *testing.T) {
	called := false
	cs, dest := externalCloneFixture(t, func(context.Context, string, string, git.CloneOptions) error { called = true; return nil })
	failure := errors.New("ownership persist unavailable")
	calls := 0
	var persisted api.RigProvisionManifest
	_, err := cs.ProvisionRigFromGit(context.Background(), config.Rig{Name: "project", Path: dest}, "https://example.com/repo.git", nil, func(m api.RigProvisionManifest) error {
		calls++
		if calls == 1 {
			persisted = m
			return nil
		}
		return failure
	})
	if !errors.Is(err, failure) || called || calls != 2 {
		t.Fatalf("clone started without durable ownership: calls=%d clone=%v err=%v", calls, called, err)
	}
	if info, err := os.Lstat(dest); err != nil || !info.IsDir() {
		t.Fatalf("unproven reserved directory not retained: %v %v", info, err)
	}
	if err := cs.TeardownPartialRig(context.Background(), persisted); err == nil {
		t.Fatal("old external intent authorized removal without identity")
	}
	if _, err := os.Stat(dest); err != nil {
		t.Fatalf("unproven orphan removed: %v", err)
	}
}

func TestExternalRigClonePostCloneNameConflictCleansOnlyNewDestination(t *testing.T) {
	cs, dest := externalCloneFixture(t, func(_ context.Context, _ string, path string, _ git.CloneOptions) error {
		return os.WriteFile(filepath.Join(path, "clone-file"), []byte("new clone"), 0o600)
	})
	existing := t.TempDir()
	sentinel := filepath.Join(existing, "user-file")
	if err := os.WriteFile(sentinel, []byte("existing"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := cs.CreateRig(config.Rig{Name: "project", Path: existing, Prefix: "old"}, false); err != nil {
		t.Fatal(err)
	}
	var m api.RigProvisionManifest
	_, err := cs.ProvisionRigFromGit(context.Background(), config.Rig{Name: "project", Path: dest}, "https://example.com/repo.git", nil, func(row api.RigProvisionManifest) error { m = row; return nil })
	if err == nil || errors.Is(err, rig.ErrCloneFailed) {
		t.Fatalf("wanted post-clone config conflict, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "clone-file")); err != nil {
		t.Fatalf("fixture did not reach clone before conflict: %v", err)
	}
	if err := cs.TeardownPartialRig(context.Background(), m); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "existing" {
		t.Fatalf("existing project changed: %q %v", data, err)
	}
	if len(cs.Config().Rigs) != 1 || cs.Config().Rigs[0].Path != existing {
		t.Fatalf("existing registration changed: %+v", cs.Config().Rigs)
	}
}

func TestExternalRigCloneRefusesOwnershipChangeBeforeFetch(t *testing.T) {
	for _, stage := range []string{"parent after intent", "leaf after identity"} {
		t.Run(stage, func(t *testing.T) {
			called := false
			cs, dest := externalCloneFixture(t, func(context.Context, string, string, git.CloneOptions) error { called = true; return nil })
			parent := filepath.Join(filepath.Dir(dest), "container")
			if err := os.Mkdir(parent, 0o700); err != nil {
				t.Fatal(err)
			}
			dest = filepath.Join(parent, "project")
			victim := t.TempDir()
			sentinel := filepath.Join(victim, "user-file")
			if err := os.WriteFile(sentinel, []byte("preserve"), 0o600); err != nil {
				t.Fatal(err)
			}
			calls := 0
			_, err := cs.ProvisionRigFromGit(context.Background(), config.Rig{Name: "project", Path: dest}, "https://example.com/repo.git", nil, func(m api.RigProvisionManifest) error {
				calls++
				if stage == "parent after intent" && calls == 1 {
					if err := os.Rename(parent, parent+"-retained"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(victim, parent); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "leaf after identity" && externalCloneIdentity(m) != "" {
					if err := os.Rename(dest, dest+"-retained"); err != nil {
						t.Fatal(err)
					}
					if err := os.Symlink(victim, dest); err != nil {
						t.Fatal(err)
					}
				}
				return nil
			})
			if err == nil || called {
				t.Fatalf("changed ownership reached fetch: clone=%v err=%v", called, err)
			}
			if data, err := os.ReadFile(sentinel); err != nil || string(data) != "preserve" {
				t.Fatalf("foreign data changed: %q %v", data, err)
			}
		})
	}
}

func TestExternalRigCloneAbsoluteInsideCityIncompleteIdentitySurvivesSweep(t *testing.T) {
	cloneCalled := false
	cs, _ := externalCloneFixture(t, func(context.Context, string, string, git.CloneOptions) error { cloneCalled = true; return nil })
	destination := filepath.Join(cs.cityPath, "absolute-clone")
	persistFailure := errors.New("physical identity write failed")
	var first api.RigProvisionManifest
	calls := 0
	_, err := cs.ProvisionRigFromGit(context.Background(), config.Rig{Name: "project", Path: destination}, "https://example.com/repo.git", nil, func(m api.RigProvisionManifest) error {
		calls++
		if calls == 1 {
			first = m
			return nil
		}
		return persistFailure
	})
	if !errors.Is(err, persistFailure) || calls != 2 || cloneCalled {
		t.Fatalf("did not fail at physical-identity persistence: calls=%d clone=%v err=%v", calls, cloneCalled, err)
	}
	if err := os.Rename(destination, destination+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(destination, 0o755); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(destination, "user-replacement")
	if err := os.WriteFile(sentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Reconstruct the only durable intent, as a fresh controller would after a
	// crash. The second manifest never landed. Reflection keeps the RED fixture
	// compile-valid against the prior manifest type.
	metadata := map[string]string{
		"gc.idem.kind": "rig-create", "gc.idem.city": cs.cityPath,
		"gc.idem.state": "in_flight", "gc.idem.rig_name": first.RigName,
		"gc.idem.created_dir": first.CreatedDir,
	}
	if field := reflect.ValueOf(first).FieldByName("CreatedDirIdentityRequired"); field.IsValid() && field.Bool() {
		metadata["gc.idem.created_dir_identity_required"] = "true"
	}
	store := beads.NewMemStore()
	record, err := store.Create(beads.Bead{Title: "interrupted absolute clone", Metadata: metadata})
	if err != nil {
		t.Fatal(err)
	}
	if err := api.SweepOrphanRigProvisions(context.Background(), store, cs.cityPath, cs); err == nil {
		t.Fatal("sweep accepted an incomplete absolute ownership intent")
	}
	if data, err := os.ReadFile(sentinel); err != nil || string(data) != "keep" {
		t.Fatalf("unproven replacement was removed: %q %v", data, err)
	}
	got, err := store.Get(record.ID)
	if err != nil || got.Metadata["gc.idem.state"] != "in_flight" {
		t.Fatalf("sweep marked retained data clean: %+v %v", got, err)
	}
}
