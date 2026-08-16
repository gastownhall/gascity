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
// This is the one integrity property whose mechanism is fragile under any
// single-traversal validation. A manifest-driven os.Lstat per expected file
// cannot miss a deletion. A filepath.WalkDir can: it never visits a path that is
// not there, so a walk that only rejects *unexpected* paths accepts a cache with
// files removed — the worst direction for a cache-integrity check to fail,
// because the rehydration it gates would never fire.
//
// The package had no test for the deletion class; it was covered only implicitly
// by the per-file Lstat.
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

			err := ValidateSyntheticRepo(dst, testCommit)
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

// TestValidateSyntheticRepoAcceptsAFreshlyMaterializedCache pins the other side
// of the missing-file check: the set of paths the validator expects and the set
// MaterializeSyntheticRepo writes are the same set. A shortfall check that is
// even one path out would reject every valid cache, which is why this assertion
// belongs next to the one above rather than being left to the other tests.
func TestValidateSyntheticRepoAcceptsAFreshlyMaterializedCache(t *testing.T) {
	dst := materializeTestRepo(t)
	if err := ValidateSyntheticRepo(dst, testCommit); err != nil {
		t.Fatalf("freshly materialized cache is invalid: %v", err)
	}
}

// TestValidateSyntheticRepoRejectsUnexpectedDirectory covers a gap that predates
// this change: the validator has always rejected a directory it does not expect,
// and nothing anywhere asserted it. Deleting the unexpected-directory bookkeeping
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

			err := ValidateSyntheticRepo(dst, testCommit)
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

// TestNoBundledPackEmbedsTheCacheMarkerName pins the assumption the tree walk
// relies on when it skips the marker: no bundled pack contributes a file at that
// path, so skipping it cannot skip a file whose content should have been
// checked. Today a pack would have to sit at the cache root to collide at all,
// but the layout set is configuration and this keeps the assumption honest.
//
// The assertion only says that because this change stopped seeding the marker
// into the allowed-file set. Before it, the marker was a hardcoded member of
// that set and its presence there said nothing about what the packs embed; the
// same assertion would have failed on a set that was correct. It is meaningful
// here precisely because the set is now derived from the pack manifests alone.
func TestNoBundledPackEmbedsTheCacheMarkerName(t *testing.T) {
	allowedFiles, _, err := syntheticRepoAllowedPaths()
	if err != nil {
		t.Fatalf("syntheticRepoAllowedPaths: %v", err)
	}
	if _, ok := allowedFiles[syntheticMarkerFile]; ok {
		t.Fatalf("a bundled pack contributes %s, which the tree walk skips as the cache marker", syntheticMarkerFile)
	}
}

// TestValidateSyntheticRepoRejectsSameLengthTamper pins that the byte-for-byte
// content comparison still runs for every expected file. It is deliberately
// redundant with TestValidateSyntheticRepoRejectsTamperedContent: that test
// rewrites a file to a different length, which a size or stat shortcut would
// also catch, while this one flips a single bit in place.
func TestValidateSyntheticRepoRejectsSameLengthTamper(t *testing.T) {
	dst := materializeTestRepo(t)
	target := filepath.Join(dst, filepath.FromSlash("internal/bootstrap/packs/core/pack.toml"))
	original, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile(%q): %v", target, err)
	}
	if len(original) == 0 {
		t.Fatalf("fixture file %s is empty; it cannot be tampered with in place", target)
	}
	tampered := make([]byte, len(original))
	copy(tampered, original)
	tampered[len(tampered)-1] ^= 0xFF
	if err := os.WriteFile(target, tampered, 0o644); err != nil {
		t.Fatalf("WriteFile(%q): %v", target, err)
	}

	err = ValidateSyntheticRepo(dst, testCommit)
	if err == nil {
		t.Fatal("ValidateSyntheticRepo accepted same-length content drift")
	}
	if !strings.Contains(err.Error(), "content differs") {
		t.Fatalf("error = %v, want content differs", err)
	}
}
