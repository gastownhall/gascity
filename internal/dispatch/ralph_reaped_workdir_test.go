package dispatch

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/convergence"
)

// These tests cover the narrowed gastownhall/gascity#4021 reaped-workdir gate:
// a graph.v2 ralph control bead inherits a member step's gc.work_dir, and by the
// time the controller runs its gc.check_path gate that worktree may have been
// reaped. runRalphCheck must classify the worktree by stat-ing it directly and
// react per class: reaped -> durable-root fallback, inaccessible -> GateError
// (infra-retry), healthy -> unchanged.

func newReapedWorkdirControl(t *testing.T, store beads.Store, checkRel, workDir string) (beads.Bead, beads.Bead) {
	t.Helper()
	root := mustCreate(t, store, beads.Bead{Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow"}})
	control := mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.check_path":   filepath.ToSlash(checkRel),
			"gc.work_dir":     workDir,
			"gc.max_attempts": "3",
		},
	})
	subject := mustCreate(t, store, beads.Bead{
		Title:    "review loop iteration 1",
		Metadata: map[string]string{"gc.kind": "scope", "gc.root_bead_id": root.ID},
	})
	return control, subject
}

// Class 1 — reaped: os.Stat(work_dir) IsNotExist. The durable, pack-shipped gate
// lives under the store root, so blanking the reaped work_dir must revert both
// the script base and the process cwd to the durable root and the gate passes —
// never a GateError (which would burn #4176's infra-retry budget), never a hard
// error (which would quarantine the control).
func TestRunRalphCheckReapedWorkDirFallsBackToDurableRoot(t *testing.T) {
	cityPath := t.TempDir()
	checkRel := filepath.Join(".gc", "scripts", "checks", "design-review-approved.sh")
	durableScript := filepath.Join(cityPath, checkRel)
	if err := os.MkdirAll(filepath.Dir(durableScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(durableScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Per-task worktree under the city root, created then reaped.
	workDir := filepath.Join(cityPath, "worktrees", "task1")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(workDir); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	control, subject := newReapedWorkdirControl(t, store, checkRel, workDir)

	result, err := runRalphCheck(store, control, subject, 1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("runRalphCheck: %v (a reaped work_dir must fall back to the durable root, not hard-error)", err)
	}
	if result.Outcome != convergence.GatePass {
		t.Fatalf("Outcome = %q (stderr=%q), want pass via durable-root fallback", result.Outcome, result.Stderr)
	}
}

// Class 2 — inaccessible (stat error that is NOT IsNotExist): a parent path
// component is a regular file, so os.Stat(work_dir) returns ENOTDIR. This is an
// ambiguous, possibly-transient fault — the gate must NOT silently re-point
// scope to the durable root. It must surface a GateError (with a nil Go error so
// the control is not quarantined) so it enters #4176's bounded infra-retry
// channel instead of burning a Ralph attempt.
func TestRunRalphCheckInaccessibleWorkDirIsInfraRetryGateError(t *testing.T) {
	cityPath := t.TempDir()
	checkRel := filepath.Join(".gc", "scripts", "checks", "design-review-approved.sh")
	durableScript := filepath.Join(cityPath, checkRel)
	if err := os.MkdirAll(filepath.Dir(durableScript), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(durableScript, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A regular file stands in for a directory component: any stat/traversal of
	// a path UNDER it yields ENOTDIR (not IsNotExist), a deterministic,
	// root-safe stand-in for the EACCES/ELOOP inaccessible-worktree class.
	fileParent := filepath.Join(cityPath, "worktrees-file")
	if err := os.MkdirAll(filepath.Dir(fileParent), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileParent, []byte("reaped"), 0o644); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(fileParent, "task1")

	store := beads.NewMemStore()
	control, subject := newReapedWorkdirControl(t, store, checkRel, workDir)

	result, err := runRalphCheck(store, control, subject, 1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("runRalphCheck: %v (an inaccessible work_dir must be a GateError, not a hard error)", err)
	}
	if result.Outcome != convergence.GateError {
		t.Fatalf("Outcome = %q (stderr=%q), want GateError so it enters the bounded infra-retry channel", result.Outcome, result.Stderr)
	}
}

// Class 3 — healthy: os.Stat(work_dir) succeeds, so the stat classifier changes
// nothing. A relative check_path that exists under the worktree still resolves
// against the worktree (not the durable root). The store-root copy exits 7 and
// the worktree copy exits 0, so a pass proves the worktree script ran — the
// #4021 stat block did not blank a live work_dir or shadow it with the fallback.
func TestRunRalphCheckHealthyWorkDirUnchanged(t *testing.T) {
	cityPath := t.TempDir()
	checkRel := "check.sh"
	if err := os.WriteFile(filepath.Join(cityPath, checkRel), []byte("#!/bin/sh\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Join(cityPath, "worktrees", "task1")
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, checkRel), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	store := beads.NewMemStore()
	control, subject := newReapedWorkdirControl(t, store, checkRel, workDir)

	result, err := runRalphCheck(store, control, subject, 1, ProcessOptions{CityPath: cityPath})
	if err != nil {
		t.Fatalf("runRalphCheck: %v", err)
	}
	if result.Outcome != convergence.GatePass {
		t.Fatalf("Outcome = %q (stderr=%q), want pass from the worktree-relative script (healthy work_dir unchanged)", result.Outcome, result.Stderr)
	}
}
