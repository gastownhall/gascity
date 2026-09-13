package main

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

// The fallback's parser and its fail-closed rules are exercised through the
// injected enumerator, so every rule below is verified on any platform —
// including Linux CI, where the fallback itself never runs. Only the real lsof
// invocation is platform-specific.

func stubLiveWorktreeCwdEnumerator(t *testing.T, out string, err error) {
	t.Helper()
	prev := liveWorktreeCwdEnumerator
	liveWorktreeCwdEnumerator = func() ([]byte, error) { return []byte(out), err }
	t.Cleanup(func() { liveWorktreeCwdEnumerator = prev })
}

func TestCollectLiveWorktreeStateFallback_ParsesFieldOutput(t *testing.T) {
	stubLiveWorktreeCwdEnumerator(t, "p433\x00fcwd\x00n/srv/city/worktrees/rig/a\x00\np540\x00fcwd\x00n/srv/city/worktrees/rig/b\x00\n", nil)

	got := collectLiveWorktreeStateFallback()

	if !got.scanned {
		t.Fatal("scanned = false, want true: the enumeration succeeded")
	}
	if len(got.cwds) != 2 {
		t.Fatalf("cwds = %v, want 2 entries", got.cwds)
	}
	if got.source != liveScanSourceLsof {
		t.Errorf("source = %q, want %q so the mechanism is observable", got.source, liveScanSourceLsof)
	}
}

func TestCollectLiveWorktreeStateFallback_DeduplicatesSharedCwds(t *testing.T) {
	// Several processes in one worktree is the normal case — an agent plus
	// whatever it spawned — and must count once.
	stubLiveWorktreeCwdEnumerator(t, "p1\x00fcwd\x00n/srv/tree\x00\np2\x00fcwd\x00n/srv/tree\x00\np3\x00fcwd\x00n/srv/tree\x00\n", nil)

	if got := collectLiveWorktreeStateFallback(); len(got.cwds) != 1 {
		t.Fatalf("cwds = %v, want 1 after dedup", got.cwds)
	}
}

func TestCollectLiveWorktreeStateFallback_SkipsNonPathRecords(t *testing.T) {
	// pid and fd records, blank lines, and relative paths carry no cwd.
	stubLiveWorktreeCwdEnumerator(t, "p1\x00fcwd\x00n/srv/one\x00\nfcwd\x00nrelative/path\x00\n\x00n\x00p2\x00\n", nil)

	if got := collectLiveWorktreeStateFallback(); len(got.cwds) != 1 {
		t.Fatalf("cwds = %v, want only the absolute path", got.cwds)
	}
}

// TestCollectLiveWorktreeStateFallback_FailsClosedWhenUnavailable is the safety
// case, and the reason lsof not being installed is harmless: no enumerator means
// no proof any tree is idle, which must protect everything rather than authorize
// a deletion.
func TestCollectLiveWorktreeStateFallback_FailsClosedWhenUnavailable(t *testing.T) {
	stubLiveWorktreeCwdEnumerator(t, "", errors.New(`exec: "lsof": executable file not found in $PATH`))

	got := collectLiveWorktreeStateFallback()

	if got.scanned {
		t.Error("scanned = true with no enumerator available; live worktrees would be treated as idle")
	}
	if got.source != "" {
		t.Errorf("source = %q, want empty for an indeterminate scan", got.source)
	}
}

// TestCollectLiveWorktreeStateFallback_FailsClosedOnEmptyOutput encodes the rule
// that separates this from a naive parse: a running host always has processes
// with a working directory, so an empty listing is a broken enumeration rather
// than an idle machine.
func TestCollectLiveWorktreeStateFallback_FailsClosedOnEmptyOutput(t *testing.T) {
	stubLiveWorktreeCwdEnumerator(t, "", nil)

	if got := collectLiveWorktreeStateFallback(); got.scanned {
		t.Error("scanned = true on empty output; zero process cwds means the scan failed")
	}
}

// TestCollectLiveWorktreeStateFallback_PartialOutputStillCounts mirrors the /proc
// path deliberately. os.Readlink on another user's /proc/<pid>/cwd fails with
// EACCES and that pid is skipped while the scan still reports scanned=true; lsof
// behaves the same way, warning on stderr and listing the rest. Holding the
// fallback to a stricter standard than /proc would just be a different way of
// scanning nothing.
func TestCollectLiveWorktreeStateFallback_PartialOutputStillCounts(t *testing.T) {
	stubLiveWorktreeCwdEnumerator(t, "p1\x00fcwd\x00n/srv/tree\x00\n", errors.New("exit status 1"))

	got := collectLiveWorktreeStateFallback()

	if !got.scanned {
		t.Error("scanned = false for a partial listing; the /proc path treats unreadable processes the same way")
	}
	if len(got.cwds) != 1 {
		t.Errorf("cwds = %v, want the one readable record", got.cwds)
	}
}

