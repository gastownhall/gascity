package packman

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestReadLockfileMissingReturnsEmpty(t *testing.T) {
	lock, err := ReadLockfile(fsys.OSFS{}, t.TempDir())
	if err != nil {
		t.Fatalf("ReadLockfile: %v", err)
	}
	if lock.Schema != LockfileSchema {
		t.Fatalf("Schema = %d, want %d", lock.Schema, LockfileSchema)
	}
	if len(lock.Packs) != 0 {
		t.Fatalf("Packs len = %d, want 0", len(lock.Packs))
	}
}

func TestParseLockfileDecodesPins(t *testing.T) {
	lock, err := ParseLockfile([]byte(`schema = 1

[packs."github.com/alpha/repo"]
version = "1.0.0"
commit = "aaaa"
fetched = "2026-01-02T03:04:05Z"
`))
	if err != nil {
		t.Fatalf("ParseLockfile: %v", err)
	}
	if lock.Schema != LockfileSchema {
		t.Fatalf("Schema = %d, want %d", lock.Schema, LockfileSchema)
	}
	pack, ok := lock.Packs["github.com/alpha/repo"]
	if !ok {
		t.Fatalf("Packs missing github.com/alpha/repo: %#v", lock.Packs)
	}
	if pack.Version != "1.0.0" || pack.Commit != "aaaa" {
		t.Fatalf("pack = %#v", pack)
	}
	if want := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC); !pack.Fetched.Equal(want) {
		t.Fatalf("Fetched = %v, want %v", pack.Fetched, want)
	}
}

func TestParseLockfileEmptyReturnsEmptyLock(t *testing.T) {
	for _, data := range [][]byte{nil, {}} {
		lock, err := ParseLockfile(data)
		if err != nil {
			t.Fatalf("ParseLockfile(%v): %v", data, err)
		}
		if lock.Schema != LockfileSchema {
			t.Fatalf("Schema = %d, want %d", lock.Schema, LockfileSchema)
		}
		if len(lock.Packs) != 0 {
			t.Fatalf("Packs len = %d, want 0", len(lock.Packs))
		}
	}
}

func TestParseLockfileInvalidTOMLErrors(t *testing.T) {
	if _, err := ParseLockfile([]byte("not = [valid")); err == nil {
		t.Fatal("ParseLockfile accepted invalid TOML")
	}
}

func TestWriteLockfileSortsKeys(t *testing.T) {
	dir := t.TempDir()
	lock := &Lockfile{
		Packs: map[string]LockedPack{
			"github.com/zeta/repo":  {Version: "2.0.0", Commit: "bbbb", Fetched: time.Unix(20, 0).UTC()},
			"github.com/alpha/repo": {Version: "1.0.0", Commit: "aaaa", Fetched: time.Unix(10, 0).UTC()},
		},
	}
	if err := WriteLockfile(fsys.OSFS{}, dir, lock); err != nil {
		t.Fatalf("WriteLockfile: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, LockfileName))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(data)
	alpha := strings.Index(text, `[packs."github.com/alpha/repo"]`)
	zeta := strings.Index(text, `[packs."github.com/zeta/repo"]`)
	if alpha == -1 || zeta == -1 || alpha > zeta {
		t.Fatalf("lockfile not sorted:\n%s", text)
	}
}
