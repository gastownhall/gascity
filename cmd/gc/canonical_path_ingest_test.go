package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/pathutil"
)

// canonicalIngestLayout creates two spellings of one existing directory.
// Normalize the fixture at its boundary so Darwin's /private aliases do not
// become a second, accidental source of differing identity in the expectations.
func canonicalIngestLayout(t *testing.T) (realDir, alias string) {
	t.Helper()
	root := t.TempDir()
	realDir = filepath.Join(root, "real city")
	if err := os.Mkdir(realDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realDir = pathutil.NormalizePathForCompare(realDir)
	alias = filepath.Join(root, "city alias")
	if err := os.Symlink(realDir, alias); err != nil {
		t.Skipf("symlinks unsupported on this filesystem: %v", err)
	}
	return realDir, alias
}

func TestWorkStoreIdentityCanonicalizesMissingScope(t *testing.T) {
	for _, tail := range []string{"", filepath.Join("not yet", "仓库")} {
		name := "existing scope"
		if tail != "" {
			name = "missing scope beneath symlinked ancestor"
		}
		t.Run(name, func(t *testing.T) {
			realDir, alias := canonicalIngestLayout(t)
			want := filepath.Join(realDir, tail)
			id, err := readWorkStoreIdentity(filepath.Join(alias, tail))
			if err != nil {
				t.Fatalf("readWorkStoreIdentity: %v", err)
			}
			if id.Path != want {
				t.Fatalf("scope identity path = %q, want canonical ingest path %q", id.Path, want)
			}
		})
	}
}

func TestWorkStoreIdentityCanonicalizesWithoutCreatingScope(t *testing.T) {
	realDir, alias := canonicalIngestLayout(t)
	missing := filepath.Join(alias, "not yet", "store")
	if _, err := readWorkStoreIdentity(missing); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(realDir, "not yet")); !os.IsNotExist(err) {
		t.Fatalf("identity read created its missing scope, or could not prove absence: %v", err)
	}
}

func TestCanonicalStoreIdentityPreservesEmptyPath(t *testing.T) {
	if got := canonicalIdentityPath(""); got != "" {
		t.Fatalf("empty optional identity became %q", got)
	}
}

func TestHookClaimMissingPathsKeepCanonicalAncestry(t *testing.T) {
	for _, tc := range []struct {
		name        string
		childAlias  bool
		parentAlias bool
		child       string
		parent      string
		outside     bool
	}{
		{"missing child through alias", true, false, "store/not-yet/child", "store", false},
		{"existing parent through alias", true, true, "store/not-yet/child", "store", false},
		{"missing parent through alias", false, true, "not-yet/store/child", "not-yet/store", false},
		{"equal missing scope", true, false, "not-yet/store", "not-yet/store", false},
		{"outside prefix sibling", true, false, "storehouse/not-yet", "store", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			realDir, alias := canonicalIngestLayout(t)
			if err := os.Mkdir(filepath.Join(realDir, "store"), 0o755); err != nil {
				t.Fatal(err)
			}
			childBase, parentBase := realDir, realDir
			if tc.childAlias {
				childBase = alias
			}
			if tc.parentAlias {
				parentBase = alias
			}
			child := filepath.Join(childBase, filepath.FromSlash(tc.child))
			parent := filepath.Join(parentBase, filepath.FromSlash(tc.parent))
			if got := hookClaimPathOutside(child, parent); got != tc.outside {
				t.Fatalf("outside(%q, %q) = %t, want %t", child, parent, got, tc.outside)
			}
		})
	}
}

func TestHookClaimOutsideEvidenceStillRejectsUnanchoredPaths(t *testing.T) {
	root := t.TempDir()
	parent := filepath.Join(root, "store")
	for _, tc := range []struct {
		name   string
		child  string
		parent string
	}{
		{"relative child", "worker", parent},
		{"relative parent", filepath.Join(root, "worker"), "store"},
		{"empty child", "", parent},
		{"empty parent", filepath.Join(root, "worker"), ""},
		// Concatenate: Join would remove the very component under test.
		{"dot-dot child", parent + string(filepath.Separator) + ".." + string(filepath.Separator) + "worker", parent},
		{"dot-dot parent", filepath.Join(root, "worker"), parent + string(filepath.Separator) + ".."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if hookClaimPathOutside(tc.child, tc.parent) {
				t.Fatalf("unanchored path pair (%q, %q) became evidence of an outside checkout", tc.child, tc.parent)
			}
		})
	}
}

func TestSupervisorExecutableIdentityCanonicalizesMissingLeaf(t *testing.T) {
	for _, tail := range []string{"gc", filepath.Join("not yet", "bin", "gc")} {
		t.Run(filepath.ToSlash(tail), func(t *testing.T) {
			realDir, alias := canonicalIngestLayout(t)
			if tail == "gc" {
				if err := os.WriteFile(filepath.Join(realDir, tail), []byte("binary identity fixture"), 0o755); err != nil {
					t.Fatal(err)
				}
			}
			want := filepath.Join(realDir, tail)
			if got := resolveExecutablePath(filepath.Join(alias, tail)); got != want {
				t.Fatalf("executable identity = %q, want canonical ingest path %q", got, want)
			}
		})
	}
}

func TestSupervisorExecutableIdentityPreservesUnknownPath(t *testing.T) {
	if got := resolveExecutablePath(""); got != "" {
		t.Fatalf("unknown executable became %q", got)
	}
}
