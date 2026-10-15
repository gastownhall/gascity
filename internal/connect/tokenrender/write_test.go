package tokenrender

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/gastownhall/gascity/internal/connect"
)

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return data
}

func statFile(t *testing.T, path string) os.FileInfo {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return st
}

func TestWriteTokenFileCreatesWith0600(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	changed, err := WriteTokenFile(path, []byte("a\n"))
	if err != nil || !changed {
		t.Fatalf("WriteTokenFile = %v, %v", changed, err)
	}
	if got := readFile(t, path); string(got) != "a\n" {
		t.Fatalf("content = %q", got)
	}
	if mode := statFile(t, path).Mode().Perm(); mode != 0o600 {
		t.Fatalf("new file mode = %o, want 600", mode)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("directory holds %d entries, want only the token file (no temp or lock file)", len(entries))
	}
}

func TestWriteTokenFileKeepsMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTokenFile(path, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	if mode := statFile(t, path).Mode().Perm(); mode != 0o640 {
		t.Fatalf("mode = %o, want 640 kept", mode)
	}
}

func TestWriteTokenFileEqualBytesWriteNothing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("same\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	changed, err := WriteTokenFile(path, []byte("same\n"))
	if err != nil || changed {
		t.Fatalf("WriteTokenFile = %v, %v; want unchanged", changed, err)
	}
	if mt := statFile(t, path).ModTime(); !mt.Equal(old) {
		t.Fatalf("mtime moved to %v for equal bytes", mt)
	}
}

// TestWriteTokenFileSameSizeRotationIsSeen simulates the pinned upstream bd
// serve, which skips a reload when (mtime, size) is unchanged (DES-CN F5): a
// same-size rotation inside one timestamp granule must still move the mtime.
func TestWriteTokenFileSameSizeRotationIsSeen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	frozen := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	now := func() time.Time { return frozen }
	type sig struct {
		mtime time.Time
		size  int64
	}
	signature := func() sig {
		st := statFile(t, path)
		return sig{st.ModTime(), st.Size()}
	}
	if _, err := writeTokenFile(path, []byte("tok-aaaa\n"), now); err != nil {
		t.Fatal(err)
	}
	before := signature()
	if !before.mtime.Equal(frozen) {
		t.Fatalf("new file mtime = %v, want the clock's %v", before.mtime, frozen)
	}
	if changed, err := writeTokenFile(path, []byte("tok-bbbb\n"), now); err != nil || !changed {
		t.Fatalf("rotation = %v, %v", changed, err)
	}
	after := signature()
	if after.size != before.size {
		t.Fatalf("sizes differ (%d, %d); the case needs a same-size rotation", before.size, after.size)
	}
	if !after.mtime.After(before.mtime) {
		t.Fatalf("mtime did not advance (%v then %v): an (mtime, size) reload check misses the rotation", before.mtime, after.mtime)
	}
	if want := frozen.Add(time.Microsecond); !after.mtime.Equal(want) {
		t.Fatalf("rotated mtime = %v, want previous + 1µs = %v", after.mtime, want)
	}

	// A clock behind the file's mtime still advances it.
	past := func() time.Time { return frozen.Add(-time.Hour) }
	before = signature()
	if _, err := writeTokenFile(path, []byte("tok-cccc\n"), past); err != nil {
		t.Fatal(err)
	}
	if after := signature(); !after.mtime.After(before.mtime) {
		t.Fatalf("mtime went from %v to %v under a clock behind it", before.mtime, after.mtime)
	}
}

func TestWriteTokenFileRefusals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tokens")
	cases := []struct {
		name    string
		content []byte
	}{
		{"empty", nil},
		{"unterminated", []byte("a")},
		{"oversized", append(bytes.Repeat([]byte("a"), connect.MaxTokenFileBytes), '\n')},
	}
	for _, tc := range cases {
		if _, err := WriteTokenFile(path, tc.content); err == nil {
			t.Errorf("%s content accepted", tc.name)
		}
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a refused write created the file: %v", err)
	}

	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("t\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTokenFile(link, []byte("x\n")); err == nil {
		t.Fatal("a symlinked token file was replaced")
	}
	sub := filepath.Join(dir, "sub")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTokenFile(sub, []byte("x\n")); err == nil {
		t.Fatal("a directory was replaced")
	}
}

// TestWriteTokenFileIsAtomic checks that a concurrent reader only ever sees a
// whole old file or a whole new one.
func TestWriteTokenFileIsAtomic(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	a := []byte(strings.Repeat("alpha-token\n", 4000))
	b := []byte(strings.Repeat("beta\n", 2000))
	if _, err := WriteTokenFile(path, a); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	var torn atomic.Int64
	var reads atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			data, err := os.ReadFile(path)
			if err != nil || (!bytes.Equal(data, a) && !bytes.Equal(data, b)) {
				torn.Add(1)
			}
			reads.Add(1)
		}
	}()
	for i := range 200 {
		content := a
		if i%2 == 0 {
			content = b
		}
		if _, err := WriteTokenFile(path, content); err != nil {
			t.Fatal(err)
		}
	}
	stop.Store(true)
	wg.Wait()
	if n := torn.Load(); n != 0 {
		t.Fatalf("%d of %d reads saw a partial or missing file", n, reads.Load())
	}
}

