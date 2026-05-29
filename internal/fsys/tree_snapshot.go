package fsys

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
)

// TreeSnapshot captures the regular files and directories under one path so
// they can be restored after a failed multi-file mutation.
type TreeSnapshot struct {
	root   string
	exists bool
	dirs   map[string]os.FileMode
	files  map[string]treeSnapshotFile
	links  map[string]string
}

type treeSnapshotFile struct {
	data []byte
	mode os.FileMode
}

type readlinkFS interface {
	Readlink(name string) (string, error)
}

type symlinkFS interface {
	Symlink(oldname, newname string) error
}

// SnapshotTree captures root using fs. Missing roots produce a snapshot whose
// restore removes any later root created at the same path.
func SnapshotTree(fs FS, root string) (*TreeSnapshot, error) {
	snapshot := &TreeSnapshot{
		root:  root,
		dirs:  make(map[string]os.FileMode),
		files: make(map[string]treeSnapshotFile),
		links: make(map[string]string),
	}
	if err := snapshot.capture(fs, root); err != nil {
		if os.IsNotExist(err) {
			return snapshot, nil
		}
		return nil, err
	}
	snapshot.exists = true
	return snapshot, nil
}

func (s *TreeSnapshot) capture(fs FS, path string) error {
	info, err := fs.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		linkFS, ok := fs.(readlinkFS)
		if !ok {
			return fmt.Errorf("filesystem cannot preserve symlink in tree snapshot: %s", path)
		}
		target, err := linkFS.Readlink(path)
		if err != nil {
			return fmt.Errorf("reading symlink %s: %w", path, err)
		}
		s.links[path] = target
		return nil
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported non-regular file in tree snapshot: %s", path)
		}
		data, err := fs.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		s.files[path] = treeSnapshotFile{data: data, mode: info.Mode().Perm()}
		return nil
	}

	s.dirs[path] = info.Mode().Perm()
	entries, err := fs.ReadDir(path)
	if err != nil {
		return fmt.Errorf("reading directory %s: %w", path, err)
	}
	for _, entry := range entries {
		if err := s.capture(fs, filepath.Join(path, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

// Restore removes the current root and recreates the captured tree.
func (s *TreeSnapshot) Restore(fs FS) error {
	if s == nil {
		return nil
	}
	var restoreErr error
	if err := RemoveAll(fs, s.root); err != nil {
		restoreErr = errors.Join(restoreErr, fmt.Errorf("removing current %s: %w", s.root, err))
	}
	if !s.exists {
		return restoreErr
	}

	dirs := make([]string, 0, len(s.dirs))
	for dir := range s.dirs {
		dirs = append(dirs, dir)
	}
	sort.Slice(dirs, func(i, j int) bool {
		if len(dirs[i]) == len(dirs[j]) {
			return dirs[i] < dirs[j]
		}
		return len(dirs[i]) < len(dirs[j])
	})
	for _, dir := range dirs {
		mode := s.dirs[dir]
		if err := fs.MkdirAll(dir, mode); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("creating %s: %w", dir, err))
			continue
		}
		if err := fs.Chmod(dir, mode); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("chmod %s: %w", dir, err))
		}
	}

	files := make([]string, 0, len(s.files))
	for path := range s.files {
		files = append(files, path)
	}
	sort.Strings(files)
	for _, path := range files {
		file := s.files[path]
		if err := fs.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("creating %s: %w", filepath.Dir(path), err))
			continue
		}
		if err := WriteFileAtomic(fs, path, file.data, file.mode); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restoring %s: %w", path, err))
		}
	}

	links := make([]string, 0, len(s.links))
	for path := range s.links {
		links = append(links, path)
	}
	sort.Strings(links)
	linkFS, linkCapable := fs.(symlinkFS)
	for _, path := range links {
		target := s.links[path]
		if err := fs.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("creating %s: %w", filepath.Dir(path), err))
			continue
		}
		if !linkCapable {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("filesystem cannot restore symlink %s", path))
			continue
		}
		if err := linkFS.Symlink(target, path); err != nil {
			restoreErr = errors.Join(restoreErr, fmt.Errorf("restoring symlink %s: %w", path, err))
		}
	}

	return restoreErr
}
