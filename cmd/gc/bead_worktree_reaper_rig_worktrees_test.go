package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pathutil"
)

// addRigWorktreesDirWorktree adds a per-bead worktree at
// <rigRoot>/worktrees/<name>, the layout mol-scoped-work and
// mol-polecat-commit create with a raw `git worktree add`, outside the city's
// .gc/worktrees/<rig>/ tree. Like addClosedWorktreeWithAge, a positive age
// backdates the worktree's creation signal past (or short of) the freshness
// quarantine; age == 0 leaves the real just-created mtime in place.
func addRigWorktreesDirWorktree(t *testing.T, rigRoot, name string, age time.Duration) string {
	t.Helper()
	wtPath := filepath.Join(rigRoot, "worktrees", name)
	if err := os.MkdirAll(filepath.Dir(wtPath), 0o755); err != nil {
		t.Fatalf("mkdir worktree parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "wt-"+name, wtPath)
	if age > 0 {
		backdateWorktreeGitFile(t, wtPath, age)
	}
	return wtPath
}

// reapRigWorktreesDir runs one real (non-dry) reaper pass over the rig's
// single store.
func reapRigWorktreesDir(t *testing.T, cityPath string, cfg *config.City, store beads.Store, dryRun bool) (reapReport, string) {
	t.Helper()
	var stderr bytes.Buffer
	report := reapClosedBeadWorktrees(cityPath, cfg, map[string]beads.Store{reapTestRigName: store}, nil, dryRun, events.Discard, nil, &stderr)
	return report, stderr.String()
}

// TestReapClosedBeadWorktrees_ReapsIdleRigWorktreesDirWorktree is the
// canonical test for the widened scan root (ga-4wem3l): a worktree at
// <rig>/worktrees/<bead-id> for a closed bead is reaped, though it sits
// nowhere under .gc/worktrees/.
func TestReapClosedBeadWorktrees_ReapsIdleRigWorktreesDirWorktree(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addRigWorktreesDirWorktree(t, rigRoot, "ga-rig001", 24*time.Hour)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-rig001", Status: "closed"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, stderr := reapRigWorktreesDir(t, cityPath, reapTestConfig(rigRoot), store, false)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-rig001" {
		t.Fatalf("Reaped = %+v, want exactly ga-rig001\nstderr:\n%s", report.Reaped, stderr)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after reap (stat err=%v)", wt, err)
	}
}

// TestReapClosedBeadWorktrees_RigWorktreesDirKeepsEverySafetyGate proves the
// widened root reuses each existing gate rather than bypassing it: every case
// is a closed bead's worktree under <rig>/worktrees/ that must be reported
// protected (so it was scanned) and left on disk.
func TestReapClosedBeadWorktrees_RigWorktreesDirKeepsEverySafetyGate(t *testing.T) {
	tests := []struct {
		name       string
		age        time.Duration
		agentNames []string
		liveCwd    bool
		dirty      bool
		borrower   bool
		wantReason string
	}{
		{name: "freshness quarantine", age: 0, wantReason: "quarantine"},
		{name: "live process", age: 24 * time.Hour, liveCwd: true, wantReason: "live"},
		{name: "uncommitted work", age: 24 * time.Hour, dirty: true, wantReason: "unsafe git state"},
		{name: "borrow veto", age: 24 * time.Hour, borrower: true, wantReason: "borrow-veto"},
		{name: "agent home name", age: 24 * time.Hour, agentNames: []string{"ga-gate01"}, wantReason: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cityPath, rigRoot := initReapRig(t)
			wt := addRigWorktreesDirWorktree(t, rigRoot, "ga-gate01", tc.age)
			cfg := reapTestConfig(rigRoot)
			for _, name := range tc.agentNames {
				cfg.Agents = append(cfg.Agents, config.Agent{Name: name})
			}
			seed := []beads.Bead{{ID: "ga-gate01", Status: "closed"}}
			if tc.borrower {
				seed = append(seed, beads.Bead{ID: "ga-other02", Status: "open", Metadata: map[string]string{beadmeta.WorkDirMetadataKey: wt}})
			}
			store := beads.NewMemStoreFrom(1, seed, nil)
			state := liveWorktreeState{scanned: true}
			if tc.liveCwd {
				state.cwds = []string{pathutil.NormalizePathForCompare(wt)}
			}
			injectLiveness(t, state)
			if tc.dirty {
				if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("edited\n"), 0o644); err != nil {
					t.Fatalf("dirty the worktree: %v", err)
				}
			}

			report, stderr := reapRigWorktreesDir(t, cityPath, cfg, store, false)

			if len(report.Reaped) != 0 {
				t.Fatalf("Reaped = %+v, want 0\nstderr:\n%s", report.Reaped, stderr)
			}
			if _, err := os.Stat(wt); err != nil {
				t.Fatalf("protected worktree %s was removed: %v", wt, err)
			}
			// The agent-home guard skips silently, as it does for the
			// .gc/worktrees root; every other gate reports a protected entry.
			if tc.wantReason == "" {
				return
			}
			if len(report.Protected) != 1 || !strings.Contains(report.Protected[0].Reason, tc.wantReason) {
				t.Fatalf("Protected = %+v, want exactly 1 entry whose reason contains %q", report.Protected, tc.wantReason)
			}
		})
	}
}

