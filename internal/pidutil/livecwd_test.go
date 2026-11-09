package pidutil

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// fakeProc builds a procfs-shaped tree: one directory per pid name in cwds,
// holding a "cwd" symlink to the target. The returned root stands in for
// /proc, so every test below runs the same way on every platform.
func fakeProc(t *testing.T, cwds map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, target := range cwds {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
		if target == "" {
			continue // a process whose cwd cannot be read
		}
		if err := os.Symlink(target, filepath.Join(dir, "cwd")); err != nil {
			t.Fatalf("symlink %s: %v", dir, err)
		}
	}
	return root
}

func noFallback(t *testing.T) func() LiveState {
	t.Helper()
	return func() LiveState {
		t.Error("the lsof fallback ran on a host whose procfs exists")
		return LiveState{}
	}
}

// TestLiveCWDsFromWalksProcfs: the walk records each numeric entry's cwd,
// attributes it to its pid, normalizes it, and counts a shared directory once.
func TestLiveCWDsFromWalksProcfs(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	root := fakeProc(t, map[string]string{"100": a, "101": a, "102": b})
	// Entries that are not processes.
	if err := os.WriteFile(filepath.Join(root, "meminfo"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "sys"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := liveCWDsFrom(root, noFallback(t))

	if !got.Scanned {
		t.Fatal("Scanned = false for a readable procfs")
	}
	wantA, wantB := pathutil.NormalizePathForCompare(a), pathutil.NormalizePathForCompare(b)
	if got.PIDCWDs[100] != wantA || got.PIDCWDs[101] != wantA || got.PIDCWDs[102] != wantB {
		t.Fatalf("PIDCWDs = %v, want 100,101 -> %s and 102 -> %s", got.PIDCWDs, wantA, wantB)
	}
	if len(got.PIDCWDs) != 3 {
		t.Fatalf("PIDCWDs = %v, want only the three numeric entries", got.PIDCWDs)
	}
	if len(got.CWDs) != 2 || !slices.Contains(got.CWDs, wantA) || !slices.Contains(got.CWDs, wantB) {
		t.Fatalf("CWDs = %v, want %s and %s once each", got.CWDs, wantA, wantB)
	}
}

// TestLiveCWDsFromSkipsUnreadableAndDeletedCwds: a process that exited
// mid-walk (no cwd link) or whose directory was unlinked (" (deleted)") is
// skipped; neither makes the scan indeterminate.
func TestLiveCWDsFromSkipsUnreadableAndDeletedCwds(t *testing.T) {
	a := t.TempDir()
	root := fakeProc(t, map[string]string{
		"200": a,
		"201": "",
		"202": filepath.Join(a, "gone") + " (deleted)",
	})

	got := liveCWDsFrom(root, noFallback(t))

	if !got.Scanned {
		t.Fatal("Scanned = false: per-process failures are not a scan failure")
	}
	if len(got.PIDCWDs) != 1 || got.PIDCWDs[200] != pathutil.NormalizePathForCompare(a) {
		t.Fatalf("PIDCWDs = %v, want only pid 200", got.PIDCWDs)
	}
}

// TestLiveCWDsFromMissingProcfsUsesTheFallback: a platform with no procfs
// (macOS, a container that does not mount it) is enumerated by the fallback
// and its answer is returned unchanged.
func TestLiveCWDsFromMissingProcfsUsesTheFallback(t *testing.T) {
	want := LiveState{CWDs: []string{"/srv/x"}, PIDCWDs: map[int]string{7: "/srv/x"}, Scanned: true}
	calls := 0

	got := liveCWDsFrom(filepath.Join(t.TempDir(), "no-proc-here"), func() LiveState {
		calls++
		return want
	})

	if calls != 1 {
		t.Fatalf("fallback ran %d times, want exactly once", calls)
	}
	if !got.Scanned || got.PIDCWDs[7] != "/srv/x" || !slices.Equal(got.CWDs, want.CWDs) {
		t.Fatalf("got %+v, want the fallback's %+v", got, want)
	}
}

// TestLiveCWDsFromFallbackFailureStaysUnscanned: no procfs and a fallback that
// cannot enumerate (lsof absent) leaves the scan indeterminate, so the caller
// fails closed. This is the explicit behavior of a host with neither.
func TestLiveCWDsFromFallbackFailureStaysUnscanned(t *testing.T) {
	got := liveCWDsFrom(filepath.Join(t.TempDir(), "no-proc-here"), func() LiveState { return LiveState{} })

	if got.Scanned {
		t.Fatalf("Scanned = true with no procfs and a failed fallback: %+v", got)
	}
}

// TestLiveCWDsFromProcfsScanErrorFailsClosed: procfs exists but cannot be
// listed. That is a real scan failure on a procfs host, not "this platform has
// none", so it is indeterminate and the fallback does not paper over it.
func TestLiveCWDsFromProcfsScanErrorFailsClosed(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "proc")
	if err := os.WriteFile(notADir, []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := liveCWDsFrom(notADir, noFallback(t))

	if got.Scanned {
		t.Fatalf("Scanned = true when procfs could not be listed: %+v", got)
	}
}

// TestLiveCWDsScansThisHost: the production entry point returns a usable scan
// on whatever platform runs the suite: procfs on Linux, lsof elsewhere. The
// test process itself is a live process, so its own directory must be in it.
func TestLiveCWDsScansThisHost(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	got := LiveCWDs()

	if !got.Scanned {
		t.Fatal("LiveCWDs().Scanned = false on a running host")
	}
	if want := pathutil.NormalizePathForCompare(dir); !slices.Contains(got.CWDs, want) {
		t.Fatalf("CWDs does not contain this process's own cwd %s", want)
	}
	if got.PIDCWDs[os.Getpid()] != pathutil.NormalizePathForCompare(dir) {
		t.Fatalf("PIDCWDs[%d] = %q, want this process's cwd", os.Getpid(), got.PIDCWDs[os.Getpid()])
	}
}

// lsofRecord is one `lsof -a -d cwd -F0pn` record.
func lsofRecord(pid int, path string) string {
	return fmt.Sprintf("p%d\x00fcwd\x00n%s\x00\n", pid, path)
}

// TestParseLsofCWDsAttributesPaths: each n field belongs to the p field
// before it, shared directories are listed once, and the result is a usable
// scan.
func TestParseLsofCWDsAttributesPaths(t *testing.T) {
	out := lsofRecord(433, "/srv/a") + lsofRecord(540, "/srv/b") + lsofRecord(541, "/srv/b")

	got := ParseLsofCWDs([]byte(out), nil)

	if !got.Scanned {
		t.Fatal("Scanned = false for a complete listing")
	}
	if got.PIDCWDs[433] != "/srv/a" || got.PIDCWDs[540] != "/srv/b" || got.PIDCWDs[541] != "/srv/b" {
		t.Fatalf("PIDCWDs = %v", got.PIDCWDs)
	}
	if !slices.Equal(got.CWDs, []string{"/srv/a", "/srv/b"}) {
		t.Fatalf("CWDs = %v, want each directory once in first-seen order", got.CWDs)
	}
}

// TestParseLsofCWDsSkipsNonPathRecords: pid and fd fields, blank fields and
// relative paths carry no cwd.
func TestParseLsofCWDsSkipsNonPathRecords(t *testing.T) {
	out := "p1\x00fcwd\x00n/srv/one\x00\nfcwd\x00nrelative/path\x00\n\x00n\x00p2\x00\n"

	got := ParseLsofCWDs([]byte(out), nil)

	if !slices.Equal(got.CWDs, []string{"/srv/one"}) {
		t.Fatalf("CWDs = %v, want only the absolute path", got.CWDs)
	}
}

// TestParseLsofCWDsSkipsErrorAnnotations: lsof reports a per-process failure
// inside the n field as an absolute-looking string; counting it would make an
// unreadable scan look usable.
func TestParseLsofCWDsSkipsErrorAnnotations(t *testing.T) {
	out := lsofRecord(1, "/srv/tree") + lsofRecord(2, "/proc/1/cwd (readlink: Permission denied)")

	got := ParseLsofCWDs([]byte(out), nil)

	if !slices.Equal(got.CWDs, []string{"/srv/tree"}) {
		t.Fatalf("CWDs = %v, want only the readable path", got.CWDs)
	}
	if _, phantom := got.PIDCWDs[2]; phantom {
		t.Fatalf("PIDCWDs = %v attributes the annotation to pid 2", got.PIDCWDs)
	}
}

// TestParseLsofCWDsFailsClosed: no readable record at all (lsof absent, or
// every record an annotation) and a truncated listing are indeterminate; an
// ordinary non-zero exit with records is the same bounded blind spot /proc has
// for other users' processes and still counts.
func TestParseLsofCWDsFailsClosed(t *testing.T) {
	annotations := lsofRecord(1, "/proc/1/cwd (readlink: Permission denied)") + lsofRecord(2, "/proc/2/cwd (readlink: Permission denied)")
	tests := []struct {
		name        string
		out         string
		err         error
		wantScanned bool
	}{
		{name: "empty output", out: "", err: nil, wantScanned: false},
		{name: "lsof missing", out: "", err: errors.New(`exec: "lsof": executable file not found in $PATH`), wantScanned: false},
		{name: "only annotations", out: annotations, err: nil, wantScanned: false},
		{name: "deadline with records", out: lsofRecord(1, "/srv/tree"), err: fmt.Errorf("lsof: %w", context.DeadlineExceeded), wantScanned: false},
		{name: "ordinary non-zero exit with records", out: lsofRecord(1, "/srv/tree"), err: errors.New("exit status 1"), wantScanned: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseLsofCWDs([]byte(tt.out), tt.err); got.Scanned != tt.wantScanned {
				t.Fatalf("Scanned = %v, want %v (%+v)", got.Scanned, tt.wantScanned, got)
			}
		})
	}
}

// TestParseLsofCWDsKeepsPathWithNewline: fields are NUL-terminated so a path
// with a newline in it is not truncated into a different directory.
func TestParseLsofCWDsKeepsPathWithNewline(t *testing.T) {
	got := ParseLsofCWDs([]byte(lsofRecord(1, "/srv/od\nd/tree")), nil)

	if !slices.Equal(got.CWDs, []string{"/srv/od\nd/tree"}) {
		t.Fatalf("CWDs = %q, want the embedded-newline path intact", got.CWDs)
	}
}
