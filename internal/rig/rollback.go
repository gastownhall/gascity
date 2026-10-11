package rig

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/gastownhall/gascity/internal/fsys"
)

// FileSnapshot captures a file's contents (or its absence) so a failed
// multi-file provisioning step can be rolled back atomically. It is the unit of
// the rig-add / rig set-endpoint topology rollback.
type FileSnapshot struct {
	Path   string
	Data   []byte
	Exists bool
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
	cp := append([]byte(nil), data...)
	return FileSnapshot{Path: path, Data: cp, Exists: true}, nil
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
	if !snap.Exists {
		if err := fs.Remove(snap.Path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return fsys.WriteFileAtomic(fs, snap.Path, snap.Data, 0o644)
}