// TestReapClosedBeadWorktrees_RigWorktreesDirSkipsOpenBead keeps the
// bead-must-exist-and-be-closed gate: an in-flight bead's worktree under
// <rig>/worktrees/ is never reaped.
func TestReapClosedBeadWorktrees_RigWorktreesDirSkipsOpenBead(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addRigWorktreesDirWorktree(t, rigRoot, "ga-open01", 24*time.Hour)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-open01", Status: "in_progress"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, stderr := reapRigWorktreesDir(t, cityPath, reapTestConfig(rigRoot), store, false)

	if len(report.Reaped) != 0 {
		t.Fatalf("Reaped = %+v, want 0 for an open bead\nstderr:\n%s", report.Reaped, stderr)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("open bead's worktree %s was removed: %v", wt, err)
	}
}

// TestReapClosedBeadWorktrees_RigWorktreesDirDryRunRemovesNothing keeps dry-run
// semantics for the widened root: reported as would-reap, left on disk.
func TestReapClosedBeadWorktrees_RigWorktreesDirDryRunRemovesNothing(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addRigWorktreesDirWorktree(t, rigRoot, "ga-dry001", 24*time.Hour)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-dry001", Status: "closed"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, stderr := reapRigWorktreesDir(t, cityPath, reapTestConfig(rigRoot), store, true)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-dry001" {
		t.Fatalf("Reaped = %+v, want ga-dry001 reported as would-reap\nstderr:\n%s", report.Reaped, stderr)
	}
	if _, err := os.Stat(wt); err != nil {
		t.Fatalf("dry-run removed worktree %s: %v", wt, err)
	}
}

// TestReapClosedBeadWorktrees_RigWorktreesDirResolvesParentNamedBead proves
// the bead-ID extraction's one-level parent climb is bounded by the matched
// scan root, not by the .gc/worktrees one: <rig>/worktrees/<bead>-<slug>/worktree
// names its bead only through the parent directory.
func TestReapClosedBeadWorktrees_RigWorktreesDirResolvesParentNamedBead(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	wt := addRigWorktreesDirWorktree(t, rigRoot, filepath.Join("ga-nest01-slug", "worktree"), 24*time.Hour)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-nest01", Status: "closed"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, stderr := reapRigWorktreesDir(t, cityPath, reapTestConfig(rigRoot), store, false)

	if len(report.Reaped) != 1 || report.Reaped[0].BeadID != "ga-nest01" {
		t.Fatalf("Reaped = %+v, want exactly ga-nest01\nstderr:\n%s", report.Reaped, stderr)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("worktree %s still present after reap (stat err=%v)", wt, err)
	}
}

// TestReapClosedBeadWorktrees_OnlyRigWorktreesDirIsWidened pins the other
// edge of the new scope: a worktree elsewhere under the rig root (a sibling of
// worktrees/, or the worktrees directory's own parent, the rig's main working
// tree) is neither reaped nor reported.
func TestReapClosedBeadWorktrees_OnlyRigWorktreesDirIsWidened(t *testing.T) {
	cityPath, rigRoot := initReapRig(t)
	sibling := filepath.Join(rigRoot, "scratch", "ga-side01")
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		t.Fatalf("mkdir sibling parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "wt-side01", sibling)
	backdateWorktreeGitFile(t, sibling, 24*time.Hour)
	store := beads.NewMemStoreFrom(1, []beads.Bead{{ID: "ga-side01", Status: "closed"}}, nil)
	injectLiveness(t, liveWorktreeState{scanned: true})

	report, stderr := reapRigWorktreesDir(t, cityPath, reapTestConfig(rigRoot), store, false)

	if len(report.Reaped) != 0 || len(report.Protected) != 0 {
		t.Fatalf("reaper acted on a worktree outside both scan roots: Reaped=%+v Protected=%+v\nstderr:\n%s", report.Reaped, report.Protected, stderr)
	}
	for _, p := range []string{sibling, rigRoot} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("%s was removed or unstattable: %v", p, err)
		}
	}
}
