package rig

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

var errBeforeMkdirRefused = errors.New("before-mkdir refused")

// beforeMkdirDeps returns Deps for a real temp city whose city.toml exists and
// whose NormalizeScopes succeeds, so a Provision that is not refused runs through
// the config write instead of failing on missing infrastructure.
func beforeMkdirDeps(t *testing.T) Deps {
	t.Helper()
	cityPath := t.TempDir()
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(originalCityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	deps := stubDeps(cityPath)
	deps.NormalizeScopes = func(string, *config.City) error { return nil }
	return deps
}

// TestProvision_BeforeMkdirRefusalPreventsDirCreation pins the race-narrowing
// seam: the hook runs immediately before the rig directory is created, and a
// refusal aborts Provision without creating anything at the rig path.
func TestProvision_BeforeMkdirRefusalPreventsDirCreation(t *testing.T) {
	deps := beforeMkdirDeps(t)
	rigPath := filepath.Join(t.TempDir(), "rig")

	var gotPath string
	deps.BeforeMkdir = func(p string) error {
		gotPath = p
		return errBeforeMkdirRefused
	}

	_, _, err := Provision(deps, ProvisionRequest{Name: "x", Path: rigPath})
	if !errors.Is(err, errBeforeMkdirRefused) {
		t.Fatalf("Provision err = %v, want the BeforeMkdir refusal", err)
	}
	if gotPath != rigPath {
		t.Errorf("BeforeMkdir path = %q, want %q", gotPath, rigPath)
	}
	if _, statErr := os.Lstat(rigPath); !os.IsNotExist(statErr) {
		t.Fatalf("rig dir exists after a refused BeforeMkdir (lstat err = %v)", statErr)
	}
}

// TestProvision_BeforeMkdirAllowsCreation proves a passing hook changes nothing:
// the directory is created as it is without the hook.
func TestProvision_BeforeMkdirAllowsCreation(t *testing.T) {
	deps := beforeMkdirDeps(t)
	rigPath := filepath.Join(t.TempDir(), "rig")

	called := 0
	deps.BeforeMkdir = func(string) error { called++; return nil }

	if _, _, err := Provision(deps, ProvisionRequest{Name: "x", Path: rigPath}); err != nil {
		t.Fatalf("Provision err = %v, want nil", err)
	}
	if called != 1 {
		t.Errorf("BeforeMkdir calls = %d, want 1", called)
	}
	if fi, err := os.Stat(rigPath); err != nil || !fi.IsDir() {
		t.Fatalf("rig dir not created (stat err = %v)", err)
	}
}

// TestProvision_BeforeMkdirSkippedWhenPathExists proves the hook guards only the
// creation: a path that already exists (or that a clone just materialized)
// reaches no MkdirAll, so the hook has nothing to narrow.
func TestProvision_BeforeMkdirSkippedWhenPathExists(t *testing.T) {
	deps := beforeMkdirDeps(t)
	rigPath := t.TempDir()

	deps.BeforeMkdir = func(string) error { return errBeforeMkdirRefused }

	_, _, err := Provision(deps, ProvisionRequest{Name: "x", Path: rigPath})
	if errors.Is(err, errBeforeMkdirRefused) {
		t.Fatalf("BeforeMkdir ran for a pre-existing rig path: %v", err)
	}
}
