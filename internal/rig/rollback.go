package rig

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/fsys"
)

// FileSnapshot captures a file's contents and permission bits (or its absence)
// so a failed multi-file provisioning step can be rolled back atomically. It is
// the unit of the rig-add / rig set-endpoint topology rollback.
type FileSnapshot struct {
	Path   string
	Data   []byte
	Exists bool
	// Mode is the file's permission bits when it existed. bd creates
	// .beads/config.yaml and metadata.json 0600, so a restore must put that
	// mode back rather than widen the file to the 0644 default. Zero means the
	// default.
	Mode os.FileMode
	// Dir marks the snapshot of a directory's mode (dirModeSnapshot): Data is
	// unused, and a restore only puts Mode back on the directory, never
	// creating, emptying or removing it.
	Dir bool
}

// SnapshotResolvedFile snapshots path for rollback through any symlink chain:
// restoring at the link path would replace the link with a regular file (the
// ga-lurp5d failure mode), so the snapshot records the resolved target and the
// restore writes there instead. Resolve-only by design — a rollback writes the
// original bytes back, so the key-loss rewrite guard does not apply. A path
// blocked by a regular-file intermediate cannot exist; it snapshots as missing,
// matching SnapshotOptionalFile.
func SnapshotResolvedFile(fs fsys.FS, path string) (FileSnapshot, error) {
	resolved, err := fsys.ResolveSymlinks(fs, path)
	if err != nil {
		if errors.Is(err, syscall.ENOTDIR) {
			return FileSnapshot{Path: path}, nil
		}
		return FileSnapshot{}, err
	}
	return SnapshotOptionalFile(fs, resolved)
}

// SnapshotOptionalFile snapshots path, recording it as missing when it does not
// exist (or an intermediate is a regular file). The returned snapshot copies the
// file bytes so a later mutation cannot alias the captured data.
func SnapshotOptionalFile(fs fsys.FS, path string) (FileSnapshot, error) {
	data, err := fs.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR) {
			return FileSnapshot{Path: path}, nil
		}
		return FileSnapshot{}, err
	}
	// The mode comes from a second call. Its failure is returned rather than
	// read as the 0644 default, which would widen a 0600 file on restore.
	info, err := fs.Stat(path)
	if err != nil {
		return FileSnapshot{}, err
	}
	cp := append([]byte(nil), data...)
	snap := FileSnapshot{Path: path, Data: cp, Exists: true}
	if info.Mode().IsRegular() {
		snap.Mode = info.Mode().Perm()
	}
	return snap, nil
}

// dirModeSnapshot records the mode of the directory at path, as info reports
// it (permission bits plus setuid, setgid and sticky), so a rollback can undo
// a chmod a failed step made to it.
func dirModeSnapshot(path string, info os.FileInfo) FileSnapshot {
	return FileSnapshot{Path: path, Exists: true, Mode: fsys.ComparableMode(info.Mode()), Dir: true}
}

// RestoreSnapshots restores every snapshot, best-effort: it attempts all of them
// and aggregates failures rather than stopping at the first, so a partial
// rollback still recovers as many files as possible.
func RestoreSnapshots(fs fsys.FS, snapshots []FileSnapshot) error {
	var failures []string
	for _, snap := range snapshots {
		if err := restoreSnapshot(fs, snap); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", snap.Path, err))
		}
	}
	if len(failures) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(failures, "; "))
}

func restoreSnapshot(fs fsys.FS, snap FileSnapshot) error {
	if snap.Dir {
		return restoreDirMode(fs, snap)
	}
	if !snap.Exists {
		if err := fs.Remove(snap.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	perm := snap.Mode
	if perm == 0 {
		perm = 0o644
	}
	// A file whose bytes and mode already match is left alone: rewriting it
	// would replace its inode (detaching a hard link) and bump its mtime.
	return fsys.WriteFileIfContentOrModeChangedAtomic(fs, snap.Path, snap.Data, perm)
}

// restoreDirMode puts a directory's snapshotted mode back. A directory that is
// gone has nothing to restore.
func restoreDirMode(fs fsys.FS, snap FileSnapshot) error {
	info, err := fs.Stat(snap.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if fsys.ComparableMode(info.Mode()) == snap.Mode {
		return nil
	}
	return fs.Chmod(snap.Path, snap.Mode)
}