// TestLockOutputsSerializesInProcess takes the second lock through its own
// open file description, which flock treats exactly as it treats another
// process's: the two serialize in the kernel, not through any in-process
// state.
func TestLockOutputsSerializesInProcess(t *testing.T) {
	dir := t.TempDir()
	outputs := []Output{{Ledger: "work", TokenFile: filepath.Join(dir, "a")}}
	unlock, err := LockOutputs(context.Background(), outputs)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan struct{})
	go func() {
		unlock2, err := LockOutputs(context.Background(), []Output{{Ledger: "infra", TokenFile: filepath.Join(dir, "b")}})
		if err != nil {
			t.Error(err)
			close(acquired)
			return
		}
		close(acquired)
		unlock2()
	}()
	select {
	case <-acquired:
		t.Fatal("a second lock on the same directory did not wait")
	case <-time.After(150 * time.Millisecond):
	}
	unlock()
	unlock() // idempotent
	select {
	case <-acquired:
	case <-time.After(10 * time.Second):
		t.Fatal("the second lock never acquired after unlock")
	}
}

func TestLockOutputsLocksEachDirectoryOnce(t *testing.T) {
	dir1, dir2 := t.TempDir(), t.TempDir()
	unlock, err := LockOutputs(context.Background(), []Output{
		{Ledger: "b", TokenFile: filepath.Join(dir2, "x")},
		{Ledger: "a", TokenFile: filepath.Join(dir1, "x")},
		{Ledger: "c", TokenFile: filepath.Join(dir1, "y")},
	})
	if err != nil {
		t.Fatalf("LockOutputs over a shared directory: %v", err)
	}
	unlock()
	if _, err := LockOutputs(context.Background(), []Output{{Ledger: "x", TokenFile: filepath.Join(dir1, "missing-dir", "f")}}); err == nil {
		t.Fatal("locking a missing directory succeeded")
	}
}

// holdDirLock takes an exclusive flock on dir through its own open file
// description, as another process would, and returns its release.
func holdDirLock(t *testing.T, dir string) func() {
	t.Helper()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close() //nolint:errcheck // test cleanup
		t.Fatalf("holding %s: %v", dir, err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			unix.Flock(int(f.Fd()), unix.LOCK_UN) //nolint:errcheck // close releases it anyway
			f.Close()                             //nolint:errcheck // test cleanup
		})
	}
	t.Cleanup(release)
	return release
}

// dirLocked reports whether another open file description holds dir's flock.
func dirLocked(t *testing.T, dir string) bool {
	t.Helper()
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close() //nolint:errcheck // test probe
	err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if err == nil {
		unix.Flock(int(f.Fd()), unix.LOCK_UN) //nolint:errcheck // probe only
		return false
	}
	if !errors.Is(err, unix.EWOULDBLOCK) {
		t.Fatalf("probing %s: %v", dir, err)
	}
	return true
}

