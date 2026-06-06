package packlint

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoDolt3307FallbackInScripts guards ga-lsois: the silent
// :=3307 / :-3307 fallback was deleted from pack scripts and formulas
// because nothing listens on 3307 in any gc-managed city. The lint
// fires the build if a regression re-introduces a literal 3307 in the
// authoritative bundled pack sources, except for the explicit allowlist
// below (which slice 2 of ga-lsois — bead ga-nptxjv — will shrink).
func TestNoDolt3307FallbackInScripts(t *testing.T) {
	root := repoRoot()
	packDirs := []string{
		filepath.Join(root, "examples", "dolt"),
		filepath.Join(root, "examples", "gastown", "packs", "maintenance"),
	}

	// Formula literals are owned by slice 2 (ga-nptxjv). When slice 2 lands,
	// this list shrinks to nil and this comment is removed.
	allowlist := map[string]struct{}{
		filepath.Join(root, "examples", "gastown", "packs", "maintenance", "formulas", "mol-dog-jsonl.toml"):  {},
		filepath.Join(root, "examples", "gastown", "packs", "maintenance", "formulas", "mol-dog-reaper.toml"): {},
	}

	// Matches GC_DOLT_PORT.*3307 on a single line in source files. The
	// regex is deliberately broad: it catches `:-3307`, `:=3307`,
	// `default = "3307"` on GC_DOLT_PORT lines, and any future variation
	// operators might re-introduce. False positives are easier to
	// allowlist than false negatives are to catch.
	re := regexp.MustCompile(`GC_DOLT_PORT.*3307`)

	var hits []string
	for _, packsDir := range packDirs {
		err := filepath.WalkDir(packsDir, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if ext != ".sh" && ext != ".toml" && ext != ".md" {
				return nil
			}
			if _, ok := allowlist[path]; ok {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return fmt.Errorf("reading %s: %w", path, err)
			}
			for lineNo, line := range strings.Split(string(data), "\n") {
				if re.MatchString(line) {
					rel, _ := filepath.Rel(root, path)
					hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, lineNo+1, strings.TrimSpace(line)))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", packsDir, err)
		}
	}

	if len(hits) > 0 {
		t.Fatalf("found %d disallowed GC_DOLT_PORT/3307 fallback(s); ga-lsois removed this fallback. Re-introduce only by allowlisting in this test (and explaining why):\n  %s",
			len(hits), strings.Join(hits, "\n  "))
	}
}
