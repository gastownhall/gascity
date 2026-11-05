package rig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
)

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(path, []byte("original = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	snap, err := SnapshotResolvedFile(fs, path)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if !snap.Exists || string(snap.Data) != "original = true\n" {
		t.Fatalf("snapshot did not capture existing contents: %+v", snap)
	}

	if err := os.WriteFile(path, []byte("mutated = true\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSnapshots(fs, []FileSnapshot{snap}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, _ := os.ReadFile(path)
	if string(got) != "original = true\n" {
		t.Fatalf("restore did not recover original, got %q", got)
	}
}

// bd creates .beads/config.yaml and metadata.json 0600; a rollback must put
// that mode back rather than widen the file to 0644.
func TestSnapshotRestorePreservesFileMode(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("issue-prefix: \"ar\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	snap, err := SnapshotOptionalFile(fs, path)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}

	if err := os.WriteFile(path, []byte("issue_prefix: ar\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSnapshots(fs, []FileSnapshot{snap}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "issue-prefix: \"ar\"\n" {
		t.Errorf("restore did not recover original, got %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("restored mode = %v, want 0600", perm)
	}
}

func TestSnapshotRestoreRemovesFileCreatedAfterSnapshot(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	path := filepath.Join(dir, "new-rig", "routes.jsonl")

	// Snapshot a path that does not exist yet.
	snap, err := SnapshotResolvedFile(fs, path)
	if err != nil {
		t.Fatalf("snapshot missing: %v", err)
	}
	if snap.Exists {
		t.Fatalf("snapshot of a missing file should record Exists=false: %+v", snap)
	}

	// The provisioning step creates it, then fails and rolls back.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSnapshots(fs, []FileSnapshot{snap}); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("restore should have removed the file created after the snapshot, stat err=%v", err)
	}
}

func TestRestoreSnapshotsUsesAtomicWrite(t *testing.T) {
	fs := fsys.NewFake()
	fs.Dirs["/city"] = true
	snap := FileSnapshot{Path: "/city/city.toml", Data: []byte("updated = true\n"), Exists: true}
	if err := RestoreSnapshots(fs, []FileSnapshot{snap}); err != nil {
		t.Fatalf("RestoreSnapshots: %v", err)
	}
	var renamed bool
	for _, call := range fs.Calls {
		if call.Method == "Rename" && strings.HasPrefix(call.Path, snap.Path+".tmp.") {
			renamed = true
			break
		}
	}
	if !renamed {
		t.Fatalf("fs calls = %+v, want atomic rename", fs.Calls)
	}
	if got := string(fs.Files[snap.Path]); got != "updated = true\n" {
		t.Fatalf("restored file = %q", got)
	}
}

func TestRestoreSnapshotsAggregatesAcrossFiles(t *testing.T) {
	fs := fsys.OSFS{}
	dir := t.TempDir()
	a := filepath.Join(dir, "a.toml")
	b := filepath.Join(dir, "b.toml")
	if err := os.WriteFile(a, []byte("a=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, []byte("b=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	snaps := make([]FileSnapshot, 0, 2)
	for _, p := range []string{a, b} {
		s, err := SnapshotResolvedFile(fs, p)
		if err != nil {
			t.Fatal(err)
		}
		snaps = append(snaps, s)
	}
	_ = os.WriteFile(a, []byte("a=2\n"), 0o644)
	_ = os.WriteFile(b, []byte("b=2\n"), 0o644)
	if err := RestoreSnapshots(fs, snaps); err != nil {
		t.Fatalf("restore: %v", err)
	}
	ga, _ := os.ReadFile(a)
	gb, _ := os.ReadFile(b)
	if string(ga) != "a=1\n" || string(gb) != "b=1\n" {
		t.Fatalf("restore did not recover both files: a=%q b=%q", ga, gb)
	}
}

// The rig-add topology rollback restores packs.lock with a temp-file +
// rename, so it must snapshot the resolved symlink target: restoring at the
// link path would replace a symlinked packs.lock with a regular file.
func TestSnapshotTopologyFilesPreservesSymlinkedPacksLock(t *testing.T) {
	fs := fsys.OSFS{}
	cityDir := t.TempDir()
	checkoutDir := t.TempDir()
	target := filepath.Join(checkoutDir, "packs.lock")
	if err := os.WriteFile(target, []byte("schema = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cityDir, "packs.lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	snaps, err := SnapshotTopologyFiles(fs, cityDir, &config.City{})
	if err != nil {
		t.Fatalf("SnapshotTopologyFiles: %v", err)
	}
	if err := os.WriteFile(target, []byte("schema = 1\n\n[packs.\"x\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSnapshots(fs, snaps); err != nil {
		t.Fatalf("RestoreSnapshots: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("Lstat packs.lock link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("packs.lock symlink was replaced by mode %v", info.Mode())
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "schema = 1\n" {
		t.Fatalf("packs.lock target not restored, got %q", got)
	}
}

// statFailingFS fails every Stat of path.
type statFailingFS struct {
	fsys.FS
	path string
}

func (f statFailingFS) Stat(name string) (os.FileInfo, error) {
	if name == f.path {
		return nil, &os.PathError{Op: "stat", Path: name, Err: syscall.EIO}
	}
	return f.FS.Stat(name)
}

// ga-hfly3b.45: a Stat failure after a successful read is returned, not read
// as the 0644 default that would widen a 0600 file on restore.
func TestSnapshotOptionalFileReturnsStatError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("issue_prefix: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := SnapshotOptionalFile(statFailingFS{FS: fsys.OSFS{}, path: path}, path); !errors.Is(err, syscall.EIO) {
		t.Fatalf("SnapshotOptionalFile error = %v, want the Stat failure", err)
	}
}

// ga-hfly3b.45: restoring a file whose bytes and mode did not change leaves it
// in place, so a hard link to it still shares it.
func TestRestoreSnapshotsLeavesUnchangedFileInPlace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("issue_prefix: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "config.yaml.link")
	if err := os.Link(path, link); err != nil {
		t.Fatal(err)
	}
	snap, err := SnapshotOptionalFile(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := RestoreSnapshots(fsys.OSFS{}, []FileSnapshot{snap}); err != nil {
		t.Fatal(err)
	}
	pathInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	linkInfo, err := os.Stat(link)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(pathInfo, linkInfo) {
		t.Errorf("a no-op restore replaced %s; the hard link %s is detached", path, link)
	}
}

// A file whose bytes match but whose mode changed gets its mode back.
func TestRestoreSnapshotsRepairsModeOnlyChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("issue_prefix: x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	snap, err := SnapshotOptionalFile(fsys.OSFS{}, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSnapshots(fsys.OSFS{}, []FileSnapshot{snap}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("mode after restore = %v, want 0600", got)
	}
}

// ga-hfly3b.44: a directory snapshot restores only the directory's mode, and a
// directory that is gone has nothing to restore.
func TestRestoreSnapshotsRestoresDirMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), ".beads")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	snap := dirModeSnapshot(dir, info)
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSnapshots(fsys.OSFS{}, []FileSnapshot{snap}); err != nil {
		t.Fatal(err)
	}
	if info, err = os.Stat(dir); err != nil {
		t.Fatal(err)
	} else if got := info.Mode().Perm(); got != 0o755 {
		t.Errorf("dir mode after restore = %v, want 0755", got)
	}
	if !info.IsDir() {
		t.Fatalf("%s is no longer a directory", dir)
	}

	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := RestoreSnapshots(fsys.OSFS{}, []FileSnapshot{snap}); err != nil {
		t.Fatalf("restoring the mode of a gone directory = %v, want a no-op", err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("restoring a gone directory's mode recreated it (stat err = %v)", err)
	}
}
