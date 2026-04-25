package cityinit

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/fsys"
)

var _ ScaffoldFS = testOSScaffoldFS{}

type testOSScaffoldFS struct{ fsys.OSFS }

func (testOSScaffoldFS) Walk(root string, fn filepath.WalkFunc) error {
	return filepath.Walk(root, fn)
}

func (testOSScaffoldFS) Readlink(name string) (string, error) {
	return os.Readlink(name)
}

func (testOSScaffoldFS) Symlink(oldname, newname string) error {
	return os.Symlink(oldname, newname)
}

func (testOSScaffoldFS) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

var _ ScaffoldFS = (*fakeScaffoldFS)(nil)

type fakeScaffoldFS struct {
	*fsys.Fake
}

func (f *fakeScaffoldFS) Walk(root string, fn filepath.WalkFunc) error {
	root = filepath.Clean(root)
	var paths []string
	collectPaths := func(m map[string]bool) {
		for p := range m {
			if p == root || strings.HasPrefix(p, root+string(filepath.Separator)) {
				paths = append(paths, p)
			}
		}
	}
	collectFiles := func(m map[string][]byte) {
		for p := range m {
			if p == root || strings.HasPrefix(p, root+string(filepath.Separator)) {
				paths = append(paths, p)
			}
		}
	}
	collectSymlinks := func(m map[string]string) {
		for p := range m {
			if p == root || strings.HasPrefix(p, root+string(filepath.Separator)) {
				paths = append(paths, p)
			}
		}
	}
	collectPaths(f.Dirs)
	collectFiles(f.Files)
	collectSymlinks(f.Symlinks)

	seen := make(map[string]bool, len(paths))
	var unique []string
	for _, p := range paths {
		if !seen[p] {
			seen[p] = true
			unique = append(unique, p)
		}
	}
	sort.Strings(unique)

	for _, p := range unique {
		info, err := f.Lstat(p)
		if err != nil {
			if walkErr := fn(p, nil, err); walkErr != nil {
				return walkErr
			}
			continue
		}
		if walkErr := fn(p, info, nil); walkErr != nil {
			if errors.Is(walkErr, filepath.SkipDir) {
				continue
			}
			return walkErr
		}
	}
	return nil
}

func (f *fakeScaffoldFS) Readlink(name string) (string, error) {
	if target, ok := f.Symlinks[name]; ok {
		return target, nil
	}
	return "", &os.PathError{Op: "readlink", Path: name, Err: os.ErrNotExist}
}

func (f *fakeScaffoldFS) Symlink(oldname, newname string) error {
	if f.Symlinks == nil {
		f.Symlinks = make(map[string]string)
	}
	f.Symlinks[newname] = oldname
	return nil
}

func (f *fakeScaffoldFS) RemoveAll(path string) error {
	path = filepath.Clean(path)
	prefix := path + string(filepath.Separator)
	for p := range f.Dirs {
		if p == path || strings.HasPrefix(p, prefix) {
			delete(f.Dirs, p)
		}
	}
	for p := range f.Files {
		if p == path || strings.HasPrefix(p, prefix) {
			delete(f.Files, p)
		}
	}
	for p := range f.Symlinks {
		if p == path || strings.HasPrefix(p, prefix) {
			delete(f.Symlinks, p)
		}
	}
	return nil
}
