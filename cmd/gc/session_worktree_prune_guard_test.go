package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// guardedPruneFixture builds the on-disk shape the session close-path prune
// needs: a worker_dir under the city's .gc/worktrees tree with a .git
// marker, and a city whose rigs resolve the retiring seat's template to a
// rig root. gitProbe is stubbed (fakeGitProbe, from
// session_worktree_prune_test.go) so the WorktreeRemove the prune would
// perform is observable without a real git repository.
type guardedPruneFixture struct {
	env       *reconcilerTestEnv
	rigRoot   string
	workerDir string
}

func newGuardedPruneFixture(t *testing.T) *guardedPruneFixture {
	t.Helper()
	env := newReconcilerTestEnv(t)
	env.cfg = &config.City{
		Agents: []config.Agent{{Name: "worker"}},
	}
	rigRoot := filepath.Join(t.TempDir(), "app")
	workerDir := filepath.Join(env.city, ".gc", "worktrees", "app", "worker-1")
	if err := os.MkdirAll(workerDir, 0o755); err != nil {
		t.Fatalf("mkdir workerDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(workerDir, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatalf("write .git marker: %v", err)
	}

	orig := newGitProbe
	t.Cleanup(func() { newGitProbe = orig })
	newGitProbe = func(string) gitProbe {
		return &fakeGitProbe{isRepo: true, currentBranch: "gc-worker-1-abc"}
	}

	return &guardedPruneFixture{env: env, rigRoot: rigRoot, workerDir: workerDir}
}

// closeIdlePoolSeat parks a pool-managed session bead in the shape the
// pool-freeable close reclaims — asleep, idle, runtime absent, no assigned
// work — with its worker_dir recorded, and runs one reconciler pass over
// it, mirroring TestReconcileSessionBeads_AsleepIdlePoolBeadFreesSlot's
// direct reconcileSessionBeadsAtPath invocation.
func (fx *guardedPruneFixture) closeIdlePoolSeat(t *testing.T) {
	t.Helper()
	fx.env.cfg.Rigs = []config.Rig{{Name: "app", Path: fx.rigRoot}}
	fx.env.addDesired("worker-1-pool", "app/worker", false) // desired but not running
	session := fx.env.createSessionBead("worker-1-pool", "app/worker")
	fx.env.setSessionMetadata(&session, map[string]string{
		"state":                "asleep",
		"sleep_reason":         "idle",
		poolManagedMetadataKey: boolMetadata(true),
		"worker_dir":           fx.workerDir,
	})

	reconcileSessionBeadsAtPath(
		context.Background(),
		fx.env.city,
		[]beads.Bead{session},
		fx.env.desiredState,
		map[string]bool{},
		fx.env.cfg,
		fx.env.sp,
		fx.env.store,
		newFakeDrainOps(),
		nil,
		nil,
		nil,
		fx.env.dt,
		nil,
		false,
		nil,
		"",
		nil,
		fx.env.clk,
		fx.env.rec,
		0,
		0,
		&fx.env.stdout,
		&fx.env.stderr,
		withAsyncDrainAckStopTracker(fx.env.stops),
	)

	got, err := fx.env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", session.ID, err)
	}
	if got.Status != "closed" {
		t.Fatalf("precondition: seat not closed by the pool-freeable path (status %q, metadata=%v)", got.Status, got.Metadata)
	}
}

// TestIdlePoolClosePrunesTheSlotWorktree is the control arm: an idle pool
// seat's close reclaims its worktree when nothing else works in it — the
// newGitProbe stub's WorktreeRemove records the removal attempt.
func TestIdlePoolClosePrunesTheSlotWorktree(t *testing.T) {
	fx := newGuardedPruneFixture(t)
	var removed []string
	orig := newGitProbe
	newGitProbe = func(string) gitProbe {
		return &fakeGitProbe{isRepo: true, currentBranch: "gc-worker-1-abc", worktreeRemove: func(path string, _ bool) error {
			removed = append(removed, path)
			return nil
		}}
	}
	t.Cleanup(func() { newGitProbe = orig })

	fx.closeIdlePoolSeat(t)

	if len(removed) != 1 || removed[0] != fx.workerDir {
		t.Fatalf("worktree removals = %v, want exactly [%s]", removed, fx.workerDir)
	}
}

