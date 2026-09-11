package fsys_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

func TestResolveSymlinks_RelativeLinkBehindSymlinkedParent(t *testing.T) {
	// A `..`-bearing relative target behind a symlinked parent directory
	// must resolve against the physical parent, not the lexical one:
	// /base/linkcity -> /other/realcity and city.toml -> ../checkout/city.toml
	// resolves to /other/checkout/city.toml, where a lexical join would
	// cancel the symlinked component and yield /base/checkout/city.toml.
	fs := fsys.NewFake()
	fs.Dirs["/base"] = true
	fs.Dirs["/other/realcity"] = true
	fs.Dirs["/other/checkout"] = true
	fs.Symlinks["/base/linkcity"] = "/other/realcity"
	fs.Symlinks["/other/realcity/city.toml"] = "../checkout/city.toml"
	fs.Files["/other/checkout/city.toml"] = []byte("data")

	got, err := fsys.ResolveSymlinks(fs, "/base/linkcity/city.toml")
	if err != nil {
		t.Fatalf("ResolveSymlinks: %v", err)
	}
	if got != "/other/checkout/city.toml" {
		t.Fatalf("got %q, want /other/checkout/city.toml", got)
	}
}

func TestResolveSymlinks_NonDirIntermediateComponentErrors(t *testing.T) {
	fs := fsys.NewFake()
	fs.Files["/city/city.toml"] = []byte("data")

	_, err := fsys.ResolveSymlinks(fs, "/city/city.toml/nested")
	if err == nil {
		t.Fatal("ResolveSymlinks through a regular file succeeded, want error")
	}
	if !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("error = %v, want ENOTDIR", err)
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
	dir := osTempDirPhysical(t)
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

func TestResolveSymlinks_OSFSRelativeLinkBehindSymlinkedParent(t *testing.T) {
	dir := osTempDirPhysical(t)
	for _, sub := range []string{"other/realcity", "other/checkout"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	want := filepath.Join(dir, "other", "checkout", "city.toml")
	if err := os.WriteFile(want, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "other", "realcity"), filepath.Join(dir, "linkcity")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "checkout", "city.toml"), filepath.Join(dir, "other", "realcity", "city.toml")); err != nil {
		t.Fatal(err)
	}

	got, err := fsys.ResolveSymlinks(fsys.OSFS{}, filepath.Join(dir, "linkcity", "city.toml"))
	if err != nil {
		t.Fatalf("ResolveSymlinks: %v", err)
	}
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
	kernel, err := filepath.EvalSymlinks(filepath.Join(dir, "linkcity", "city.toml"))
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	if got != kernel {
		t.Fatalf("got %q, want kernel resolution %q", got, kernel)
	}
}

// osTempDirPhysical returns a fresh temp dir with any symlinked prefix
// (e.g. /tmp or /var on some platforms) resolved, so resolved-path
// equality assertions compare physical paths to physical paths.
func osTempDirPhysical(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
