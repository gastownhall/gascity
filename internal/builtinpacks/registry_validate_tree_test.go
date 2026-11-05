package builtinpacks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateSyntheticRepoRejectsDeletedFile pins that a cache which is *short*
// of an expected file is rejected, and that the rejection names the missing path
// relative to the cache root.
//
// Today that property comes for free: validatePackFiles does a manifest-driven
// os.Lstat per expected file, and an Lstat cannot miss a deletion. The test is a
// forward guard for any future single-traversal validator, because that is the
// one integrity property whose mechanism would not survive the change.
// A filepath.WalkDir never visits a path that is not there, so a walk that only
// rejects *unexpected* paths accepts a cache with files removed — the worst
// direction for a cache-integrity check to fail, because the rehydration it
// gates would never fire.
//
// The package had no test for the deletion class; it was covered only
// implicitly by the per-file Lstat.
func TestValidateSyntheticRepoRejectsDeletedFile(t *testing.T) {
	cases := []struct {
		name   string
		remove string
		isDir  bool
	}{
		{
			name:   "one file",
			remove: "internal/bootstrap/packs/core/pack.toml",
		},
		{
			name:   "a whole pack subtree",
			remove: "examples/gastown/packs/gastown",
			isDir:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := materializeTestRepo(t)
			target := filepath.Join(dst, filepath.FromSlash(tc.remove))
			if _, err := os.Stat(target); err != nil {
				t.Fatalf("fixture does not contain %s: %v", tc.remove, err)
			}
			if tc.isDir {
				if err := os.RemoveAll(target); err != nil {
					t.Fatalf("RemoveAll(%q): %v", target, err)
				}
			} else {
				if err := os.Remove(target); err != nil {
					t.Fatalf("Remove(%q): %v", target, err)
				}
			}

			err := ValidateSyntheticRepo(dst, Repository, testCommit)
			if err == nil {
				t.Fatalf("a cache missing %s validated clean; deletion is undetected", tc.remove)
			}
			if strings.Contains(err.Error(), "unexpected") {
				t.Fatalf("error = %v, want a missing-file rejection, not an unexpected-path one", err)
			}
			if !strings.Contains(err.Error(), tc.remove) {
				t.Fatalf("error = %v, want it to name the missing path %s relative to the cache root", err, tc.remove)
			}
		})
	}
}

// TestValidateSyntheticRepoRejectsUnexpectedDirectory covers a long-standing
// gap: the validator has always rejected a directory it does not expect, and
// nothing anywhere asserted it. Deleting the unexpected-directory bookkeeping
// entirely left the package green.
//
// The directories are empty on purpose. A directory with a file in it is already
// rejected for containing an unexpected *file*, which would let this test pass
// without the directory check existing at all.
func TestValidateSyntheticRepoRejectsUnexpectedDirectory(t *testing.T) {
	cases := []struct {
		name   string
		create string
	}{
		{
			name:   "at the cache root",
			create: "zzz-empty",
		},
		{
			name:   "inside an expected pack directory",
			create: "internal/bootstrap/packs/core/zzz-empty",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := materializeTestRepo(t)
			if err := os.Mkdir(filepath.Join(dst, filepath.FromSlash(tc.create)), 0o755); err != nil {
				t.Fatalf("Mkdir(%q): %v", tc.create, err)
			}

			err := ValidateSyntheticRepo(dst, Repository, testCommit)
			if err == nil {
				t.Fatalf("ValidateSyntheticRepo accepted unexpected directory %s", tc.create)
			}
			want := "unexpected directory " + tc.create
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("error = %v, want it to contain %q", err, want)
			}
		})
	}
}
