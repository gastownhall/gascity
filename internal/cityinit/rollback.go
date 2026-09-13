package cityinit

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"syscall"
)

func rollbackScaffoldFailure(sfs ScaffoldFS, dir string, dirExisted bool, rollbackState *scaffoldRollbackState, err error) error {
	if dirExisted && rollbackState != nil {
		if markErr := rollbackState.markScaffoldState(sfs); markErr != nil {
			return errors.Join(err, fmt.Errorf("snapshot scaffold state for rollback: %w", markErr))
		}
		if cleanupErr := rollbackState.restore(sfs); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("restoring existing directory after scaffold failure: %w", cleanupErr))
		}
		return err
	}
	if !dirExisted {
		if cleanupErr := sfs.RemoveAll(dir); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("cleaning scaffold after failure: %w", cleanupErr))
		}
	}
	return err
}

type scaffoldRollbackEntry struct {
	mode       fs.FileMode
	data       []byte
	linkTarget string
}

type scaffoldSnapshot struct {
	root    string
	paths   []string
	entries map[string]scaffoldRollbackEntry
}

type scaffoldRollbackState struct {
	root   string
	paths  []string
	before map[string]scaffoldRollbackEntry
	after  map[string]scaffoldRollbackEntry
}

func newScaffoldRollbackState(sfs ScaffoldFS, root string, paths []string) (*scaffoldRollbackState, error) {
	snapshot, err := captureScaffoldSnapshot(sfs, root, paths)
	if err != nil {
		return nil, err
	}
	return &scaffoldRollbackState{
		root:   root,
		paths:  append([]string(nil), paths...),
		before: snapshot.entries,
	}, nil
}

func captureScaffoldSnapshot(sfs ScaffoldFS, root string, paths []string) (*scaffoldSnapshot, error) {
	snapshot := &scaffoldSnapshot{
		root:    root,
		paths:   append([]string(nil), paths...),
		entries: make(map[string]scaffoldRollbackEntry),
	}
	for _, rel := range paths {
		if err := snapshot.capture(sfs, rel); err != nil {
			return nil, err
		}
	}
	return snapshot, nil
}

func (s *scaffoldSnapshot) capture(sfs ScaffoldFS, rel string) error {
	abs := filepath.Join(s.root, rel)
	_, err := sfs.Lstat(abs)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("snapshot %q: %w", abs, err)
	}
	return sfs.Walk(abs, func(path string, info fs.FileInfo, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("snapshot %q: %w", path, walkErr)
		}
		relPath, err := filepath.Rel(s.root, path)
		if err != nil {
			return fmt.Errorf("relative path for %q: %w", path, err)
		}
		entry := scaffoldRollbackEntry{mode: info.Mode()}
		if info.Mode()&fs.ModeSymlink != 0 {
			target, err := sfs.Readlink(path)
			if err != nil {
				return fmt.Errorf("readlink %q: %w", path, err)
			}
			entry.linkTarget = target
		} else if !info.IsDir() {
			data, err := sfs.ReadFile(path)
			if err != nil {
				return fmt.Errorf("read %q: %w", path, err)
			}
			entry.data = data
		}
		s.entries[filepath.Clean(relPath)] = entry
		return nil
	})
}

func (s *scaffoldRollbackState) markScaffoldState(sfs ScaffoldFS) error {
	snapshot, err := captureScaffoldSnapshot(sfs, s.root, s.paths)
	if err != nil {
		return err
	}
	s.after = snapshot.entries
	return nil
}

func rollbackEntryEqual(a, b scaffoldRollbackEntry) bool {
	return a.mode == b.mode && a.linkTarget == b.linkTarget && bytes.Equal(a.data, b.data)
}

func restoreRollbackEntry(sfs ScaffoldFS, abs string, entry scaffoldRollbackEntry) error {
	switch {
	case entry.mode.IsDir():
		return sfs.MkdirAll(abs, entry.mode.Perm())
	case entry.mode&fs.ModeSymlink != 0:
		if err := sfs.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		if err := sfs.Remove(abs); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return sfs.Symlink(entry.linkTarget, abs)
	default:
		if err := sfs.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
			return err
		}
		return sfs.WriteFile(abs, entry.data, entry.mode.Perm())
	}
}

func (s *scaffoldRollbackState) restore(sfs ScaffoldFS) error {
	current, err := captureScaffoldSnapshot(sfs, s.root, s.paths)
	if err != nil {
		return err
	}

	var errs []error
	var createdDirs []string
	for rel, after := range s.after {
		before, hadBefore := s.before[rel]
		currentEntry, existsNow := current.entries[rel]
		switch {
		case !hadBefore:
			if after.mode.IsDir() {
				createdDirs = append(createdDirs, rel)
				continue
			}
			if existsNow && rollbackEntryEqual(currentEntry, after) {
				if err := sfs.Remove(filepath.Join(s.root, rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
					errs = append(errs, fmt.Errorf("remove %q: %w", filepath.Join(s.root, rel), err))
				}
			}
		case rollbackEntryEqual(before, after):
			continue
		default:
			if after.mode.IsDir() {
				continue
			}
			if existsNow && rollbackEntryEqual(currentEntry, after) {
				if err := restoreRollbackEntry(sfs, filepath.Join(s.root, rel), before); err != nil {
					errs = append(errs, fmt.Errorf("restore %q: %w", filepath.Join(s.root, rel), err))
				}
			}
		}
	}

	for rel, before := range s.before {
		if _, hadAfter := s.after[rel]; hadAfter {
			continue
		}
		if before.mode.IsDir() {
			continue
		}
		if _, existsNow := current.entries[rel]; existsNow {
			continue
		}
		if err := restoreRollbackEntry(sfs, filepath.Join(s.root, rel), before); err != nil {
			errs = append(errs, fmt.Errorf("restore %q: %w", filepath.Join(s.root, rel), err))
		}
	}

	sort.Slice(createdDirs, func(i, j int) bool {
		return len(createdDirs[i]) > len(createdDirs[j])
	})
	for _, rel := range createdDirs {
		if err := sfs.Remove(filepath.Join(s.root, rel)); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if errors.Is(err, syscall.ENOTEMPTY) {
				continue
			}
			errs = append(errs, fmt.Errorf("remove %q: %w", filepath.Join(s.root, rel), err))
		}
	}

	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	return nil
}
