package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/targetscope"
)

func seedBead(t *testing.T, store *beads.MemStore, metadata map[string]string) beads.Bead {
	t.Helper()
	bead, err := store.Create(beads.Bead{Title: "work", Metadata: metadata})
	if err != nil {
		t.Fatalf("creating bead: %v", err)
	}
	return bead
}

// Legacy behavior is preserved for the entire existing population: no scope
// reachable means reconcile may still stamp the session cwd.
func TestReconcileStampsCwdWorkDirWhenScopeAbsent(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedBead(t, store, nil)

	if !mayStampCwdWorkDir(store, bead, nil) {
		t.Fatal("an unscoped bead must keep legacy work_dir behavior")
	}
}

// THE DEFECT, INVERTED on the reconcile side.
func TestReconcileDoesNotStampCwdWorkDirWhenScopeValid(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedBead(t, store, map[string]string{
		beadmeta.TargetScopeMetadataKey: `{"v":1,"worktree":"/srv/declared"}`,
	})

	if mayStampCwdWorkDir(store, bead, nil) {
		t.Fatal("reconcile would stamp session cwd over a declared scope")
	}
}

// Whole-scope semantics: a scope declaring only a branch still suppresses the
// cwd work_dir stamp. The missing worktree field means unknown.
func TestReconcileDoesNotStampCwdWorkDirWhenOnlyBranchDeclared(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedBead(t, store, map[string]string{
		beadmeta.TargetScopeMetadataKey: `{"v":1,"branch":"release"}`,
	})

	if mayStampCwdWorkDir(store, bead, nil) {
		t.Fatal("a branch-only scope must still suppress the cwd work_dir stamp")
	}
}

// An unusable object is not permission to stamp cwd.
func TestReconcileDoesNotStampCwdWorkDirWhenScopeInvalid(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedBead(t, store, map[string]string{
		beadmeta.TargetScopeMetadataKey: `{"v":1,"worktree":"relative/path"}`,
	})

	if mayStampCwdWorkDir(store, bead, nil) {
		t.Fatal("an unusable scope must not fall through to a cwd stamp")
	}
}

// THE INHERITED CLASS. A formula-generated stage carries only a symbolic root
// reference, not a copy of root metadata — so the guard must reach the root's
// scope through the walk. This is the class the whole design exists for.
func TestReconcileHonorsScopeInheritedFromRoot(t *testing.T) {
	store := beads.NewMemStore()
	root := seedBead(t, store, map[string]string{
		beadmeta.TargetScopeMetadataKey: `{"v":1,"worktree":"/srv/declared"}`,
	})
	stage := seedBead(t, store, map[string]string{
		beadmeta.RootBeadIDMetadataKey: root.ID,
	})

	if mayStampCwdWorkDir(store, stage, nil) {
		t.Fatal("a stage must inherit the root's scope; bead-local reads miss exactly this class")
	}
}

// The same must hold through the parent chain.
func TestReconcileHonorsScopeInheritedFromParent(t *testing.T) {
	store := beads.NewMemStore()
	parent := seedBead(t, store, map[string]string{
		beadmeta.TargetScopeMetadataKey: `{"v":1,"branch":"release"}`,
	})
	child, err := store.Create(beads.Bead{Title: "child", ParentID: parent.ID, Metadata: beads.StringMap{}})
	if err != nil {
		t.Fatalf("creating child: %v", err)
	}

	if mayStampCwdWorkDir(store, child, nil) {
		t.Fatal("a child must inherit its parent's scope")
	}
}

// Poisoned flat keys are not a scope: a bead carrying only claim-derived
// gc.work_* still resolves ABSENT, so legacy behavior applies and the poison is
// never laundered into the new object.
func TestReconcileTreatsPoisonedFlatKeysAsUnscoped(t *testing.T) {
	store := beads.NewMemStore()
	bead := seedBead(t, store, map[string]string{
		beadmeta.WorkDirMetadataKey:    "/shared/poison",
		beadmeta.WorkBranchMetadataKey: "parked",
	})

	if !mayStampCwdWorkDir(store, bead, nil) {
		t.Fatal("flat gc.work_* keys must not be mistaken for a declared scope")
	}
	if got := targetscope.ResolveInherited(store, bead); got.State != targetscope.StateAbsent {
		t.Fatalf("state = %v, want absent", got.State)
	}
}
