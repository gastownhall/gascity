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
