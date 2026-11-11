package main

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/ssrf"
)

// mustWriteFile writes a file, creating its parent directories.
func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestRemoveRigDirConfinedRemovesContainedDir(t *testing.T) {
	city := t.TempDir()
	rigDir := filepath.Join(city, "rigs", "x")
	mustWriteFile(t, filepath.Join(rigDir, ".beads", "metadata.json"), "{}")

	if err := removeRigDirConfined(city, rigDir); err != nil {
		t.Fatalf("removeRigDirConfined = %v, want nil", err)
	}
	if _, err := os.Lstat(rigDir); !os.IsNotExist(err) {
		t.Fatalf("rig dir still present (lstat err = %v)", err)
	}
	if _, err := os.Lstat(filepath.Join(city, "rigs")); err != nil {
		t.Fatalf("sibling parent removed along with the rig dir: %v", err)
	}
}

// A teardown of debris that is already gone (a crash before the clone landed, a
// second sweep) must stay the no-op os.RemoveAll was.
func TestRemoveRigDirConfinedMissingTargetIsNoOp(t *testing.T) {
	city := t.TempDir()
	for _, dir := range []string{
		filepath.Join(city, "rigs", "x"),   // parent missing
		filepath.Join(city, "x"),           // parent is the city
		filepath.Join(city, "a", "b", "c"), // several missing components
	} {
		if err := removeRigDirConfined(city, dir); err != nil {
			t.Errorf("removeRigDirConfined(%q) = %v, want nil for an absent dir", dir, err)
		}
	}
}

