// Package packlint verifies mechanical invariants across shipped packs and
// example shell snippets.
//
// TestBdShowJqScalarExpect guards issue #810: bd show --json returns a JSON
// array ([{...}]), so scalar-expect filters like `jq -r '.metadata.X'`
// silently yield empty strings. The correct form prefixes `.[0].` or handles
// both shapes with an `if type == "array"` conditional.
package packlint

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func repoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..", "..")
}

// badScalarExtractRE matches a `bd show ... --json ... | jq[...] '.<lowercase-field>`
// pattern where the field is read directly off the top-level value. Such reads
// fail silently on `bd show --json` because the output is always an array.
//
// Safe forms we explicitly allow elsewhere and do NOT match:
//   - `.[0].field` or `.[0]` (explicit index)
//   - `if type == "array" then ... else ... end` (defensive conditional)
//   - wrapper functions like `jq_bead` that normalize shape internally
//
// Known limit: patterns with an intermediary command between `bd show` and
// `jq` (`bd show --json | tee /tmp/out | jq '.field'`) are not caught. That
// shape does not occur in the current codebase.
var badScalarExtractRE = regexp.MustCompile(`bd show\b[^|]*--json[^|]*\|[^|']*jq\b[^']*'\.[a-zA-Z_]`)

// scanDirs is the set of repo-root-relative directories whose shell snippets
// and formula descriptions must use the correct array-aware jq pattern.
var scanDirs = []string{
	"examples",
	"internal/bootstrap/packs",
}

// scanExts limits walking to files that ship embedded shell text.
var scanExts = map[string]bool{
	".toml": true,
	".md":   true,
	".sh":   true,
}

func TestBdShowJqScalarExpect(t *testing.T) {
	root := repoRoot()
	var violations []string
	for _, dir := range scanDirs {
		abs := filepath.Join(root, dir)
		err := filepath.WalkDir(abs, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			if !scanExts[filepath.Ext(path)] {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			for lineNum, line := range strings.Split(string(data), "\n") {
				if !badScalarExtractRE.MatchString(line) {
					continue
				}
				rel, _ := filepath.Rel(root, path)
				violations = append(violations, rel+":"+strconv.Itoa(lineNum+1)+": "+strings.TrimSpace(line))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}
	if len(violations) > 0 {
		t.Errorf("bd show --json pipelines use scalar-expect jq filter without `.[0].` prefix (see issue #810).\n"+
			"bd show --json returns a JSON array; `jq -r '.metadata.X'` silently returns empty.\n"+
			"Fix: change `jq -r '.field'` to `jq -r '.[0].field'`.\n\n%s",
			strings.Join(violations, "\n"))
	}
}
