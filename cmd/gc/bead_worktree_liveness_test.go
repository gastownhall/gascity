package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/pathutil"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

func TestWorktreeIsLive_ProcessCWDEqualsWorktree(t *testing.T) {
	wt := t.TempDir()
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(wt)}}
	got, reason := worktreeIsLive(wt, live, nil)
	if !got {
		t.Fatalf("worktreeIsLive = false, want true when a process cwd equals the worktree (reason %q)", reason)
	}
}

func TestWorktreeIsLive_ProcessCWDUnderWorktree(t *testing.T) {
	wt := t.TempDir()
	nested := filepath.Join(wt, "test", "integration")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(nested)}}
	got, reason := worktreeIsLive(wt, live, nil)
	if !got {
		t.Fatalf("worktreeIsLive = false, want true when a process cwd is a nested subdir (reason %q)", reason)
	}
}

func TestWorktreeIsLive_ProcessCWDAboveWorktreeIsNotLive(t *testing.T) {
	parent := t.TempDir()
	wt := filepath.Join(parent, "wt")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatalf("mkdir wt: %v", err)
	}
	// A process sitting in the PARENT of the worktree is not "working in" it.
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(parent)}}
	if got, _ := worktreeIsLive(wt, live, nil); got {
		t.Fatal("worktreeIsLive = true, want false when the only live cwd is an ancestor of the worktree")
	}
}

func TestWorktreeIsLive_SiblingCWDIsNotLive(t *testing.T) {
	base := t.TempDir()
	wt := filepath.Join(base, "wt")
	sibling := filepath.Join(base, "other")
	for _, d := range []string{wt, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(sibling)}}
	if got, _ := worktreeIsLive(wt, live, nil); got {
		t.Fatal("worktreeIsLive = true, want false for a sibling directory cwd")
	}
}

func TestWorktreeIsLive_SessionDirProtects(t *testing.T) {
	wt := t.TempDir()
	live := liveWorktreeState{scanned: true} // no live process cwds
	got, reason := worktreeIsLive(wt, live, []string{wt})
	if !got {
		t.Fatalf("worktreeIsLive = false, want true when an active session dir equals the worktree (reason %q)", reason)
	}
}

func TestWorktreeIsLive_NothingMatches(t *testing.T) {
	wt := t.TempDir()
	other := t.TempDir()
	live := liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(other)}}
	if got, _ := worktreeIsLive(wt, live, []string{other}); got {
		t.Fatal("worktreeIsLive = true, want false when no live signal is at or under the worktree")
	}
}

