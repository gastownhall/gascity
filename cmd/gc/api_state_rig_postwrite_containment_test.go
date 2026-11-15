package main

// Post-write containment re-check for rig creation (ga-j704cy.3.1).
//
// assertRigPathWithinCity runs BEFORE the rig directory is written. These tests
// pin a second, detect-only run of the same helper AFTER the write, at the two
// call sites that create the directory (the git_url clone and rig.Provision's
// MkdirAll). The re-check fails loud — a returned error — and never deletes,
// renames or modifies content that escaped. It does not close the
// swap-and-restore race; that is out of scope here.

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/git"
	"github.com/gastownhall/gascity/internal/ssrf"
)

// TestProvisionRigFromGitFailsWhenCloneLandsOutsideCity simulates a clone whose
// destination is a symlink to a directory outside the city by the time the clone
// returns. The post-clone re-check must return a containment error, leave the
// escaped content and the symlink untouched, and provision nothing there.
func TestProvisionRigFromGitFailsWhenCloneLandsOutsideCity(t *testing.T) {
	origResolver := ssrf.HostResolver
	ssrf.HostResolver = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("140.82.112.3")}, nil }
	defer func() { ssrf.HostResolver = origResolver }()

	outside := t.TempDir()
	marker := filepath.Join(outside, "keep.txt")
	if err := os.WriteFile(marker, []byte("outside content"), 0o644); err != nil {
		t.Fatal(err)
	}

	origClone := rigCloneGit
	rigCloneGit = func(_ context.Context, _, dst string, _ git.CloneOptions) error {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.Symlink(outside, dst)
	}
	defer func() { rigCloneGit = origClone }()

	// A real editor over an absent city.toml: if provisioning is (wrongly)
	// reached it fails on the missing config instead of dereferencing a nil
	// editor, so the failure below is an assertion rather than a panic.
	cityPath := t.TempDir()
	cs := &controllerState{
		cityPath: cityPath,
		editor:   configedit.NewEditor(fsys.OSFS{}, filepath.Join(cityPath, "city.toml")),
	}
	_, err := cs.ProvisionRigFromGit(context.Background(),
		config.Rig{Name: "escaped"},
		"https://example.com/repo.git",
		nil,
		func(api.RigProvisionManifest) error { return nil },
	)
	if err == nil {
		t.Fatal("ProvisionRigFromGit succeeded though the clone landed outside the city; the post-write re-check must fail loud")
	}
	if !errors.Is(err, configedit.ErrValidation) || !strings.Contains(err.Error(), "escapes the city root") {
		t.Fatalf("ProvisionRigFromGit = %v, want the containment error from assertRigPathWithinCity", err)
	}

	link := filepath.Join(cs.cityPath, "rigs", "escaped")
	if target, lerr := os.Readlink(link); lerr != nil || target != outside {
		t.Fatalf("clone destination symlink was removed or altered (readlink = %q, %v); the check is detect-only", target, lerr)
	}
	if got, rerr := os.ReadFile(marker); rerr != nil || string(got) != "outside content" {
		t.Fatalf("escaped content was modified or deleted (read = %q, %v)", got, rerr)
	}
	if _, serr := os.Stat(filepath.Join(outside, ".beads")); !os.IsNotExist(serr) {
		t.Fatalf("rig provisioning wrote into the outside directory (stat .beads = %v)", serr)
	}
}

// TestRigProvisionDepsVerifyRigPathEnforcesContainment pins that the API path
// wires the post-write hook to the existing containment helper: a rig path that
// resolves outside the city is rejected, one inside is accepted.
func TestRigProvisionDepsVerifyRigPathEnforcesContainment(t *testing.T) {
	cityPath := t.TempDir()
	cs := &controllerState{cityPath: cityPath}
	deps := cs.rigProvisionDeps(&config.City{}, config.Rig{Name: "x"}, nil)
	if deps.VerifyRigPath == nil {
		t.Fatal("rigProvisionDeps left VerifyRigPath nil; the API path must verify the created directory")
	}

	inside := filepath.Join(cityPath, "rigs", "ok")
	if err := os.MkdirAll(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := deps.VerifyRigPath(inside); err != nil {
		t.Fatalf("VerifyRigPath(inside) = %v, want nil", err)
	}

	outside := t.TempDir()
	link := filepath.Join(cityPath, "rigs", "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}
	if err := deps.VerifyRigPath(link); err == nil || !errors.Is(err, configedit.ErrValidation) {
		t.Fatalf("VerifyRigPath(symlink to outside) = %v, want a containment validation error", err)
	}
}
