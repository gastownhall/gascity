package livefile

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// TestStatPinsIdentityAtSnapshot covers the rotation shape: a file is renamed
// away and a fresh one takes its path. The earlier snapshot must keep
// describing the file it saw, so os.SameFile tells the two apart.
func TestStatPinsIdentityAtSnapshot(t *testing.T) {
	for _, tc := range []struct {
		name   string
		create func(path string) error
	}{
		{name: "regular file", create: func(path string) error { return os.WriteFile(path, []byte("a\n"), 0o644) }},
		{name: "directory", create: func(path string) error { return os.Mkdir(path, 0o755) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "active")
			if err := tc.create(path); err != nil {
				t.Fatalf("create: %v", err)
			}
			before, err := Stat(path)
			if err != nil {
				t.Fatalf("Stat before replace: %v", err)
			}

			moved := path + ".rotating"
			if err := os.Rename(path, moved); err != nil {
				t.Fatalf("rename: %v", err)
			}
			if err := tc.create(path); err != nil {
				t.Fatalf("recreate: %v", err)
			}

			after, err := Stat(path)
			if err != nil {
				t.Fatalf("Stat after replace: %v", err)
			}
			if os.SameFile(before, after) {
				t.Fatal("snapshot taken before the replace matches the new file at the path")
			}
			archived, err := Stat(moved)
			if err != nil {
				t.Fatalf("Stat moved file: %v", err)
			}
			if !os.SameFile(before, archived) {
				t.Fatal("snapshot lost the identity of the file it described")
			}
		})
	}
}

// TestStatUnixSocket covers endpoints that are re-bound at the same path. On
// Windows an AF_UNIX socket is a reparse point that cannot be opened as a
// file, so Stat must still answer for it, with its socket mode and an identity
// that tells a re-bound socket from the original.
//
// The first socket is renamed aside rather than removed: a removed socket
// frees its inode, and ext4 hands that inode straight to the re-bound socket,
// so the two would correctly compare as the same dev/inode. Binding without
// listening is enough to put a socket file on disk.
func TestStatUnixSocket(t *testing.T) {
	path := filepath.Join(shortTempDir(t), "s")
	closeFirst := bindUnixSocket(t, path)

	before, err := Stat(path)
	if err != nil {
		t.Fatalf("Stat socket: %v", err)
	}
	if before.Mode().Type() != fs.ModeSocket {
		t.Fatalf("socket mode = %v, want %v", before.Mode().Type(), fs.ModeSocket)
	}
	closeFirst()
	moved := path + ".old"
	if err := os.Rename(path, moved); err != nil {
		t.Fatalf("move first socket aside: %v", err)
	}
	bindUnixSocket(t, path)

	after, err := Stat(path)
	if err != nil {
		t.Fatalf("Stat re-bound socket: %v", err)
	}
	if os.SameFile(before, after) {
		t.Fatal("snapshot of the first socket matches the re-bound socket")
	}
	original, err := Stat(moved)
	if err != nil {
		t.Fatalf("Stat moved socket: %v", err)
	}
	if !os.SameFile(before, original) {
		t.Fatal("snapshot lost the identity of the socket it described")
	}
}

func TestStatMissingIsNotExist(t *testing.T) {
	_, err := Stat(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Stat missing path error = %v, want fs.ErrNotExist", err)
	}
}

// TestOpenAllowsRenameAndRemove covers a reader holding a log that its writer
// rotates: the rename and the later removal must both succeed while the reader
// is open, and the reader keeps reading the file it opened.
func TestOpenAllowsRenameAndRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "events.jsonl")
	if err := os.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer f.Close() //nolint:errcheck // test cleanup

	rotating := path + ".rotating"
	if err := os.Rename(path, rotating); err != nil {
		t.Fatalf("rename while open: %v", err)
	}
	if err := os.WriteFile(path, []byte("two\n"), 0o644); err != nil {
		t.Fatalf("recreate active path while open: %v", err)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read after rename: %v", err)
	}
	if string(data) != "one\n" {
		t.Fatalf("read %q after rename, want the opened file's %q", data, "one\n")
	}
	if err := os.Remove(rotating); err != nil {
		t.Fatalf("remove while open: %v", err)
	}
	if _, err := os.Lstat(rotating); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed file still visible: %v", err)
	}
}

func TestOpenMissingIsNotExist(t *testing.T) {
	_, err := Open(filepath.Join(t.TempDir(), "missing"))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open missing path error = %v, want fs.ErrNotExist", err)
	}
}

// TestOpenFileAppendAllowsRenameAndRemove covers a second writer holding an
// append handle on a log that its owner rotates: the rename and the later
// removal must both succeed while the writer is open, and the writer's handle
// keeps referring to the renamed file.
func TestOpenFileAppendAllowsRenameAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	w, err := OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("OpenFile: %v", err)
	}
	defer w.Close() //nolint:errcheck // test cleanup
	if _, err := w.Write([]byte("one\n")); err != nil {
		t.Fatalf("write before rename: %v", err)
	}

	rotating := path + ".rotating"
	if err := os.Rename(path, rotating); err != nil {
		t.Fatalf("rename while a writer is open: %v", err)
	}
	if _, err := w.Write([]byte("two\n")); err != nil {
		t.Fatalf("write after rename: %v", err)
	}
	data, err := os.ReadFile(rotating)
	if err != nil {
		t.Fatalf("read renamed file: %v", err)
	}
	if string(data) != "one\ntwo\n" {
		t.Fatalf("renamed file holds %q, want both writes %q", data, "one\ntwo\n")
	}
	if err := os.Remove(rotating); err != nil {
		t.Fatalf("remove while a writer is open: %v", err)
	}
	if _, err := os.Lstat(rotating); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("removed file still visible: %v", err)
	}
}

// TestOpenFileAppendsFromEveryHandle checks O_APPEND keeps its meaning: two
// handles writing in turn each append, and neither overwrites the other.
func TestOpenFileAppendsFromEveryHandle(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(path, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	var handles [2]*os.File
	for i := range handles {
		f, err := OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			t.Fatalf("OpenFile %d: %v", i, err)
		}
		defer f.Close() //nolint:errcheck // test cleanup
		handles[i] = f
	}
	for _, line := range []string{"a1\n", "b1\n", "a2\n", "b2\n"} {
		f := handles[0]
		if line[0] == 'b' {
			f = handles[1]
		}
		if _, err := f.Write([]byte(line)); err != nil {
			t.Fatalf("write %q: %v", line, err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := "0\na1\nb1\na2\nb2\n"; string(data) != want {
		t.Fatalf("file = %q, want %q", data, want)
	}
}

func TestOpenFileReadWriteMissingIsNotExist(t *testing.T) {
	_, err := OpenFile(filepath.Join(t.TempDir(), "missing"), os.O_RDWR, 0)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("OpenFile missing path error = %v, want fs.ErrNotExist", err)
	}
}

func TestOpenFileRejectsUnsupportedFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if f, err := OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		f.Close() //nolint:errcheck // test cleanup
		t.Fatal("OpenFile accepted O_TRUNC")
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != "keep\n" {
		t.Fatalf("file after rejected open = %q, %v; want it untouched", data, err)
	}
}

// shortTempDir keeps socket paths inside the AF_UNIX pathname limit, which
// t.TempDir's test-name-derived directories can exceed.
func shortTempDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "lf")
	if err != nil {
		t.Fatalf("creating a short temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) }) //nolint:errcheck // test cleanup
	return dir
}