// TestCollectLiveWorktreeState_IncludesOwnCWD no longer skips off Linux. The
// skip described the /proc-only limitation instead of asserting against it,
// which let the gate stay permanently indeterminate on other platforms with a
// green suite. The portable fallback makes the assertion meaningful on both.
func TestCollectLiveWorktreeState_IncludesOwnCWD(t *testing.T) {
	live := collectLiveWorktreeState()
	if !live.scanned {
		t.Fatalf("collectLiveWorktreeState scanned = false on %s, want true", runtime.GOOS)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	want := pathutil.NormalizePathForCompare(cwd)
	for _, c := range live.cwds {
		if c == want {
			return // found this process's own cwd in the live set
		}
	}
	t.Fatalf("collectLiveWorktreeState did not include this process's cwd %q in %d entries", want, len(live.cwds))
}

func TestLiveSessionWorktreeDirs_CollectsAndDedups(t *testing.T) {
	abs1 := t.TempDir()
	abs2 := t.TempDir()
	snapshot := newSessionBeadSnapshotFromInfos([]sessionpkg.Info{
		{ID: "s1", WorkerDir: abs1},
		{ID: "s2", WorkDir: abs2},
		{ID: "s3", WorkerDir: abs1},          // duplicate of s1 → deduped
		{ID: "s4", WorkDir: "relative/path"}, // non-absolute → dropped
		{ID: "s5"},                           // empty → dropped
	})
	got := liveSessionWorktreeDirs(snapshot)

	want := map[string]bool{
		pathutil.NormalizePathForCompare(abs1): false,
		pathutil.NormalizePathForCompare(abs2): false,
	}
	for _, d := range got {
		nd := pathutil.NormalizePathForCompare(d)
		if _, ok := want[nd]; !ok {
			t.Errorf("unexpected dir %q (normalized %q)", d, nd)
			continue
		}
		want[nd] = true
	}
	for d, seen := range want {
		if !seen {
			t.Errorf("expected dir %q missing from result %v", d, got)
		}
	}
}

func TestLiveSessionWorktreeDirs_NilSnapshot(t *testing.T) {
	if got := liveSessionWorktreeDirs(nil); got != nil {
		t.Fatalf("liveSessionWorktreeDirs(nil) = %v, want nil", got)
	}
}

// TestLiveExternalWorkDirSet_IncludesLiveExcludesNotLive is focused coverage
// for ga-1xaqgo.3 cause C: liveExternalWorkDirSet must report a worktree with
// a live process cwd, and must not report a sibling worktree with none, so
// reconciler capacity accounting only treats genuinely active external work
// as occupying a slot.
func TestLiveExternalWorkDirSet_IncludesLiveExcludesNotLive(t *testing.T) {
	_, rigRoot := initReapRig(t)
	live := filepath.Join(t.TempDir(), ".claude", "worktrees", "live-session")
	idle := filepath.Join(t.TempDir(), ".claude", "worktrees", "idle-session")
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		t.Fatalf("mkdir live worktree parent: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(idle), 0o755); err != nil {
		t.Fatalf("mkdir idle worktree parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "live-branch", live)
	mustGit(t, rigRoot, "worktree", "add", "-b", "idle-branch", idle)

	injectLiveness(t, liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(live)}})
	cfg := reapTestConfig(rigRoot)

	got := liveExternalWorkDirSet(cfg, nil, io.Discard)

	if !got[pathutil.NormalizePathForCompare(live)] {
		t.Fatalf("liveExternalWorkDirSet omitted the live worktree %s; got %v", live, got)
	}
	if got[pathutil.NormalizePathForCompare(idle)] {
		t.Fatalf("liveExternalWorkDirSet wrongly included the idle worktree %s; got %v", idle, got)
	}
}

// TestLiveExternalWorkDirSet_FailsClosedWhenScanIndeterminate pins the
// fail-closed contract documented on liveExternalWorkDirSet: when the
// process-table scan itself is indeterminate (scanned=false), every worktree
// in scope must be treated as live. An unobservable worktree must not read as
// free capacity -- that would admit the very duplicate spawn ga-1xaqgo.3
// exists to prevent.
func TestLiveExternalWorkDirSet_FailsClosedWhenScanIndeterminate(t *testing.T) {
	_, rigRoot := initReapRig(t)
	foreign := filepath.Join(t.TempDir(), ".claude", "worktrees", "some-session")
	if err := os.MkdirAll(filepath.Dir(foreign), 0o755); err != nil {
		t.Fatalf("mkdir foreign worktree parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "foreign-branch", foreign)

	injectLiveness(t, liveWorktreeState{scanned: false})
	cfg := reapTestConfig(rigRoot)

	got := liveExternalWorkDirSet(cfg, nil, io.Discard)

	if !got[pathutil.NormalizePathForCompare(foreign)] {
		t.Fatalf("liveExternalWorkDirSet must fail closed (treat every in-scope worktree as live) when the scan is indeterminate; got %v for %s", got, foreign)
	}
}

// TestLiveExternalWorkDirSet_SkipsRigOnScanErrorContinuesOthers proves a
// per-rig discovery error (e.g. a misconfigured rig path) is logged and
// skipped rather than aborting the whole pass, matching
// reapClosedBeadWorktrees's own posture -- one broken rig must not blind
// capacity accounting to every other rig's live external work.
func TestLiveExternalWorkDirSet_SkipsRigOnScanErrorContinuesOthers(t *testing.T) {
	_, rigRoot := initReapRig(t)
	live := filepath.Join(t.TempDir(), ".claude", "worktrees", "live-session")
	if err := os.MkdirAll(filepath.Dir(live), 0o755); err != nil {
		t.Fatalf("mkdir live worktree parent: %v", err)
	}
	mustGit(t, rigRoot, "worktree", "add", "-b", "live-branch", live)

	injectLiveness(t, liveWorktreeState{scanned: true, cwds: []string{pathutil.NormalizePathForCompare(live)}})
	notARepo := t.TempDir()
	cfg := &config.City{
		Rigs: []config.Rig{
			{Name: "broken", Path: notARepo},
			{Name: reapTestRigName, Path: rigRoot},
		},
	}

	got := liveExternalWorkDirSet(cfg, nil, io.Discard)

	if !got[pathutil.NormalizePathForCompare(live)] {
		t.Fatalf("liveExternalWorkDirSet should still report the healthy rig's live worktree despite the other rig's scan error; got %v", got)
	}
}
