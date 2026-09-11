// Package fsystest provides conformance tests for fsys.FS implementations.
package fsystest

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// RunConformance exercises the portable namespace contract shared by real
// filesystems and reusable filesystem doubles. newFS must return a fresh,
// empty implementation for every call.
func RunConformance[T fsys.FS](t *testing.T, newFS func() T) {
	t.Helper()

	t.Run("RegularFileRoundTripUsesDefensiveCopies", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		path := filepath.Join(root, "round-trip.txt")
		input := []byte("original")
		if err := filesystem.WriteFile(path, input, 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}

		input[0] = 'X'
		first := mustReadFile(t, filesystem, path)
		if !bytes.Equal(first, []byte("original")) {
			t.Fatalf("content after mutating WriteFile input = %q, want %q", first, "original")
		}

		first[0] = 'Y'
		second := mustReadFile(t, filesystem, path)
		if !bytes.Equal(second, []byte("original")) {
			t.Fatalf("content after mutating ReadFile result = %q, want %q", second, "original")
		}
		info := mustStat(t, filesystem, path)
		if !info.Mode().IsRegular() {
			t.Fatalf("mode = %v, want regular file", info.Mode())
		}
	})

	t.Run("WriteFileRequiresExistingParent", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		parent := filepath.Join(root, "missing")
		path := filepath.Join(parent, "file.txt")
		if err := filesystem.WriteFile(path, []byte("data"), 0o600); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("WriteFile without parent error = %v, want fs.ErrNotExist", err)
		}
		assertNotExist(t, filesystem, parent)
		assertNotExist(t, filesystem, path)
	})

	t.Run("MkdirAllRejectsFileAncestor", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		ancestor := filepath.Join(root, "ancestor")
		child := filepath.Join(ancestor, "child")
		if err := filesystem.WriteFile(ancestor, []byte("file"), 0o600); err != nil {
			t.Fatalf("WriteFile ancestor: %v", err)
		}
		if err := filesystem.MkdirAll(child, 0o700); err == nil {
			t.Error("MkdirAll through file ancestor succeeded")
		}
		if info := mustStat(t, filesystem, ancestor); !info.Mode().IsRegular() {
			t.Errorf("ancestor mode = %v, want regular file", info.Mode())
		}
		if info, err := filesystem.Stat(child); err == nil {
			t.Errorf("Stat(%q) = (%v, nil), want inaccessible child", child, info)
		}
	})

	t.Run("MkdirAllRejectsExistingFile", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		path := filepath.Join(root, "file")
		if err := filesystem.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := filesystem.MkdirAll(path, 0o700); err == nil {
			t.Error("MkdirAll over existing file succeeded")
		}
		if got := mustReadFile(t, filesystem, path); !bytes.Equal(got, []byte("data")) {
			t.Errorf("file content after MkdirAll = %q, want %q", got, "data")
		}
	})

	t.Run("WriteFileRejectsDirectory", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		path := filepath.Join(root, "directory")
		if err := filesystem.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := filesystem.WriteFile(path, []byte("data"), 0o600); err == nil {
			t.Error("WriteFile over directory succeeded")
		}
		if info := mustStat(t, filesystem, path); !info.IsDir() {
			t.Errorf("path mode after WriteFile = %v, want directory", info.Mode())
		}
		if _, err := filesystem.ReadFile(path); err == nil {
			t.Error("ReadFile on directory succeeded after rejected WriteFile")
		}
	})

	t.Run("WriteFilePreservesExistingMode", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		path := filepath.Join(root, "existing.txt")
		if err := filesystem.WriteFile(path, []byte("before"), 0o600); err != nil {
			t.Fatalf("initial WriteFile: %v", err)
		}
		if err := filesystem.Chmod(path, 0o600); err != nil {
			t.Fatalf("initial Chmod: %v", err)
		}
		wantMode := mustStat(t, filesystem, path).Mode().Perm()
		if err := filesystem.WriteFile(path, []byte("after"), 0o644); err != nil {
			t.Fatalf("replacement WriteFile: %v", err)
		}
		if got := mustReadFile(t, filesystem, path); !bytes.Equal(got, []byte("after")) {
			t.Errorf("content = %q, want %q", got, "after")
		}
		if got := mustStat(t, filesystem, path).Mode().Perm(); got != wantMode {
			t.Errorf("mode = %v, want existing mode %v", got, wantMode)
		}
	})

	t.Run("ReadDirRejectsMissingPath", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		path := filepath.Join(root, "missing")
		if _, err := filesystem.ReadDir(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadDir missing error = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("ReadDirRejectsRegularFile", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		path := filepath.Join(root, "file.txt")
		if err := filesystem.WriteFile(path, []byte("data"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if _, err := filesystem.ReadDir(path); err == nil {
			t.Error("ReadDir on regular file succeeded")
		}
		if got := mustReadFile(t, filesystem, path); !bytes.Equal(got, []byte("data")) {
			t.Errorf("file content after ReadDir = %q, want %q", got, "data")
		}
	})

	t.Run("RenameFilePreservesContentAndMode", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		source := filepath.Join(root, "source.txt")
		destination := filepath.Join(root, "destination.txt")
		if err := filesystem.WriteFile(source, []byte("data"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := filesystem.Chmod(source, 0o600); err != nil {
			t.Fatalf("Chmod: %v", err)
		}
		wantMode := mustStat(t, filesystem, source).Mode().Perm()
		if err := filesystem.Rename(source, destination); err != nil {
			t.Fatalf("Rename: %v", err)
		}

		assertNotExist(t, filesystem, source)
		if got := mustReadFile(t, filesystem, destination); !bytes.Equal(got, []byte("data")) {
			t.Errorf("destination content = %q, want %q", got, "data")
		}
		if got := mustStat(t, filesystem, destination).Mode().Perm(); got != wantMode {
			t.Errorf("destination mode = %v, want source mode %v", got, wantMode)
		}
	})

	t.Run("RenameMovesDirectoryTree", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		source := filepath.Join(root, "source")
		sourceChild := filepath.Join(source, "nested", "file.txt")
		siblingChild := filepath.Join(root, "source-sibling", "keep.txt")
		destination := filepath.Join(root, "destination")
		destinationChild := filepath.Join(destination, "nested", "file.txt")
		if err := filesystem.MkdirAll(filepath.Dir(sourceChild), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := filesystem.MkdirAll(filepath.Dir(siblingChild), 0o700); err != nil {
			t.Fatalf("MkdirAll sibling: %v", err)
		}
		if err := filesystem.WriteFile(sourceChild, []byte("nested"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := filesystem.WriteFile(siblingChild, []byte("sibling"), 0o600); err != nil {
			t.Fatalf("WriteFile sibling: %v", err)
		}
		if err := filesystem.Chmod(source, 0o750); err != nil {
			t.Fatalf("Chmod source: %v", err)
		}
		if err := filesystem.Chmod(filepath.Dir(sourceChild), 0o710); err != nil {
			t.Fatalf("Chmod nested directory: %v", err)
		}
		sourceMode := mustStat(t, filesystem, source).Mode().Perm()
		nestedMode := mustStat(t, filesystem, filepath.Dir(sourceChild)).Mode().Perm()
		if err := filesystem.Rename(source, destination); err != nil {
			t.Fatalf("Rename directory: %v", err)
		}

		assertNotExist(t, filesystem, source)
		if info := mustStat(t, filesystem, destination); !info.IsDir() || info.Mode().Perm() != sourceMode {
			t.Errorf("renamed directory mode = %v, want directory mode %v", info.Mode(), sourceMode)
		}
		if info := mustStat(t, filesystem, filepath.Join(destination, "nested")); !info.IsDir() || info.Mode().Perm() != nestedMode {
			t.Errorf("renamed child mode = %v, want directory mode %v", info.Mode(), nestedMode)
		}
		if got := mustReadFile(t, filesystem, destinationChild); !bytes.Equal(got, []byte("nested")) {
			t.Errorf("renamed child content = %q, want %q", got, "nested")
		}
		if got := mustReadFile(t, filesystem, siblingChild); !bytes.Equal(got, []byte("sibling")) {
			t.Errorf("prefix-similar sibling content = %q, want %q", got, "sibling")
		}
	})

	t.Run("RenameRequiresDestinationParent", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		source := filepath.Join(root, "source.txt")
		destinationParent := filepath.Join(root, "missing")
		destination := filepath.Join(destinationParent, "destination.txt")
		if err := filesystem.WriteFile(source, []byte("data"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := filesystem.Rename(source, destination); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Rename into missing parent error = %v, want fs.ErrNotExist", err)
		}
		if got := mustReadFile(t, filesystem, source); !bytes.Equal(got, []byte("data")) {
			t.Errorf("source content after Rename = %q, want %q", got, "data")
		}
		assertNotExist(t, filesystem, destinationParent)
		assertNotExist(t, filesystem, destination)
	})

	t.Run("RenameRejectsMissingSource", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		source := filepath.Join(root, "missing.txt")
		destination := filepath.Join(root, "destination.txt")
		if err := filesystem.Rename(source, destination); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Rename missing source error = %v, want fs.ErrNotExist", err)
		}
		assertNotExist(t, filesystem, source)
		assertNotExist(t, filesystem, destination)
	})

	t.Run("RenameRejectsFileDirectoryCollisions", func(t *testing.T) {
		t.Run("file onto directory", func(t *testing.T) {
			filesystem, root := newNamespace(t, newFS)
			file := filepath.Join(root, "file")
			directory := filepath.Join(root, "directory")
			if err := filesystem.WriteFile(file, []byte("file"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
			if err := filesystem.MkdirAll(directory, 0o700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := filesystem.Rename(file, directory); err == nil {
				t.Error("Rename file onto directory succeeded")
			}
			if got := mustReadFile(t, filesystem, file); !bytes.Equal(got, []byte("file")) {
				t.Errorf("source file content = %q, want %q", got, "file")
			}
			if info := mustStat(t, filesystem, directory); !info.IsDir() {
				t.Errorf("destination mode = %v, want directory", info.Mode())
			}
		})

		t.Run("directory onto file", func(t *testing.T) {
			filesystem, root := newNamespace(t, newFS)
			directory := filepath.Join(root, "directory")
			child := filepath.Join(directory, "child.txt")
			file := filepath.Join(root, "file")
			if err := filesystem.MkdirAll(directory, 0o700); err != nil {
				t.Fatalf("MkdirAll: %v", err)
			}
			if err := filesystem.WriteFile(child, []byte("child"), 0o600); err != nil {
				t.Fatalf("WriteFile child: %v", err)
			}
			if err := filesystem.WriteFile(file, []byte("file"), 0o600); err != nil {
				t.Fatalf("WriteFile destination: %v", err)
			}
			if err := filesystem.Rename(directory, file); err == nil {
				t.Error("Rename directory onto file succeeded")
			}
			if got := mustReadFile(t, filesystem, child); !bytes.Equal(got, []byte("child")) {
				t.Errorf("source child content = %q, want %q", got, "child")
			}
			if got := mustReadFile(t, filesystem, file); !bytes.Equal(got, []byte("file")) {
				t.Errorf("destination file content = %q, want %q", got, "file")
			}
		})
	})

	t.Run("RemoveRejectsNonEmptyDirectory", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		directory := filepath.Join(root, "directory")
		child := filepath.Join(directory, "child.txt")
		if err := filesystem.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := filesystem.WriteFile(child, []byte("data"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := filesystem.Remove(directory); err == nil {
			t.Error("Remove non-empty directory succeeded")
		}
		if info := mustStat(t, filesystem, directory); !info.IsDir() {
			t.Errorf("directory mode after Remove = %v, want directory", info.Mode())
		}
		if got := mustReadFile(t, filesystem, child); !bytes.Equal(got, []byte("data")) {
			t.Errorf("child content after Remove = %q, want %q", got, "data")
		}
	})

	t.Run("RemoveDeletesFilesAndEmptyDirectories", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		file := filepath.Join(root, "file.txt")
		directory := filepath.Join(root, "empty")
		if err := filesystem.WriteFile(file, []byte("data"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := filesystem.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := filesystem.Remove(file); err != nil {
			t.Fatalf("Remove file: %v", err)
		}
		if err := filesystem.Remove(directory); err != nil {
			t.Fatalf("Remove empty directory: %v", err)
		}
		assertNotExist(t, filesystem, file)
		assertNotExist(t, filesystem, directory)
		if err := filesystem.Remove(file); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Remove missing file error = %v, want fs.ErrNotExist", err)
		}
	})

	t.Run("ChmodUpdatesFilesAndDirectories", func(t *testing.T) {
		filesystem, root := newNamespace(t, newFS)
		file := filepath.Join(root, "file.txt")
		directory := filepath.Join(root, "directory")
		if err := filesystem.WriteFile(file, []byte("data"), 0o600); err != nil {
			t.Fatalf("WriteFile: %v", err)
		}
		if err := filesystem.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
		if err := filesystem.Chmod(file, 0o640); err != nil {
			t.Fatalf("Chmod file: %v", err)
		}
		if err := filesystem.Chmod(directory, 0o750); err != nil {
			t.Fatalf("Chmod directory: %v", err)
		}
		fileInfo := mustStat(t, filesystem, file)
		if !fileInfo.Mode().IsRegular() {
			t.Errorf("file mode after Chmod = %v, want regular file", fileInfo.Mode())
		}
		directoryInfo := mustStat(t, filesystem, directory)
		if !directoryInfo.IsDir() {
			t.Errorf("directory mode after Chmod = %v, want directory", directoryInfo.Mode())
		}
		if runtime.GOOS == "linux" || runtime.GOOS == "darwin" {
			if got := fileInfo.Mode().Perm(); got != 0o640 {
				t.Errorf("file mode = %v, want 0640", got)
			}
			if got := directoryInfo.Mode().Perm(); got != 0o750 {
				t.Errorf("directory mode = %v, want 0750", got)
			}
		}

		missing := filepath.Join(root, "missing")
		if err := filesystem.Chmod(missing, 0o600); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Chmod missing error = %v, want fs.ErrNotExist", err)
		}
	})
}

func newNamespace[T fsys.FS](t *testing.T, newFS func() T) (fsys.FS, string) {
	t.Helper()
	filesystem := fsys.FS(newFS())
	root := filepath.Join(t.TempDir(), "namespace")
	if !filepath.IsAbs(root) {
		t.Fatalf("test namespace %q is not absolute", root)
	}
	if err := filesystem.MkdirAll(root, 0o700); err != nil {
		t.Fatalf("MkdirAll namespace: %v", err)
	}
	return filesystem, root
}

func mustReadFile(t *testing.T, filesystem fsys.FS, path string) []byte {
	t.Helper()
	data, err := filesystem.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", path, err)
	}
	return data
}

func mustStat(t *testing.T, filesystem fsys.FS, path string) os.FileInfo {
	t.Helper()
	info, err := filesystem.Stat(path)
	if err != nil {
		t.Fatalf("Stat(%q): %v", path, err)
	}
	return info
}

func assertNotExist(t *testing.T, filesystem fsys.FS, path string) {
	t.Helper()
	if info, err := filesystem.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Stat(%q) = (%v, %v), want fs.ErrNotExist", path, info, err)
	}
}
