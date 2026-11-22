package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/fsys"
)

// EditSnapshot records the content and existence of a fixed set of config
// files before a multi-file edit, so a failed edit can put them back.
type EditSnapshot struct {
	snapshot *configFileRestoreSnapshot
}

// SnapshotFilesForEdit records the current content, mode and existence of
// each path. A missing path is recorded as absent; any other read error is
// returned.
func SnapshotFilesForEdit(fs fsys.FS, paths ...string) (*EditSnapshot, error) {
	snapshot, err := snapshotCityAndSiteFiles(fs, paths...)
	if err != nil {
		return nil, err
	}
	return &EditSnapshot{snapshot: snapshot}, nil
}

// RestoreChanged restores every snapshotted file whose current state differs
// from the snapshot: changed content is rewritten atomically and a file that
// did not exist is removed. Files that still match are left untouched, so a
// refused edit that wrote nothing churns no mtimes. It returns how many files
// it restored and every restore failure, joined.
func (s *EditSnapshot) RestoreChanged(fs fsys.FS) (int, error) {
	changed := &configFileRestoreSnapshot{files: make(map[string]configFileSnapshot)}
	var readErr error
	for path, want := range s.snapshot.files {
		data, err := fs.ReadFile(path)
		switch {
		case err == nil:
			if want.existed && bytes.Equal(data, want.data) {
				continue
			}
		case os.IsNotExist(err):
			if !want.existed {
				continue
			}
		default:
			readErr = errors.Join(readErr, fmt.Errorf("reading %s to restore it: %w", path, err))
		}
		changed.files[path] = want
	}
	return len(changed.files), errors.Join(readErr, changed.restore(fs))
}
