//go:build integration

package integration

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/doctor"
)

// TestBdStoreDeleteBatchOrphansExternalDependents proves the wisp-GC batch
// delete uses non-recursive `bd delete … --force` semantics against a REAL
// BdStore → bd CLI → Dolt SQL stack, not a spy.
//
// The wisp GC collects only an ownership closure and deletes it as a batch via
// beads.BatchDeleter. A live bead OUTSIDE that closure may depend on a closure
// member (convoy tracks, blocks/waits-for gates). The batch delete must remove
// exactly the collected ids and ORPHAN the external dependent — never delete it.
// Passing --cascade instead of --force here recursively deletes the dependent,
// which is the fleet-wide data-loss regression this test guards. A spy cannot
// catch that flag swap; only the real bd contract can.
func TestBdStoreDeleteBatchOrphansExternalDependents(t *testing.T) {
	requireDoltIntegration(t)
	env := newIsolatedToolEnv(t, true)

	rootDir := t.TempDir()
	doltDataDir := filepath.Join(rootDir, "dolt")
	wsDir := filepath.Join(rootDir, "ws")
	serverPort := startSharedDoltServer(t, env, doltDataDir)

	if err := os.MkdirAll(wsDir, 0o755); err != nil {
		t.Fatalf("creating workspace: %v", err)
	}
	gitInitWorkspace(t, wsDir)
	runBDInit(t, env, wsDir, "bd", serverPort)
	configureCustomTypes(t, env, wsDir, doctor.RequiredCustomTypes)

	store := beads.NewBdStore(wsDir, beads.ExecCommandRunner())

	// Ownership closure: root + child (child is a parent-child descendant).
	root, err := store.Create(beads.Bead{Title: "closure root", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	child, err := store.Create(beads.Bead{Title: "closure child", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("create child: %v", err)
	}
	// External dependent OUTSIDE the closure: survivor depends on child.
	survivor, err := store.Create(beads.Bead{Title: "external survivor", Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create survivor: %v", err)
	}
	if err := store.DepAdd(child.ID, root.ID, "parent-child"); err != nil {
		t.Fatalf("DepAdd child->root: %v", err)
	}
	if err := store.DepAdd(survivor.ID, child.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd survivor->child: %v", err)
	}

	batcher, ok := beads.Store(store).(beads.BatchDeleter)
	if !ok {
		t.Fatalf("BdStore does not satisfy beads.BatchDeleter")
	}
	if err := batcher.DeleteBatch([]string{root.ID, child.ID}); err != nil {
		t.Fatalf("DeleteBatch(root, child): %v", err)
	}

	// The collected closure is gone.
	for _, id := range []string{root.ID, child.ID} {
		if _, err := store.Get(id); err == nil {
			t.Errorf("closure member %s still present after batch delete, want deleted", id)
		}
	}

	// The external dependent is orphaned, NOT deleted. This is the assertion
	// that fails under --cascade.
	got, err := store.Get(survivor.ID)
	if err != nil {
		t.Fatalf("external survivor %s deleted by batch delete (regression: --cascade recursion): %v", survivor.ID, err)
	}
	if got.ID != survivor.ID {
		t.Fatalf("survivor Get returned %q, want %q", got.ID, survivor.ID)
	}

	// The backend dropped the now-dangling edge between survivor and the deleted
	// child (bd delete removes all dependency links touching the deleted ids).
	assertNoDepReferences(t, store, survivor.ID, child.ID)
}

func gitInitWorkspace(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "--quiet")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
}

// assertNoDepReferences fails if beadID has any dependency edge, in either
// direction, that references removedID.
func assertNoDepReferences(t *testing.T, store *beads.BdStore, beadID, removedID string) {
	t.Helper()
	for _, dir := range []string{"down", "up"} {
		deps, err := store.DepList(beadID, dir)
		if err != nil {
			t.Fatalf("DepList(%s, %s): %v", beadID, dir, err)
		}
		for _, d := range deps {
			if d.DependsOnID == removedID || d.IssueID == removedID {
				t.Errorf("bead %s retains %s edge referencing deleted bead %s: %+v", beadID, dir, removedID, d)
			}
		}
	}
}