// TestCollectLiveWorktreeStateFallback_SkipsLsofErrorAnnotations pins the
// distinction between a path and lsof's way of reporting that it could not read
// one. The error text lands inside the n field and starts with a slash, so it
// passes the absolute-path filter and normalizes non-empty; counted, it would be
// a phantom cwd that matches no worktree.
func TestCollectLiveWorktreeStateFallback_SkipsLsofErrorAnnotations(t *testing.T) {
	stubLiveWorktreeCwdEnumerator(t, "p1\x00fcwd\x00n/srv/tree\x00\np2\x00fcwd\x00n/proc/1/cwd (readlink: Permission denied)\x00\np3\x00fcwd\x00n/proc/2/cwd (readlink: Permission denied)\x00\n", nil)

	got := collectLiveWorktreeStateFallback()

	if len(got.cwds) != 1 {
		t.Fatalf("cwds = %q, want only the readable path", got.cwds)
	}
	if got.cwds[0] != "/srv/tree" {
		t.Errorf("cwds[0] = %q, want %q", got.cwds[0], "/srv/tree")
	}
}

// TestCollectLiveWorktreeStateFallback_FailsClosedWhenEveryRecordIsAnAnnotation
// is the case that makes the previous test matter: on a host where lsof can read
// no process it owns nothing of, every record is an error annotation. Counting
// them would report a usable scan holding no signal, and every live worktree
// would look idle to the reaper.
func TestCollectLiveWorktreeStateFallback_FailsClosedWhenEveryRecordIsAnAnnotation(t *testing.T) {
	stubLiveWorktreeCwdEnumerator(t, "p1\x00fcwd\x00n/proc/1/cwd (readlink: Permission denied)\x00\np2\x00fcwd\x00n/proc/2/cwd (readlink: Permission denied)\x00\n", nil)

	got := collectLiveWorktreeStateFallback()

	if got.scanned {
		t.Error("scanned = true for a listing of nothing but error annotations; the scan read no cwd at all")
	}
	if got.source != "" {
		t.Errorf("source = %q, want empty for an indeterminate scan", got.source)
	}
}

// TestCollectLiveWorktreeStateFallback_FailsClosedOnTimeout separates a deadline
// from the ordinary non-zero exit in _PartialOutputStillCounts. Both hand back
// records plus an error, but truncation at an arbitrary point omits records for
// no reason the listing describes — unlike EACCES, which omits exactly the
// processes this user cannot see, the same blind spot /proc has.
func TestCollectLiveWorktreeStateFallback_FailsClosedOnTimeout(t *testing.T) {
	stubLiveWorktreeCwdEnumerator(t, "p1\x00fcwd\x00n/srv/tree\x00\n", fmt.Errorf("lsof: %w", context.DeadlineExceeded))

	got := collectLiveWorktreeStateFallback()

	if got.scanned {
		t.Error("scanned = true for a listing truncated by the deadline; the missing records are not a bounded blind spot")
	}
	if got.source != "" {
		t.Errorf("source = %q, want empty for an indeterminate scan", got.source)
	}
}

// TestCollectLiveWorktreeState_ScansOnThisHost is the regression test for the
// defect itself: on a real host, with the real mechanism, the scan must come back
// usable and say which mechanism it used. On Linux that exercises /proc; on a
// host without /proc it exercises the fallback. Before this change the latter
// returned scanned=false unconditionally.
func TestCollectLiveWorktreeState_ScansOnThisHost(t *testing.T) {
	got := collectLiveWorktreeStateFn()

	if !got.scanned {
		t.Fatalf("liveness scan unavailable on %s; the reaper protects every candidate indefinitely in this state", runtime.GOOS)
	}
	if len(got.cwds) == 0 {
		t.Errorf("scan reported no process cwds on %s, which cannot be true of a running host", runtime.GOOS)
	}
	if got.source == "" {
		t.Error("source is empty on a successful scan; the mechanism must be recorded")
	}
}

// TestCollectLiveWorktreeStateFallback_PathWithNewlineSurvives is why the
// enumerator asks for NUL-terminated fields. With newline-delimited output this
// path would be truncated at the newline, and a truncated cwd no longer matches
// the worktree it is inside — under-protection in a path that deletes
// directories. Such a path is pathological, and the parser should not be the
// reason it turns into data loss.
func TestCollectLiveWorktreeStateFallback_PathWithNewlineSurvives(t *testing.T) {
	stubLiveWorktreeCwdEnumerator(t, "p1\x00fcwd\x00n/srv/od\nd/tree\x00\n", nil)

	got := collectLiveWorktreeStateFallback()

	if len(got.cwds) != 1 {
		t.Fatalf("cwds = %q, want the single embedded-newline path intact", got.cwds)
	}
	if !strings.Contains(got.cwds[0], "\n") {
		t.Errorf("cwds[0] = %q, want the newline preserved rather than truncated", got.cwds[0])
	}
}
