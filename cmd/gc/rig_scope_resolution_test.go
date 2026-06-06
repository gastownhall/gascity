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
