package main

// Regression coverage for the symlinked-city CreateRig rejection (adjacent
// defect found while investigating ga-xbilek).
//
// It needs no macOS and no GOTMPDIR emulation — it creates its own symlink.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/configedit"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestAssertRigPathWithinCityAcceptsResolvedTargetUnderSymlinkedCity pins that a
// rig living INSIDE the city validates even when the city is reached through a
// symlinked ancestor (e.g. ~/gc -> /data/gc, the exact case
// resolveStoreScopeRoot's own comment says it supports).
//
// controllerState.CreateRig sets r.Path = resolveStoreScopeRoot(...), which
// normalizes through pathutil and therefore RESOLVES the symlink, then calls
// assertRigPathWithinCity(cs.cityPath, r.Path) with cs.cityPath still in its
// UNRESOLVED form. assertRigPathWithinCity normalizes both operands before the
// lexical relWithinCity pass, so those two forms no longer disagree and an
// in-city rig is not reported as an escape. The independent symlink-aware pass
// that follows is unchanged, so a genuine escape must still fail both checks.
func TestAssertRigPathWithinCityAcceptsResolvedTargetUnderSymlinkedCity(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	cityPath := filepath.Join(link, "city")
	rigPath := filepath.Join(cityPath, "repo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	resolved := resolveStoreScopeRoot(cityPath, rigPath)
	if resolved == rigPath {
		t.Fatalf("precondition: resolveStoreScopeRoot did not resolve the symlink (got %q)", resolved)
	}

	if err := assertRigPathWithinCity(cityPath, resolved); err != nil {
		t.Fatalf("assertRigPathWithinCity(%q, %q) = %v, want nil: the rig is inside the city, "+
			"only the two arguments disagree about symlink resolution", cityPath, resolved, err)
	}
}

// TestAssertRigPathWithinCityAcceptsWhenBothSidesResolved disproves the
// alternative hypothesis that the symlink-AWARE second pass is at fault: with
// both arguments in the same form the identical layout validates cleanly.
func TestAssertRigPathWithinCityAcceptsWhenBothSidesResolved(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("symlinks unsupported on this platform: %v", err)
	}

	cityPath := filepath.Join(link, "city")
	rigPath := filepath.Join(cityPath, "repo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	resolvedCity, err := filepath.EvalSymlinks(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := assertRigPathWithinCity(resolvedCity, resolveStoreScopeRoot(cityPath, rigPath)); err != nil {
		t.Fatalf("assertRigPathWithinCity(%q, ...) = %v, want nil", resolvedCity, err)
	}
}

// A second API binding of the same project is a typed 409 conflict, including
// when the composed snapshot has not yet observed the first registration.
// The authoritative raw-config check must run before store provisioning.
func TestControllerStateCreateRigRejectsCanonicalPathConflict(t *testing.T) {
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	for _, stale := range []bool{false, true} {
		for _, alias := range []bool{false, true} {
			for _, adopt := range []bool{false, true} {
				t.Run(fmt.Sprintf("stale=%v/symlink=%v/adopt=%v", stale, alias, adopt), func(t *testing.T) {
					root := t.TempDir()
					city := filepath.Join(root, "city")
					project := filepath.Join(root, "projects", "existing")
					for _, path := range []string{city, filepath.Join(project, ".beads")} {
						if err := os.MkdirAll(path, 0o755); err != nil {
							t.Fatal(err)
						}
					}
					requestPath := project
					if alias {
						requestPath = filepath.Join(root, "project-link")
						if err := os.Symlink(project, requestPath); err != nil {
							t.Fatalf("creating the test symlink: %v", err)
						}
					}
					// Exercise a stored relative path as well as an absolute request;
					// comparing either against the daemon CWD would miss the holder.
					storedPath, err := filepath.Rel(city, project)
					if err != nil {
						t.Fatal(err)
					}
					cityFile := filepath.Join(city, "city.toml")
					protected := map[string]string{
						cityFile:                                             fmt.Sprintf("[workspace]\nname=\"city1\"\n[[rigs]]\nname=\"existing-project\"\npath=%q\nprefix=\"kept\"\n", filepath.ToSlash(storedPath)),
						filepath.Join(project, "dirty.txt"):                  "existing dirty bytes\n",
						filepath.Join(project, "untracked.txt"):              "existing untracked bytes\n",
						filepath.Join(project, ".beads", "metadata.json"):    "{}\n",
						filepath.Join(project, ".beads", "config.yaml"):      "issue-prefix: kept\n",
						filepath.Join(project, ".beads", "history.sentinel"): "existing queue history\n",
					}
					for path, text := range protected {
						if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
							t.Fatal(err)
						}
					}
					cfg := &config.City{Workspace: config.Workspace{Name: "city1"}}
					if !stale {
						cfg.Rigs = []config.Rig{{Name: "existing-project", Path: storedPath, Prefix: "kept"}}
					}
					cs := newControllerState(context.Background(), cfg, runtime.NewFake(), events.NewFake(), "city1", city)
					initCalls := 0
					oldInit := controllerStateInitRigDirIfReady
					controllerStateInitRigDirIfReady = func(_, _, _ string) (bool, error) {
						initCalls++
						return false, errors.New("duplicate project reached store provisioning")
					}
					defer func() { controllerStateInitRigDirIfReady = oldInit }()
					err = cs.CreateRig(config.Rig{Name: "duplicate-path", Path: requestPath}, adopt)
					if !errors.Is(err, configedit.ErrAlreadyExists) {
						t.Fatalf("same canonical project under another name = %v, want typed ErrAlreadyExists (HTTP409)", err)
					}
					if !strings.Contains(err.Error(), "existing-project") {
						t.Fatalf("path conflict omits the owning rig: %v", err)
					}
					if initCalls != 0 {
						t.Fatalf("duplicate path reached store initialization %d times", initCalls)
					}
					for path, want := range protected {
						got, err := os.ReadFile(path)
						if err != nil || string(got) != want {
							t.Fatalf("conflict changed retained file %s: %q, %v", path, got, err)
						}
					}
					if alias {
						if target, err := os.Readlink(requestPath); err != nil || target != project {
							t.Fatalf("conflict changed the project symlink: %q, %v", target, err)
						}
					}
				})
			}
		}
	}
}