// TestIdlePoolCloseSparesThePruneWhenASuccessorSharesTheWorkDir is the
// defect arm of issue #1181: successive pool sessions share one slot
// checkout, so the idle close must first ask the store whether another open
// session row records the same worker_dir — and keep the worktree when one
// does, instead of deleting the successor's working tree under it.
func TestIdlePoolCloseSparesThePruneWhenASuccessorSharesTheWorkDir(t *testing.T) {
	fx := newGuardedPruneFixture(t)
	var removed []string
	orig := newGitProbe
	newGitProbe = func(string) gitProbe {
		return &fakeGitProbe{isRepo: true, currentBranch: "gc-worker-1-abc", worktreeRemove: func(path string, _ bool) error {
			removed = append(removed, path)
			return nil
		}}
	}
	t.Cleanup(func() { newGitProbe = orig })

	// The successor seat the floor already started in the same slot
	// directory — an open session row recording the same worker_dir.
	if _, err := fx.env.store.Create(sessionRow("succ", "worker_dir", fx.workerDir+"/")); err != nil {
		t.Fatal(err)
	}

	fx.closeIdlePoolSeat(t)

	if len(removed) != 0 {
		t.Fatalf("worktree removals = %v, want none — a successor session still works in %s", removed, fx.workerDir)
	}
	if _, err := os.Stat(filepath.Join(fx.workerDir, ".git")); err != nil {
		t.Errorf("the shared slot checkout did not survive the close: %v", err)
	}
}

// TestIdlePoolCloseStampsTheStaleMarkerOnASparedSharedWorkDir pins the
// operator affordance for the spared arm: the worktree a successor still
// works in is left with the .worktree-stale marker the later cleanup pass
// keys on, so a shared slot is eventually reclaimed once the successor is
// itself closed.
func TestIdlePoolCloseStampsTheStaleMarkerOnASparedSharedWorkDir(t *testing.T) {
	fx := newGuardedPruneFixture(t)
	orig := newGitProbe
	newGitProbe = func(string) gitProbe {
		return &fakeGitProbe{isRepo: true, currentBranch: "gc-worker-1-abc"}
	}
	t.Cleanup(func() { newGitProbe = orig })

	if _, err := fx.env.store.Create(sessionRow("succ", "worker_dir", fx.workerDir+"/")); err != nil {
		t.Fatal(err)
	}

	fx.closeIdlePoolSeat(t)

	assertWorktreeStaleMarker(t, fx.workerDir, "gc-worker-1-abc", "shared-with-open-session")
}

// TestPruneClosedSessionWorktreeIfUnusedGuardsEveryPrune pins the shared
// guard itself: the wrapper every close-path prune site routes through
// hands the seat to prune only when the store shows no other open session
// row working in the same directory.
func TestPruneClosedSessionWorktreeIfUnusedGuardsEveryPrune(t *testing.T) {
	city := t.TempDir()
	dir := filepath.Join(city, ".gc", "worktrees", "app", "worker-1")
	info := sessionpkg.Info{ID: "closed-1", WorkerDir: dir}

	t.Run("prunes when nothing else works in the directory", func(t *testing.T) {
		store := beads.NewMemStoreFrom(0, nil, nil)
		if !pruneClosedSessionWorktreeIfUnusedReached(store, info, city, t) {
			t.Fatal("guard blocked the prune with no other session row")
		}
	})
	t.Run("prunes when the seat records no worker_dir", func(t *testing.T) {
		store := beads.NewMemStoreFrom(0, nil, nil)
		if !pruneClosedSessionWorktreeIfUnusedReached(store, sessionpkg.Info{ID: "closed-1"}, city, t) {
			t.Fatal("guard blocked the prune for a seat with no worker_dir")
		}
	})
	t.Run("spares the worktree a successor row shares", func(t *testing.T) {
		store := beads.NewMemStoreFrom(0, []beads.Bead{sessionRow("succ", "worker_dir", dir+"/")}, nil)
		if pruneClosedSessionWorktreeIfUnusedReached(store, info, city, t) {
			t.Fatal("guard let the prune through although a successor row records the same worker_dir")
		}
	})
	t.Run("spares the worktree when the store read fails", func(t *testing.T) {
		store := censusErrStore{Store: beads.NewMemStore(), err: errors.New("unreadable")}
		if pruneClosedSessionWorktreeIfUnusedReached(store, info, city, t) {
			t.Fatal("guard let the prune through on a store read error; a read failure must count as in use")
		}
	})
}

// pruneClosedSessionWorktreeIfUnusedReached reports whether the guarded
// prune reached its prune function for this seat.
func pruneClosedSessionWorktreeIfUnusedReached(store beads.Store, info sessionpkg.Info, city string, t *testing.T) bool {
	t.Helper()
	reached := false
	pruneClosedSessionWorktreeIfUnused(store, func(sessionpkg.Info, string, *config.City, io.Writer) {
		reached = true
	}, info, city, nil, io.Discard)
	return reached
}
