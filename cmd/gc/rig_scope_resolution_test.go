package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestRigFromRedirectedBeadsDirIgnoresCwdOutsideCity verifies that when the
// caller's cwd is outside cityPath, any .beads/redirect found while walking
// the cwd's ancestor chain is ignored. The walk must be bounded by cityPath
// so that a polecat worktree's foreign-rig redirect (e.g., the shared rig
// repo checkout at /home/b/GIT/gascity/.beads) cannot bleed into rig
// resolution against an unrelated city.
func TestRigFromRedirectedBeadsDirIgnoresCwdOutsideCity(t *testing.T) {
	foreignRoot := filepath.Join(t.TempDir(), "foreign")
	if err := os.MkdirAll(filepath.Join(foreignRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	cwdRoot := t.TempDir()
	cwd := filepath.Join(cwdRoot, "worktree", "polecat-1")
	if err := os.MkdirAll(filepath.Join(cwd, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(cwd, ".beads", "redirect"),
		[]byte(filepath.Join(foreignRoot, ".beads")+"\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	cityDir := t.TempDir()
	rigDir := filepath.Join(cityDir, "rigs", "frontend")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: filepath.Join("rigs", "frontend"), Prefix: "fr"},
		},
	}

	rig, ok, err := rigFromRedirectedBeadsDir(cfg, cityDir, normalizePathForCompare(cwd))
	if err != nil {
		t.Fatalf("rigFromRedirectedBeadsDir() error = %v, want nil (cwd outside cityPath)", err)
	}
	if ok {
		t.Fatalf("rigFromRedirectedBeadsDir() ok = true, want false; rig = %+v", rig)
	}
}

// TestRigFromRedirectedBeadsDirTreatsCityStoreRedirectAsCityScope verifies
// that a .beads/redirect naming the city's own HQ store resolves to city
// scope instead of being refused as a foreign store. A city-scoped agent's
// worktree of the city repo (.gc/worktrees/<city>/<agent>) carries exactly
// this redirect, and its refusal broke every gc bd and gc formula call from
// that cwd that did not name an existing bead (ga-k1e9yp, ga-8cvakw).
func TestRigFromRedirectedBeadsDirTreatsCityStoreRedirectAsCityScope(t *testing.T) {
	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	// The same city reached through a symlink, as a pre_start templated from
	// an aliased city path would write it.
	cityAlias := filepath.Join(t.TempDir(), "city-alias")
	if err := os.Symlink(cityDir, cityAlias); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "demo"},
		Rigs: []config.Rig{
			{Name: "frontend", Path: filepath.Join("rigs", "frontend"), Prefix: "fr"},
		},
	}

	for _, tc := range []struct {
		name, agent, target string
	}{
		{name: "city path", agent: "pack-author", target: filepath.Join(cityDir, ".beads")},
		{name: "symlinked city path", agent: "aliased", target: filepath.Join(cityAlias, ".beads")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			worktree := filepath.Join(cityDir, ".gc", "worktrees", "demo", tc.agent)
			if err := os.MkdirAll(filepath.Join(worktree, ".beads"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(worktree, ".beads", "redirect"), []byte(tc.target+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			rig, ok, err := rigFromRedirectedBeadsDir(cfg, cityDir, normalizePathForCompare(worktree))
			if err != nil {
				t.Fatalf("rigFromRedirectedBeadsDir() error = %v, want nil (redirect names the city store)", err)
			}
			if ok {
				t.Fatalf("rigFromRedirectedBeadsDir() ok = true, want false (city scope); rig = %+v", rig)
			}
		})
	}
}
