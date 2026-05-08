package fsys

import (
	"os"
	"testing"
	"time"
)

func TestWriteFileIfContentOrModeChangedAtomic_RewritesWhenIdentityChanges(t *testing.T) {
	fs := &identityChangingFS{data: []byte("#!/bin/sh\n")}

	if err := WriteFileIfContentOrModeChangedAtomic(fs, "/script.sh", fs.data, 0o755); err != nil {
		t.Fatalf("WriteFileIfContentOrModeChangedAtomic: %v", err)
	}

	if !fs.renamed {
		t.Fatalf("identity-changing file was not rewritten")
	}
}

func TestWriteFileIfChangedAtomic_RewritesWhenIdentityChanges(t *testing.T) {
	fs := &identityChangingFS{data: []byte("hello = true\n")}

	if err := WriteFileIfChangedAtomic(fs, "/config.toml", fs.data, 0o644); err != nil {
		t.Fatalf("WriteFileIfChangedAtomic: %v", err)
	}

	if !fs.renamed {
		t.Fatalf("identity-changing file was not rewritten")
	}
}

func TestWriteFileIfContentOrModeChangedAtomic_RewritesWithoutSnapshotIdentity(t *testing.T) {
	fs := &noIdentitySnapshotFS{data: []byte("#!/bin/sh\n")}

	if err := WriteFileIfContentOrModeChangedAtomic(fs, "/script.sh", fs.data, 0o755); err != nil {
		t.Fatalf("WriteFileIfContentOrModeChangedAtomic: %v", err)
	}

	if !fs.renamed {
		t.Fatalf("no-identity snapshot was not rewritten")
	}
}

func TestWriteFileIfChangedAtomic_RewritesWithoutSnapshotIdentity(t *testing.T) {
	fs := &noIdentitySnapshotFS{data: []byte("hello = true\n")}

	if err := WriteFileIfChangedAtomic(fs, "/config.toml", fs.data, 0o644); err != nil {
		t.Fatalf("WriteFileIfChangedAtomic: %v", err)
	}

	if !fs.renamed {
		t.Fatalf("no-identity snapshot was not rewritten")
	}
}

func TestFileIdentityFromSys_NormalizesSignedDeviceField(t *testing.T) {
	id, ok := fileIdentityFromSys(struct {
		Dev int32
		Ino uint64
	}{
		Dev: 7,
		Ino: 11,
	})
	if !ok {
		t.Fatalf("fileIdentityFromSys returned ok=false for signed Dev field")
	}

	want := fileIdentity{dev: 7, ino: 11}
	if id != want {
		t.Fatalf("fileIdentityFromSys = %#v, want %#v", id, want)
	}
}

func TestFileIdentityFromSys_NormalizesSignedDeviceFieldPointer(t *testing.T) {
	id, ok := fileIdentityFromSys(&struct {
		Dev int32
		Ino uint64
	}{
		Dev: 7,
		Ino: 11,
	})
	if !ok {
		t.Fatalf("fileIdentityFromSys returned ok=false for pointer-shaped signed Dev field")
	}

	want := fileIdentity{dev: 7, ino: 11}
	if id != want {
		t.Fatalf("fileIdentityFromSys = %#v, want %#v", id, want)
	}
}

func TestFileIdentityFromSys_PreservesNegativeSignedDeviceFieldBits(t *testing.T) {
	id, ok := fileIdentityFromSys(struct {
		Dev int32
		Ino uint64
	}{
		Dev: -1,
		Ino: 11,
	})
	if !ok {
		t.Fatalf("fileIdentityFromSys returned ok=false for negative signed Dev field")
	}

	dev := int32(-1)
	want := fileIdentity{dev: uint64(dev), ino: 11}
	if id != want {
		t.Fatalf("fileIdentityFromSys = %#v, want %#v", id, want)
	}
}

type identityChangingFS struct {
	data        []byte
	snapshotErr error
	renamed     bool
	lstats      int
}

func (f *identityChangingFS) MkdirAll(string, os.FileMode) error { return nil }

func (f *identityChangingFS) WriteFile(string, []byte, os.FileMode) error { return nil }

func (f *identityChangingFS) ReadFile(string) ([]byte, error) { return f.data, nil }

func (f *identityChangingFS) Stat(string) (os.FileInfo, error) {
	return identityFileInfo{mode: 0o755, id: fileIdentity{dev: 1, ino: 1}}, nil
}

func (f *identityChangingFS) Lstat(string) (os.FileInfo, error) {
	f.lstats++
	id := fileIdentity{dev: 1, ino: 1}
	if f.lstats > 1 {
		id = fileIdentity{dev: 1, ino: 2}
	}
	return identityFileInfo{mode: 0o755, id: id}, nil
}

func (f *identityChangingFS) ReadDir(string) ([]os.DirEntry, error) { return nil, nil }

func (f *identityChangingFS) Rename(string, string) error {
	f.renamed = true
	return nil
}

func (f *identityChangingFS) Remove(string) error { return nil }

func (f *identityChangingFS) Chmod(string, os.FileMode) error { return nil }

func (f *identityChangingFS) readRegularFileSnapshot(string) (regularFileSnapshot, error) {
	if f.snapshotErr != nil {
		return regularFileSnapshot{}, f.snapshotErr
	}
	return regularFileSnapshot{
		data:  f.data,
		id:    fileIdentity{dev: 1, ino: 1},
		hasID: true,
	}, nil
}

type identityFileInfo struct {
	mode os.FileMode
	id   fileIdentity
}

func (i identityFileInfo) Name() string       { return "script.sh" }
func (i identityFileInfo) Size() int64        { return int64(len("#!/bin/sh\n")) }
func (i identityFileInfo) Mode() os.FileMode  { return i.mode }
func (i identityFileInfo) ModTime() time.Time { return time.Time{} }
func (i identityFileInfo) IsDir() bool        { return false }
func (i identityFileInfo) Sys() any           { return struct{ Dev, Ino uint64 }{i.id.dev, i.id.ino} }

var _ FS = (*identityChangingFS)(nil)

type noIdentitySnapshotFS struct {
	data        []byte
	snapshotErr error
	renamed     bool
}

func (f *noIdentitySnapshotFS) MkdirAll(string, os.FileMode) error { return nil }

func (f *noIdentitySnapshotFS) WriteFile(string, []byte, os.FileMode) error { return nil }

func (f *noIdentitySnapshotFS) ReadFile(string) ([]byte, error) { return f.data, nil }

func (f *noIdentitySnapshotFS) Stat(string) (os.FileInfo, error) {
	return identityFileInfo{mode: 0o755, id: fileIdentity{dev: 1, ino: 1}}, nil
}

func (f *noIdentitySnapshotFS) Lstat(string) (os.FileInfo, error) {
	return identityFileInfo{mode: 0o755, id: fileIdentity{dev: 1, ino: 1}}, nil
}

func (f *noIdentitySnapshotFS) ReadDir(string) ([]os.DirEntry, error) { return nil, nil }

func (f *noIdentitySnapshotFS) Rename(string, string) error {
	f.renamed = true
	return nil
}

func (f *noIdentitySnapshotFS) Remove(string) error { return nil }

func (f *noIdentitySnapshotFS) Chmod(string, os.FileMode) error { return nil }

func (f *noIdentitySnapshotFS) readRegularFileSnapshot(string) (regularFileSnapshot, error) {
	if f.snapshotErr != nil {
		return regularFileSnapshot{}, f.snapshotErr
	}
	return regularFileSnapshot{data: f.data}, nil
}

var _ FS = (*noIdentitySnapshotFS)(nil)
