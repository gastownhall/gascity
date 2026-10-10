package apierr

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// This guard scans every non-test Go file in the module. It lives with the
// registry rather than in internal/api so that, under Bazel, an edit to any
// Go file re-runs this small target instead of all of //internal/api:api_test.

// urnLiteralRe matches any Gas City error-type URN literal as it would appear in
// source — the prefix plus whatever follows up to the closing string delimiter
// (quote, whitespace, or backtick). It intentionally does NOT constrain the tail
// to kebab-case: a malformed or mis-cased code (e.g. "...:Rogue", "...:2fa") can
// never be registered, so requiring the tail to look well-formed to be seen would
// make the guard silently ignore exactly the typos it exists to catch. A bare
// prefix (empty tail) matches too and fails LookupURN, so a literal
// "urn:gascity:error:" concatenated with a code is caught as well.
var urnLiteralRe = regexp.MustCompile("urn:gascity:error:[^\"\\s`]*")

// TestEveryEmittedErrorCodeIsRegistered is the error-contract analog of
// TestEveryKnownEventTypeHasRegisteredPayload: it guarantees the API cannot ship
// a problem-type URN the registry doesn't know about. Every urn:gascity:error:<x>
// string literal in non-test Go anywhere in the module (internal/, cmd/, pkg/,
// root, …) must resolve via LookupURN, and the apierr package is the sole
// place allowed to author a URN literal — every other site must mint errors
// through the catalog constructors (which derive the URN from the registry) so
// the type can never drift from a registered code.
func TestEveryEmittedErrorCodeIsRegistered(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := bazeltest.OverrideRoot()
	if repoRoot == "" {
		repoRoot = filepath.Join(filepath.Dir(currentFile), "..", "..", "..")
	}

	// Scan git-tracked Go source, not a filesystem walk. A raw WalkDir over
	// repoRoot descends into nested Gas City runtime state — checked-out worktrees
	// under .gc/ and .worktrees/ — whose historical copies of shipped files
	// legitimately carry pre-migration raw URN literals, so the guard would fail on
	// stale non-shipped source instead of on what the module actually ships.
	// `git ls-files` is the precise definition of shipped source: it excludes
	// untracked worktrees and build output while still seeing every tracked .go
	// under internal/, cmd/, pkg/, the module root, examples/, and so on.
	var tracked []string
	out, err := exec.Command("git", "-C", repoRoot, "ls-files", "-z", "--", "*.go").Output()
	if err == nil {
		tracked = strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	} else {
		// Bazel runfiles trees carry no .git; walk the declared source tree.
		tracked = apiErrWalkedGoFiles(repoRoot)
	}
	for _, rel := range tracked {
		if rel == "" || strings.HasSuffix(rel, "_test.go") {
			continue
		}
		// The apierr package is the registry itself: it authors the URN prefix and
		// (in its own docs) sample URNs. It is the one sanctioned definer. Anchor the
		// exact package path at the module root so an unrelated ".../api/apierr/..."
		// directory elsewhere is not accidentally exempted.
		if strings.HasPrefix(filepath.ToSlash(rel), "internal/api/apierr/") {
			continue
		}
		path := filepath.Join(repoRoot, rel)
		data, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		for _, urn := range urnLiteralRe.FindAllString(string(data), -1) {
			if _, ok := LookupURN(urn); !ok {
				t.Errorf("%s contains unregistered error URN %q — register it in internal/api/apierr/catalog.go or mint it through the catalog constructors", path, urn)
			} else {
				t.Errorf("%s authors a raw error URN literal %q — mint the error through the apierr catalog constructor instead so the URN derives from the registry", path, urn)
			}
		}
	}
}

// apiErrWalkedGoFiles enumerates non-test .go files across the module when
// the git index is unavailable (bazel runfiles trees).
func apiErrWalkedGoFiles(root string) []string {
	var files []string
	for _, top := range []string{"internal", "cmd", "pkg", "examples", "test", "scripts"} {
		_ = filepath.WalkDir(filepath.Join(root, top), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return nil // best effort
			}
			if d.IsDir() {
				if name := d.Name(); name != "." && strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				return nil
			}
			if strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				files = append(files, rel)
			}
			return nil
		})
	}
	return files
}
