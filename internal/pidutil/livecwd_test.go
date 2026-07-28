package pidutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// TestLiveCWDs_PIDCWDsIncludesOwnPID proves LiveCWDs attributes its own cwd
// to its own PID, not just to the deduplicated CWDs set. This is the
// primitive the cwd-collision guard's PID-attributed refusal (ga-9x4z1g.1
// FR3) needs to tell "some live process has this cwd" (CWDs, directory-scoped)
// apart from "this specific known session's PID has this cwd" (PIDCWDs,
// attributable).
func TestLiveCWDs_PIDCWDsIncludesOwnPID(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("LiveCWDs relies on /proc; GOOS=%s has none", runtime.GOOS)
	}
	live := LiveCWDs()
	if !live.Scanned {
		t.Fatal("LiveCWDs().Scanned = false on linux, want true")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	want := filepath.Clean(cwd)
	pid := os.Getpid()
	got, ok := live.PIDCWDs[pid]
	if !ok {
		t.Fatalf("LiveCWDs().PIDCWDs[%d] missing; want this test process's own cwd %q among %d entries", pid, want, len(live.PIDCWDs))
	}
	if filepath.Clean(got) != want {
		t.Fatalf("LiveCWDs().PIDCWDs[%d] = %q, want %q", pid, got, want)
	}
}

// TestLiveCWDs_UnscannedZeroValuePIDCWDsIsFailClosed mirrors
// TestLiveCWDs_UnscannedZeroValueIsFailClosed for the PID-attributed field:
// a caller-injected fail-closed stub must report an empty PIDCWDs, never a
// populated one implying attribution succeeded.
func TestLiveCWDs_UnscannedZeroValuePIDCWDsIsFailClosed(t *testing.T) {
	var zero LiveState
	if len(zero.PIDCWDs) != 0 {
		t.Fatalf("zero-value LiveState.PIDCWDs = %v, want empty", zero.PIDCWDs)
	}
}

// TestLiveCWDs_IncludesOwnCWD proves LiveCWDs (the primitive
// internal/session's cwd-collision guard shares with
// cmd/gc/bead_worktree_liveness.go's collectLiveWorktreeState, per ga-ighomh.1
// acceptance criterion 5 / fd63422bd) actually walks /proc and reports this
// test process's own cwd, mirroring
// cmd/gc's TestCollectLiveWorktreeState_IncludesOwnCWD.
func TestLiveCWDs_IncludesOwnCWD(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skipf("LiveCWDs relies on /proc; GOOS=%s has none", runtime.GOOS)
	}
	live := LiveCWDs()
	if !live.Scanned {
		t.Fatal("LiveCWDs().Scanned = false on linux, want true")
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	want := filepath.Clean(cwd)
	for _, c := range live.CWDs {
		if filepath.Clean(c) == want {
			return
		}
	}
	t.Fatalf("LiveCWDs() did not include this process's cwd %q among %d entries", want, len(live.CWDs))
}

// TestLiveCWDs_UnscannedZeroValueIsFailClosed documents the zero value of
// LiveState as the fail-closed signal a caller-injected scanner stub should
// return to simulate "known-session process/cwd state cannot be enumerated"
// (ga-ighomh.1 acceptance criterion 3). A LiveState with Scanned == false
// must never be treated as an empty-but-successful scan.
func TestLiveCWDs_UnscannedZeroValueIsFailClosed(t *testing.T) {
	var zero LiveState
	if zero.Scanned {
		t.Fatal("zero-value LiveState.Scanned = true, want false so it reads as fail-closed by default")
	}
	if len(zero.CWDs) != 0 {
		t.Fatalf("zero-value LiveState.CWDs = %v, want empty", zero.CWDs)
	}
}

func TestPathAtOrUnder_Equal(t *testing.T) {
	dir := t.TempDir()
	if !PathAtOrUnder(dir, dir) {
		t.Fatal("PathAtOrUnder(dir, dir) = false, want true for identical paths")
	}
}

func TestPathAtOrUnder_Nested(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if !PathAtOrUnder(root, nested) {
		t.Fatal("PathAtOrUnder(root, nested) = false, want true for a nested descendant")
	}
}

func TestPathAtOrUnder_AncestorIsNotUnder(t *testing.T) {
	parent := t.TempDir()
	child := filepath.Join(parent, "child")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatalf("mkdir child: %v", err)
	}
	// The parent of root is not "at or under" root.
	if PathAtOrUnder(child, parent) {
		t.Fatal("PathAtOrUnder(child, parent) = true, want false: an ancestor is not at-or-under its descendant")
	}
}

func TestPathAtOrUnder_SiblingIsNotUnder(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	sibling := filepath.Join(base, "sibling")
	for _, d := range []string{root, sibling} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	if PathAtOrUnder(root, sibling) {
		t.Fatal("PathAtOrUnder(root, sibling) = true, want false for a sibling directory")
	}
}
