package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Regression for gastownhall/gascity#784:
// The pack migration guide claimed `overlays/` (plural) was the canonical
// pack-wide overlay directory, but the loader (ExpandPacks in pack.go and
// DiscoverPackAgents in agent_discovery.go) only reads `overlay/` (singular).
// Students following the guide created a directory the loader ignores, with
// silent failure.
//
// Guard against the guide re-diverging: any backtick-quoted reference to the
// directory name must use the singular form that the loader actually reads.
func TestMigrationGuide_Regression784_UsesSingularOverlayDirectory(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(thisFile), "..", ".."))
	guidePath := filepath.Join(repoRoot, "docs", "guides", "migrating-to-pack-vnext.md")

	data, err := os.ReadFile(guidePath)
	if err != nil {
		t.Fatalf("reading %s: %v", guidePath, err)
	}

	// The loader reads `overlay/` (singular) for the pack-wide bucket; see
	// internal/config/pack.go ExpandPacks and internal/config/agent_discovery.go
	// DiscoverPackAgents. The guide may mention `overlays/` (plural) when
	// explaining the common typo, but must never describe it as canonical.
	text := string(data)
	forbidden := []string{
		"top-level `overlays/`",     // former canonical-instruction wording
		"pack-wide `overlays/`",     // cross-reference in migration tables
		"`overlays/` for pack-wide", // the "use overlays/ for pack-wide" lie
		"Keep as top-level `overlays/`",
		"Keep as top-level  `overlays/`",
	}
	var hits []string
	for _, phrase := range forbidden {
		if strings.Contains(text, phrase) {
			hits = append(hits, phrase)
		}
	}
	if len(hits) > 0 {
		t.Fatalf("%s still describes `overlays/` (plural) as canonical via these phrases: %v\nThe loader only reads `overlay/` (singular); the guide must match. See gastownhall/gascity#784.",
			guidePath, hits)
	}

	// Guard: the guide must clearly state which form the loader actually reads,
	// so readers following the skew-warning are pointed at the right answer.
	if !strings.Contains(text, "loader only discovers `overlay/`") {
		t.Fatalf("%s must state `loader only discovers \\`overlay/\\`` so readers know which form is canonical. See gastownhall/gascity#784.", guidePath)
	}
}

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
		filepath.Join(repoRoot, "docs", "packv2"),
	} {
		err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() || filepath.Ext(path) != ".md" {
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

	migrationGuide := filepath.Join(repoRoot, "docs", "guides", "migrating-to-pack-vnext.md")
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
			if allowedPluralOverlayLine(path, migrationGuide, line) {
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

func allowedPluralOverlayLine(path, migrationGuide, line string) bool {
	if strings.Contains(line, `overlay_dir = "overlays/`) {
		withoutLegacySource := strings.Replace(line, `overlay_dir = "overlays/`, "", 1)
		return !strings.Contains(withoutLegacySource, "`overlays/`")
	}
	return path == migrationGuide &&
		(strings.Contains(line, "`overlays/` (plural) is silently ignored") ||
			strings.Contains(line, "Rename to `overlay/`"))
}

func TestAllowedPluralOverlayLineRejectsCanonicalDestination(t *testing.T) {
	migrationGuide := filepath.Join("docs", "guides", "migrating-to-pack-vnext.md")
	packDoc := filepath.Join("docs", "packv2", "doc-pack-v2.md")

	legacySourceToSingularDestination := "| Pack-wide overlays | `overlay_dir = \"overlays/default\"` | `overlay/` directory |"
	if !allowedPluralOverlayLine(packDoc, migrationGuide, legacySourceToSingularDestination) {
		t.Fatalf("legacy overlay_dir source should be allowed when destination is singular: %s", legacySourceToSingularDestination)
	}

	legacySourceToPluralDestination := "| Pack-wide overlays | `overlay_dir = \"overlays/default\"` | `overlays/` directory |"
	if allowedPluralOverlayLine(packDoc, migrationGuide, legacySourceToPluralDestination) {
		t.Fatalf("legacy overlay_dir source must not allow canonical plural destination: %s", legacySourceToPluralDestination)
	}
}