// The weaker of the two swap cases: the ancestor is already a symlink out of the
// city when this helper runs, so any implementation that validates the path first
// refuses it. TestRemoveRigDirConfinedRefusesAncestorSwappedAfterValidation is the
// one that pins the os.Root anchoring.
func TestRemoveRigDirConfinedRefusesAncestorSwappedToEscapingSymlink(t *testing.T) {
	city := t.TempDir()
	outside := t.TempDir()
	rigDir := filepath.Join(city, "rigs", "x")
	mustWriteFile(t, filepath.Join(rigDir, "keep"), "in-city")
	precious := filepath.Join(outside, "x", "precious")
	mustWriteFile(t, precious, "outside")

	// Swap: city/rigs becomes a symlink to the outside tree (which also holds an
	// "x" directory, as an attacker would arrange).
	if err := os.Rename(filepath.Join(city, "rigs"), filepath.Join(city, "rigs.moved")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(city, "rigs")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := removeRigDirConfined(city, rigDir); err == nil {
		t.Fatal("removeRigDirConfined followed a swapped ancestor symlink out of the city, want an error")
	}
	if b, err := os.ReadFile(precious); err != nil || string(b) != "outside" {
		t.Fatalf("out-of-city file disturbed: ReadFile = %q, %v", b, err)
	}
}

// The deterministic race: the ancestor is swapped for a symlink out of the city
// AFTER the helper validated the path and opened its root, injected through
// rigDirRemoveHook (no timing). A check-then-os.RemoveAll implementation follows
// the swapped link and deletes the outside tree; the os.Root-anchored removal
// refuses it, so this fails if the anchoring is reverted to a path-based removal.
func TestRemoveRigDirConfinedRefusesAncestorSwappedAfterValidation(t *testing.T) {
	city := t.TempDir()
	outside := t.TempDir()
	rigDir := filepath.Join(city, "rigs", "x")
	mustWriteFile(t, filepath.Join(rigDir, "keep"), "in-city")
	precious := filepath.Join(outside, "x", "precious")
	mustWriteFile(t, precious, "outside")

	orig := rigDirRemoveHook
	t.Cleanup(func() { rigDirRemoveHook = orig })
	rigDirRemoveHook = func() {
		if err := os.Rename(filepath.Join(city, "rigs"), filepath.Join(city, "rigs.moved")); err != nil {
			t.Errorf("rename: %v", err)
			return
		}
		if err := os.Symlink(outside, filepath.Join(city, "rigs")); err != nil {
			t.Errorf("symlink: %v", err)
		}
	}

	if err := removeRigDirConfined(city, rigDir); err == nil {
		t.Fatal("removeRigDirConfined followed an ancestor swapped after validation out of the city, want an error")
	}
	if b, err := os.ReadFile(precious); err != nil || string(b) != "outside" {
		t.Fatalf("out-of-city file disturbed: ReadFile = %q, %v", b, err)
	}
}

// A rig dir that is itself a symlink to another in-city directory must be
// unlinked, not followed: os.RemoveAll only ever unlinked the link, so deleting
// the target (<city>/.gc here) and leaving the link behind would be a regression.
func TestRemoveRigDirConfinedUnlinksSymlinkedRigDirWithoutFollowing(t *testing.T) {
	city := t.TempDir()
	sibling := filepath.Join(city, ".gc")
	mustWriteFile(t, filepath.Join(sibling, "state"), "keep")
	rigDir := filepath.Join(city, "rigs", "x")
	if err := os.MkdirAll(filepath.Dir(rigDir), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.Symlink(sibling, rigDir); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	if err := removeRigDirConfined(city, rigDir); err != nil {
		t.Fatalf("removeRigDirConfined = %v, want nil", err)
	}
	if _, err := os.Lstat(rigDir); !os.IsNotExist(err) {
		t.Errorf("rig dir link still present (lstat err = %v)", err)
	}
	if b, err := os.ReadFile(filepath.Join(sibling, "state")); err != nil || string(b) != "keep" {
		t.Fatalf("in-city link target disturbed: ReadFile = %q, %v", b, err)
	}
}

// CreatedDir == the city root would, with a path-based RemoveAll, delete the
// whole city. The anchored removal must fail closed instead.
func TestRemoveRigDirConfinedRefusesCityRoot(t *testing.T) {
	city := t.TempDir()
	mustWriteFile(t, filepath.Join(city, "city.toml"), "x")

	if err := removeRigDirConfined(city, city); err == nil {
		t.Fatal("removeRigDirConfined(city, city) = nil, want an error")
	}
	if _, err := os.Stat(filepath.Join(city, "city.toml")); err != nil {
		t.Fatalf("city contents removed: %v", err)
	}
}

// resolveStoreScopeRoot stores CreatedDir symlink-resolved while cityPath may be
// the raw linked path (~/gc -> /real/city); the confinement must not read that as
// an escape and must still remove the rig.
func TestRemoveRigDirConfinedUnderSymlinkedCity(t *testing.T) {
	realCity := t.TempDir()
	link := filepath.Join(t.TempDir(), "gc")
	if err := os.Symlink(realCity, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	rigDir := filepath.Join(realCity, "rigs", "x")
	mustWriteFile(t, filepath.Join(rigDir, "f"), "x")

	if err := removeRigDirConfined(link, rigDir); err != nil {
		t.Fatalf("removeRigDirConfined(link, real/rigs/x) = %v, want nil", err)
	}
	if _, err := os.Lstat(rigDir); !os.IsNotExist(err) {
		t.Fatalf("rig dir still present (lstat err = %v)", err)
	}
}

func TestTeardownPartialRigRemovesRigUnderSymlinkedCity(t *testing.T) {
	realCity := t.TempDir()
	link := filepath.Join(t.TempDir(), "gc")
	if err := os.Symlink(realCity, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	rigDir := filepath.Join(realCity, "rigs", "x")
	mustWriteFile(t, filepath.Join(rigDir, "README"), "debris")

	cs := &controllerState{cityPath: link}
	if err := cs.TeardownPartialRig(context.Background(), api.RigProvisionManifest{RigName: "x", CreatedDir: rigDir}); err != nil {
		t.Fatalf("TeardownPartialRig = %v, want nil", err)
	}
	if _, err := os.Lstat(rigDir); !os.IsNotExist(err) {
		t.Fatalf("rig dir still present (lstat err = %v)", err)
	}
}

// The git_url path re-asserts containment immediately before the clone. The
// manifest callback runs between the first check and the clone, so swapping the
// (then-absent) rigs/ directory for an escaping symlink there is a deterministic
// post-check swap.
func TestProvisionRigFromGitRechecksContainmentBeforeClone(t *testing.T) {
	origResolver := ssrf.HostResolver
	ssrf.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("140.82.112.3")}, nil }
	defer func() { ssrf.HostResolver = origResolver }()

	cloneCalled := false
	origClone := rigCloneGit
	rigCloneGit = func(context.Context, string, string, git.CloneOptions) error {
		cloneCalled = true
		return nil
	}
	defer func() { rigCloneGit = origClone }()

	city := t.TempDir()
	outside := t.TempDir()
	cs := &controllerState{cityPath: city}
	var err error
	// Without the re-check the call runs on past the clone into the config
	// transaction, which this bare controllerState cannot serve; recover so the
	// clone assertion below reports the real defect instead of a nil dereference.
	func() {
		defer func() { _ = recover() }()
		_, err = cs.ProvisionRigFromGit(context.Background(),
			config.Rig{Name: "swap"},
			"https://example.com/repo.git",
			nil,
			func(api.RigProvisionManifest) error {
				return os.Symlink(outside, filepath.Join(city, "rigs"))
			},
		)
	}()
	if cloneCalled {
		t.Fatal("clone ran after the ancestor was swapped to an escaping symlink")
	}
	if !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("ProvisionRigFromGit after a post-check ancestor swap = %v, want ErrValidation", err)
	}
}

// The sync and git_url provisioning both build their rig.Deps here; the hook
// must re-assert city containment right before the rig directory is created.
func TestRigProvisionDepsBeforeMkdirAssertsContainment(t *testing.T) {
	city := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(city, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	cs := &controllerState{cityPath: city}
	deps := cs.rigProvisionDeps(&config.City{}, config.Rig{}, nil)
	if deps.BeforeMkdir == nil {
		t.Fatal("rigProvisionDeps left BeforeMkdir nil; the API path must re-assert containment before MkdirAll")
	}
	if err := deps.BeforeMkdir(filepath.Join(city, "rigs", "ok")); err != nil {
		t.Errorf("BeforeMkdir(in-city) = %v, want nil", err)
	}
	if err := deps.BeforeMkdir(filepath.Join(city, "link", "rig")); !errors.Is(err, configedit.ErrValidation) {
		t.Errorf("BeforeMkdir(symlink escape) = %v, want ErrValidation", err)
	}
}
