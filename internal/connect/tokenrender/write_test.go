package tokenrender

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	unlock, err := LockOutputs(outputs)
	if err != nil {
		t.Fatal(err)
	}
	acquired := make(chan struct{})
	go func() {
		unlock2, err := LockOutputs([]Output{{Ledger: "infra", TokenFile: filepath.Join(dir, "b")}})
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
	unlock, err := LockOutputs([]Output{
		{Ledger: "b", TokenFile: filepath.Join(dir2, "x")},
		{Ledger: "a", TokenFile: filepath.Join(dir1, "x")},
		{Ledger: "c", TokenFile: filepath.Join(dir1, "y")},
	})
	if err != nil {
		t.Fatalf("LockOutputs over a shared directory: %v", err)
	}
	unlock()
	if _, err := LockOutputs([]Output{{Ledger: "x", TokenFile: filepath.Join(dir1, "missing-dir", "f")}}); err == nil {
		t.Fatal("locking a missing directory succeeded")
	}
}
