package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAuthoritativeDocsUseSingularOverlayDirectory(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))

	var docs []string
	for _, dir := range []string{
		filepath.Join(repoRoot, "docs", "guides"),
		filepath.Join(repoRoot, "docs", "tutorials"),
		filepath.Join(repoRoot, "engdocs", "design", "packv2"),
	} {
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return nil
			}
			switch filepath.Ext(path) {
			case ".md", ".mdx":
			default:
				return nil
			}
			if filepath.Base(path) == "doc-consistency-audit.md" {
				return nil
			}
			docs = append(docs, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", dir, err)
		}
	}

	var hits []string
	for _, path := range docs {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for lineNo, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(line, "overlays/") {
				continue
			}
			if allowedPluralOverlayLine(line) {
				continue
			}
			rel, err := filepath.Rel(repoRoot, path)
			if err != nil {
				rel = path
			}
			hits = append(hits, fmt.Sprintf("%s:%d: %s", rel, lineNo+1, line))
		}
	}
	if len(hits) > 0 {
		t.Fatalf("authoritative docs must not present `overlays/` as a directory convention; use `overlay/` instead. Hits:\n%s",
			strings.Join(hits, "\n"))
	}
}

func allowedPluralOverlayLine(line string) bool {
	if strings.Contains(line, `overlay_dir = "overlays/`) {
		withoutLegacySource := strings.Replace(line, `overlay_dir = "overlays/`, "", 1)
		return !strings.Contains(withoutLegacySource, "`overlays/`")
	}
	return false
}

func TestAllowedPluralOverlayLineRejectsCanonicalDestination(t *testing.T) {
	legacySourceToSingularDestination := "| Pack-wide overlays | `overlay_dir = \"overlays/default\"` | `overlay/` directory |"
	if !allowedPluralOverlayLine(legacySourceToSingularDestination) {
		t.Fatalf("legacy overlay_dir source should be allowed when destination is singular: %s", legacySourceToSingularDestination)
	}

	legacySourceToPluralDestination := "| Pack-wide overlays | `overlay_dir = \"overlays/default\"` | `overlays/` directory |"
	if allowedPluralOverlayLine(legacySourceToPluralDestination) {
		t.Fatalf("legacy overlay_dir source must not allow canonical plural destination: %s", legacySourceToPluralDestination)
	}
}