func TestLockOutputsHonorsContext(t *testing.T) {
	dir := t.TempDir()
	holdDirLock(t, dir)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := LockOutputs(ctx, []Output{{Ledger: "work", TokenFile: filepath.Join(dir, "a")}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("LockOutputs = %v, want the context's deadline", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("LockOutputs returned %s after its deadline", elapsed)
	}
}

// TestLockOutputsDedupesOneDirectoryReachedTwoWays pins that a directory
// reached through a symlinked alias is locked once: two flocks on one inode
// through separate open file descriptions would block the caller on itself.
func TestLockOutputsDedupesOneDirectoryReachedTwoWays(t *testing.T) {
	realDir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	unlock, err := LockOutputs(ctx, []Output{
		{Ledger: "work", TokenFile: filepath.Join(realDir, "a")},
		{Ledger: "infra", TokenFile: filepath.Join(alias, "b")},
	})
	if err != nil {
		t.Fatalf("LockOutputs over one directory reached two ways: %v", err)
	}
	if !dirLocked(t, realDir) {
		t.Fatal("the directory is not locked")
	}
	unlock()
	if dirLocked(t, realDir) {
		t.Fatal("unlock left the directory locked")
	}
}

// TestWriteTokenFileKeepsGroup pins that a replacement keeps the file's group
// as well as its mode: a bd serve reading a 0640 file through its group must
// keep reading it after a render.
func TestWriteTokenFileKeepsGroup(t *testing.T) {
	groups, err := os.Getgroups()
	if err != nil {
		t.Fatal(err)
	}
	other := -1
	for _, g := range groups {
		if g != os.Getegid() {
			other = g
			break
		}
	}
	if other < 0 && os.Geteuid() == 0 {
		other = 4243 // root may give a file any group
	}
	if other < 0 {
		t.Skip("the test user has no secondary group and is not root")
	}
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, -1, other); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTokenFile(path, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	st := statFile(t, path)
	if gid := int(st.Sys().(*syscall.Stat_t).Gid); gid != other {
		t.Fatalf("group = %d, want %d kept", gid, other)
	}
	if mode := st.Mode().Perm(); mode != 0o640 {
		t.Fatalf("mode = %o, want 640 kept", mode)
	}
}

func TestWriteTokenFileRefusesAFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := WriteTokenFile(path, []byte("x\n"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO token file was replaced")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("WriteTokenFile blocked on a FIFO")
	}
}

// TestWriteTokenFileAcceptsExactlyTheCap pins the cap's boundary: bd serve
// reads a file of exactly MaxTokenFileBytes.
func TestWriteTokenFileAcceptsExactlyTheCap(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	content := append(bytes.Repeat([]byte("a"), connect.MaxTokenFileBytes-1), '\n')
	if _, err := WriteTokenFile(path, content); err != nil {
		t.Fatalf("content of exactly the cap was refused: %v", err)
	}
	if got := readFile(t, path); !bytes.Equal(got, content) {
		t.Fatal("the file does not hold the content")
	}
}

// TestLockOutputsChecksTheContextFirst pins that an ended context takes no
// lock, even a free one.
func TestLockOutputsChecksTheContextFirst(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	unlock, err := LockOutputs(ctx, []Output{{Ledger: "work", TokenFile: filepath.Join(dir, "a")}})
	if err == nil {
		unlock()
		t.Fatal("LockOutputs took a lock under an ended context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("LockOutputs = %v, want the context's cancellation", err)
	}
}

// TestWriteTokenFileRefusesAnotherOwnersFile pins that a renderer that cannot
// keep a file's owner leaves the file alone: replacing it would hand bd
// serve a file owned by the renderer's user, which it may not be able to
// read.
func TestWriteTokenFileRefusesAnotherOwnersFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner := os.Geteuid()
	if owner == 0 {
		t.Skip("root keeps any owner; TestWriteTokenFileKeepsOwnerAsRoot covers it")
	}
	setGeteuid(t, func() int { return owner + 1 })
	if _, err := WriteTokenFile(path, []byte("new\n")); !errors.Is(err, errForeignOwner) {
		t.Fatalf("WriteTokenFile = %v, want a refusal to change the owner", err)
	}
	if got := readFile(t, path); !bytes.Equal(got, []byte("old\n")) {
		t.Fatalf("another owner's file was replaced: %q", got)
	}
}

// TestWriteTokenFileKeepsOwnerAsRoot pins that a renderer running as root
// (a sidecar's default) keeps the owner of the file it replaces.
func TestWriteTokenFileKeepsOwnerAsRoot(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root to own a file as another user")
	}
	const uid, gid = 4242, 4243
	path := filepath.Join(t.TempDir(), "tokens")
	if err := os.WriteFile(path, []byte("old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chown(path, uid, gid); err != nil {
		t.Fatal(err)
	}
	if _, err := WriteTokenFile(path, []byte("new\n")); err != nil {
		t.Fatal(err)
	}
	st := statFile(t, path).Sys().(*syscall.Stat_t)
	if int(st.Uid) != uid || int(st.Gid) != gid {
		t.Fatalf("owner = %d:%d, want %d:%d kept", st.Uid, st.Gid, uid, gid)
	}
}

// setGeteuid replaces the effective uid writeTokenFile sees for one test.
func setGeteuid(t *testing.T, f func() int) {
	t.Helper()
	prev := geteuid
	geteuid = f
	t.Cleanup(func() { geteuid = prev })
}

// TestTempFileErrNamesNoTempFile pins that an error on a temp file keeps its
// operation and cause but not the file's random name, for both error shapes
// the temp file's calls return.
func TestTempFileErrNamesNoTempFile(t *testing.T) {
	const tmp = "/run/gc/.serve-tokens.tmp-123456"
	cases := []struct {
		err   error
		want  string
		cause error
	}{
		{&os.PathError{Op: "open", Path: tmp, Err: syscall.EACCES}, "open temp file: " + syscall.EACCES.Error(), syscall.EACCES},
		{&os.PathError{Op: "write", Path: tmp, Err: syscall.ENOSPC}, "write temp file: " + syscall.ENOSPC.Error(), syscall.ENOSPC},
		{&os.LinkError{Op: "rename", Old: tmp, New: "/run/gc/serve-tokens", Err: syscall.EROFS}, "rename temp file: " + syscall.EROFS.Error(), syscall.EROFS},
	}
	for _, tc := range cases {
		got := tempFileErr(tc.err)
		if got.Error() != tc.want || !errors.Is(got, tc.cause) {
			t.Errorf("tempFileErr(%v) = %q, want %q wrapping %v", tc.err, got, tc.want, tc.cause)
		}
	}
	plain := errors.New("no file named")
	if got := tempFileErr(plain); !errors.Is(got, plain) || got.Error() != plain.Error() {
		t.Errorf("tempFileErr changed an error that names no file: %v", got)
	}
}
