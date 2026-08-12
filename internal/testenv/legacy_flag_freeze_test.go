package testenv_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// legacyFlagNeedles are the identifiers of the legacy formula_v2 global-setter
// mechanism that migration S5 deletes: the two atomic-backed setter/getter pairs
// and the four internal/formulatest wrappers. This test FREEZES their footprint
// so nothing new couples to the legacy path between now and S5 (new code uses the
// daemon.formula_v2 rollout gate via rollout.ForTest). S5-T5 deletes this file
// together with cmd/gc/feature_flags.go and internal/formulatest/v2.go.
// The needles include the two unexported atomic backing vars so a DIRECT bypass
// of the setters (formulaV2Enabled.Store(...)) is frozen too. The scan is textual
// (regexp over file contents) — comments and string literals count, deliberately,
// because S5's cleanup is grep-shaped: a stale comment naming a deleted identifier
// is itself footprint. Resolve a comment-only hit by rewording the comment, not by
// adding a golden entry. The sanctioned propagators applyFeatureFlags /
// syncFeatureFlags are intentionally NOT needles — their call sites are the
// expected bridge until S5, and freezing all ~30 would be noise; their bodies are
// in the golden via the setters they call.
var legacyFlagNeedles = []string{
	"SetFormulaV2Enabled", "IsFormulaV2Enabled",
	"SetGraphApplyEnabled", "IsGraphApplyEnabled",
	"LockV2ForTest", "HoldV2ForTest", "SetV2ForTest", "EnableV2ForTest",
	"formulaV2Enabled", "graphApplyEnabled",
}

// legacyFlagTestFileCeilings freezes, per package directory, the number of TEST
// files coupled to the legacy mechanism. A ceiling (file count, robust to
// reformatting) rather than a golden because test files churn legitimately. It is
// a SOFT upper bound: it blocks NET growth of coupled test files per package, but
// (by design, to tolerate churn) does not stop swapping one coupled file for
// another. The exact production footprint is frozen by the golden above; the
// tight ratchet lands with the S5 migration that removes this test. A dir with
// coupled test files but no ceiling fails loudly.
var legacyFlagTestFileCeilings = map[string]int{
	"cmd/gc": 5,
	// internal/api absorbed a third and fourth coupled test file
	// (handler_formulas_test.go, huma_handlers_run_launch_test.go) via
	// mainline merges that predated this freeze; the ceiling blocks growth
	// beyond the inherited count.
	"internal/api":        4,
	"internal/bootstrap":  1,
	"internal/dispatch":   4,
	"internal/formula":    5,
	"internal/graphroute": 1,
	"internal/graphv2":    1,
	"internal/molecule":   2,
	"internal/sling":      1,
}

// TestLegacyFormulaV2MechanismFrozen pins the legacy formula_v2 mechanism's
// footprint: an exact golden of every NON-TEST reference (the set S5 deletes) and
// a per-package ceiling on coupled TEST files. Adding a production call site or a
// new coupled test package fails; both directions force a deliberate update (or
// the S5 migration).
func TestLegacyFormulaV2MechanismFrozen(t *testing.T) {
	root := repoRoot(t)
	matchers := make([]*regexp.Regexp, len(legacyFlagNeedles))
	for i, n := range legacyFlagNeedles {
		matchers[i] = regexp.MustCompile(`\b` + regexp.QuoteMeta(n) + `\b`)
	}

	var prod []string
	testFilesByDir := map[string]map[string]bool{}

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata/ holds uncompiled fixture .go files that go build never
			// sees; scanning them would inject phantom coupling (or silently defeat
			// the freeze if a fixture names a needle).
			if skipRepoLintDir(d.Name()) || d.Name() == "testdata" || (path != root && isNestedWorktreeRoot(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		// This test file lists the needles as string DATA, not as coupling to the
		// mechanism; exclude it (by exact path) so the freeze list isn't mistaken
		// for a call site.
		if rel == "internal/testenv/legacy_flag_freeze_test.go" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				return nil // a concurrent delete in the shared tree — skip, don't flake
			}
			return rerr
		}
		content := string(data)
		isTest := strings.HasSuffix(path, "_test.go")
		dir := filepath.ToSlash(filepath.Dir(rel))
		for i, m := range matchers {
			if !m.MatchString(content) {
				continue
			}
			if isTest {
				if testFilesByDir[dir] == nil {
					testFilesByDir[dir] = map[string]bool{}
				}
				testFilesByDir[dir][rel] = true
			} else {
				prod = append(prod, rel+": "+legacyFlagNeedles[i])
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	sort.Strings(prod)
	want := readBaselineGolden(t, filepath.Join("testdata", "legacy_flag_freeze.golden"))
	if !equalStringSlices(prod, want) {
		added, removed := diffStringSets(want, prod)
		t.Errorf("legacy formula_v2 PRODUCTION references changed vs testdata/legacy_flag_freeze.golden.\n"+
			"ADDED (do not couple new production code to the legacy mechanism — use the daemon.formula_v2 rollout gate):\n  %s\n"+
			"REMOVED (S5 migration in progress? update the golden):\n  %s",
			strings.Join(added, "\n  "), strings.Join(removed, "\n  "))
	}

	for dir, files := range testFilesByDir {
		ceil, known := legacyFlagTestFileCeilings[dir]
		if !known {
			t.Errorf("%s: %d test file(s) couple to the legacy formula_v2 mechanism but no frozen ceiling exists — use rollout.ForTest, or register a ceiling deliberately", dir, len(files))
			continue
		}
		if len(files) > ceil {
			t.Errorf("%s: %d test files reference the legacy mechanism, frozen ceiling is %d — new tests must use rollout.ForTest, not the legacy globals", dir, len(files), ceil)
		}
	}
}
