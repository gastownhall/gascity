package fsys_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestResolveSymlinks_RegularFileReturnsPathUnchanged(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte("data")

	got, err := fsys.ResolveSymlinks(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("ResolveSymlinks: %v", err)
	}
	if got != "/city/city.toml" {
		t.Fatalf("got %q, want /city/city.toml", got)
	}
}

func TestResolveSymlinks_MissingPathReturnsPathUnchanged(t *testing.T) {
	fs := fsys.NewFake()

	got, err := fsys.ResolveSymlinks(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("ResolveSymlinks: %v", err)
	}
	if got != "/city/city.toml" {
		t.Fatalf("got %q, want /city/city.toml", got)
	}
}

func TestResolveSymlinks_FollowsAbsoluteLink(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/checkout/city.toml"] = []byte("data")
	fs.Symlinks["/city/city.toml"] = "/checkout/city.toml"

	got, err := fsys.ResolveSymlinks(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("ResolveSymlinks: %v", err)
	}
	if got != "/checkout/city.toml" {
		t.Fatalf("got %q, want /checkout/city.toml", got)
	}
}

func TestResolveSymlinks_ResolvesRelativeLinkAgainstLinkDir(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/checkout/city.toml"] = []byte("data")
	fs.Symlinks["/city/city.toml"] = "../checkout/city.toml"

	got, err := fsys.ResolveSymlinks(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("ResolveSymlinks: %v", err)
	}
	if got != "/checkout/city.toml" {
		t.Fatalf("got %q, want /checkout/city.toml", got)
	}
}

func TestResolveSymlinks_FollowsChains(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/final/city.toml"] = []byte("data")
	fs.Symlinks["/city/city.toml"] = "/mid/city.toml"
	fs.Symlinks["/mid/city.toml"] = "/final/city.toml"

	got, err := fsys.ResolveSymlinks(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("ResolveSymlinks: %v", err)
	}
	if got != "/final/city.toml" {
		t.Fatalf("got %q, want /final/city.toml", got)
	}
}

func TestResolveSymlinks_DanglingLinkReturnsTarget(t *testing.T) {
	fs := fsys.NewFake()
	fs.Symlinks["/city/city.toml"] = "/checkout/city.toml"

	got, err := fsys.ResolveSymlinks(fs, "/city/city.toml")
	if err != nil {
		t.Fatalf("ResolveSymlinks: %v", err)
	}
	if got != "/checkout/city.toml" {
		t.Fatalf("got %q, want /checkout/city.toml", got)
	}
}

func TestResolveSymlinks_CycleErrors(t *testing.T) {
	fs := fsys.NewFake()
	fs.Symlinks["/a"] = "/b"
	fs.Symlinks["/b"] = "/a"

	_, err := fsys.ResolveSymlinks(fs, "/a")
	if err == nil {
		t.Fatal("ResolveSymlinks on a cycle succeeded, want error")
	}
	if !strings.Contains(err.Error(), "too many levels of symbolic links") {
		t.Fatalf("error = %v, want symlink-depth error", err)
	}
}

// noReadlinkFS wraps an FS but does not expose Readlink, simulating a
// filesystem implementation without symlink-target support.
type noReadlinkFS struct {
	inner fsys.FS
}

func (f noReadlinkFS) MkdirAll(path string, perm os.FileMode) error {
	return f.inner.MkdirAll(path, perm)
}

func (f noReadlinkFS) WriteFile(name string, data []byte, perm os.FileMode) error {
	return f.inner.WriteFile(name, data, perm)
}
func (f noReadlinkFS) ReadFile(name string) ([]byte, error)       { return f.inner.ReadFile(name) }
func (f noReadlinkFS) Stat(name string) (os.FileInfo, error)      { return f.inner.Stat(name) }
func (f noReadlinkFS) Lstat(name string) (os.FileInfo, error)     { return f.inner.Lstat(name) }
func (f noReadlinkFS) ReadDir(name string) ([]os.DirEntry, error) { return f.inner.ReadDir(name) }
func (f noReadlinkFS) Rename(oldpath, newpath string) error       { return f.inner.Rename(oldpath, newpath) }
func (f noReadlinkFS) Remove(name string) error                   { return f.inner.Remove(name) }
func (f noReadlinkFS) Chmod(name string, mode os.FileMode) error  { return f.inner.Chmod(name, mode) }

func TestResolveSymlinks_ErrorsWhenFSCannotReadlink(t *testing.T) {
	fake := fsys.NewFake()
	fake.Symlinks["/city/city.toml"] = "/checkout/city.toml"

	_, err := fsys.ResolveSymlinks(noReadlinkFS{inner: fake}, "/city/city.toml")
	if err == nil {
		t.Fatal("ResolveSymlinks succeeded, want error for FS without Readlink")
	}
	if !strings.Contains(err.Error(), "Readlink") {
		t.Fatalf("error = %v, want mention of missing Readlink support", err)
	}
}

func TestResolveSymlinks_OSFS(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "checkout", "city.toml")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "city.toml")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	got, err := fsys.ResolveSymlinks(fsys.OSFS{}, link)
	if err != nil {
		t.Fatalf("ResolveSymlinks: %v", err)
	}
	if got != target {
		t.Fatalf("got %q, want %q", got, target)
	}
}
