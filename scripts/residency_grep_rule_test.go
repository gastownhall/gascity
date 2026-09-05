package scripts_test

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Rule (a-d): the GREP census.
//
// It counts the store-enumeration vocabulary declared in
// scripts/residency-boundary-patterns.txt, per (file, enclosing function,
// pattern), and ratchets those counts against the shared baseline.
// scripts/check-residency-boundary.sh is the shell rendering of the same rule
// over the same pattern file; residency_halves_agree_test.go pins that the two
// police the same tree with the same exemptions.
//
// See residency_boundary_test.go for the contract this rule is one quarter of.

type residencyPattern struct {
	name  string
	regex *regexp.Regexp
}

// loadResidencyPatterns reads the forbidden vocabulary from the file the shell
// guard reads. One source of truth: two hand-kept copies of "what is forbidden"
// would be this guard's own bug class, one level up.
func loadResidencyPatterns(t *testing.T, dir string) []residencyPattern {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, "residency-boundary-patterns.txt"))
	if err != nil {
		t.Fatalf("opening the pattern file: %v", err)
	}
	defer f.Close() //nolint:errcheck // read-only

	var out []residencyPattern
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, expr, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("pattern row %q is not name<TAB>regex", line)
		}
		re, err := regexp.Compile(expr)
		if err != nil {
			t.Fatalf("pattern %q: %v", name, err)
		}
		out = append(out, residencyPattern{name: name, regex: re})
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("reading the pattern file: %v", err)
	}
	if len(out) == 0 {
		t.Fatal("the pattern file declares no pattern; the guard is evaluating nothing")
	}
	return out
}

// enclosingFuncRe and topLevelCloseRe implement the enclosing-function rule the
// shell guard's awk pass implements, line for line. gofmt guarantees a
// top-level declaration starts at column 0 and only a top-level body closes
// with a `}` at column 0, so a two-line state machine attributes every line to
// its top-level function — a closure's hits belong to the function containing
// it — or to (file-scope). `make fmt-check` is what keeps that guarantee true.
//
// The rule is textual rather than go/ast on purpose: the shell half cannot
// parse Go, and a guard whose two halves key their baseline differently is the
// drift this whole lane exists to prevent. Exact agreement with go/ast is not
// required; exact agreement WITH EACH OTHER is, and both halves scan the real
// tree against the one baseline, so any divergence fails one of them.
var (
	enclosingFuncRe = regexp.MustCompile(`^func[ \t]+(?:\([^)]*\)[ \t]*)?([A-Za-z0-9_]+)`)
	topLevelCloseRe = regexp.MustCompile(`^\}`)
	commentLineRe   = regexp.MustCompile(`^[ \t]*(//|\*|/\*)`)
)

const residencyFileScope = "(file-scope)"

// scanResidencyGrepSites counts, per (path, enclosing function, pattern), the
// non-test source LINES carrying the forbidden vocabulary.
//
// The enclosing function is part of the key because a count keyed by file alone
// is MASKABLE: delete one call and add a different one in another function of
// the same file, and the count is level. Family (a) — the bulk of the baseline
// — is consumption-shaped, so the signature-level AST half cannot see that swap
// either. The residual, stated honestly, is a swap within one function.
func scanResidencyGrepSites(root string, dirs []string, allowlist map[string]bool, patterns []residencyPattern) (map[string]int, error) {
	found := map[string]int{}
	for _, dir := range dirs {
		abs := filepath.Join(root, filepath.FromSlash(dir))
		err := filepath.WalkDir(abs, func(path string, entry os.DirEntry, err error) error {
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			name := entry.Name()
			if entry.IsDir() {
				if path != abs && residencyPruned(name) {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)
			if allowlist[rel] {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			fn := residencyFileScope
			for _, line := range strings.Split(string(data), "\n") {
				if m := enclosingFuncRe.FindStringSubmatch(line); m != nil {
					fn = m[1]
				} else if topLevelCloseRe.MatchString(line) {
					fn = residencyFileScope
				}
				if commentLineRe.MatchString(line) || strings.Contains(line, residencyAllowMarker) {
					continue
				}
				for _, p := range patterns {
					if p.regex.MatchString(line) {
						found[rel+"\t"+fn+"\t"+p.name]++
					}
				}
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return found, nil
}

// TestResidencyBoundaryGrepRatchet is the CI-visible grep half: no new
// store-enumeration site, and no baseline entry the tree no longer reaches.
func TestResidencyBoundaryGrepRatchet(t *testing.T) {
	root := repoRoot(t)
	patterns := loadResidencyPatterns(t, residencyScriptsDir(t))
	found, err := scanResidencyGrepSites(root, residencyScanDirs, residencyAllowlist, patterns)
	if err != nil {
		t.Fatalf("scanning: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the census found no enumeration site at all; the guard is evaluating nothing")
	}
	baseline, err := readResidencyBaseline(residencyBaselinePath(t), func(p string) bool { return !residencyIsASTPattern(p) })
	if err != nil {
		t.Fatalf("reading the baseline: %v", err)
	}
	if len(baseline) == 0 {
		t.Fatal("the baseline pins no grep row; the ratchet has no denominator")
	}
	assertRatchet(t, found, baseline)
}

// TestResidencyBoundaryGrepControls falsifies the grep half on real files.
func TestResidencyBoundaryGrepControls(t *testing.T) {
	patterns := loadResidencyPatterns(t, residencyScriptsDir(t))
	root := t.TempDir()
	writeResidencyFixture(t, root, "cmd/gc/pinned.go", "package main\n\nfunc a() { _ = BeadStores() }\n")
	base := map[string]int{"cmd/gc/pinned.go\ta\ta:BeadStores": 1}
	scan := func(t *testing.T, baseline map[string]int) []string {
		t.Helper()
		found, err := scanResidencyGrepSites(root, residencyScanDirs, nil, patterns)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return ratchetViolations(found, baseline)
	}

	t.Run("baselined site passes", func(t *testing.T) {
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("a baselined site was reported: %v", v)
		}
	})

	t.Run("new site fails", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", "package main\n\nfunc b() { _ = rigBeadStores() }\n")
		v := scan(t, base)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "eleventh.go") {
			t.Fatalf("the guard accepted a NEW enumeration site: %v", v)
		}
	})

	t.Run("marker suppresses", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go",
			"package main\n\nfunc b() { _ = rigBeadStores() } // "+residencyAllowMarker+" tested escape hatch\n")
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("the marker did not suppress the hit: %v", v)
		}
	})

	t.Run("a comment is not a site", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", "package main\n\n// b would call rigBeadStores() but only in prose.\nfunc b() {}\n")
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("prose was counted as a site: %v", v)
		}
	})

	t.Run("stale baseline forces a shrink", func(t *testing.T) {
		if err := os.Remove(filepath.Join(root, "cmd/gc/eleventh.go")); err != nil {
			t.Fatalf("remove: %v", err)
		}
		stale := map[string]int{"cmd/gc/pinned.go\ta\ta:BeadStores": 1, "cmd/gc/gone.go\tgone\ta:BeadStores": 1}
		v := scan(t, stale)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "gone.go") {
			t.Fatalf("the guard accepted a baseline entry the tree no longer reaches: %v", v)
		}
	})

	// The laundering wrapper. cmd_storage.go used to be allowlisted, and an
	// allowlist filters the file BEFORE counting — so a helper written there
	// re-exported the enumeration to callers anywhere in the tree and no half of
	// the guard saw it. Now the call is censused like any other.
	//
	// It scans with the REAL residencyAllowlist rather than the nil one the rows
	// above use: an allowlist that no longer holds cmd_storage.go is the whole
	// subject, and a nil allowlist would make this row green either way.
	t.Run("a wrapper in a formerly-allowlisted file is censused", func(t *testing.T) {
		wrapped := t.TempDir()
		writeResidencyFixture(t, wrapped, "cmd/gc/cmd_storage.go",
			"package main\n\nfunc launder() map[string]beads.Store { return BeadStores() }\n")
		// The exemption that survives: the fork-only router exists in no upstream
		// tree, so it can have no baseline row to be pinned by.
		writeResidencyFixture(t, wrapped, "cmd/gc/cmd_bd_topology.go",
			"package main\n\nfunc workAxis() { _ = BeadStores() }\n")
		found, err := scanResidencyGrepSites(wrapped, residencyScanDirs, residencyAllowlist, patterns)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		v := ratchetViolations(found, map[string]int{})
		joined := strings.Join(v, " ")
		if !strings.Contains(joined, "cmd_storage.go") {
			t.Errorf("a wrapper laundering the enumeration out of a formerly-exempt file was accepted: %v", v)
		}
		if strings.Contains(joined, "cmd_bd_topology.go") {
			t.Errorf("the fork-only work-axis router lost its exemption; it has no upstream baseline row to be pinned by: %v", v)
		}
		pinned := map[string]int{"cmd/gc/cmd_storage.go\tlaunder\ta:BeadStores": 1}
		if v := ratchetViolations(found, pinned); len(v) != 0 {
			t.Errorf("the same site, pinned, must pass — the exemption moved to the call, it did not vanish: %v", v)
		}
	})

	// The mask the file-keyed baseline let through: delete one call and add a
	// different one in ANOTHER function of the SAME file. The counts stay level
	// per file, and family (a) is consumption-shaped so no signature changes —
	// nothing but the enclosing-function key can see this.
	t.Run("a same-file swap into another function fails", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/swap.go",
			"package main\n\nfunc keeper() { _ = BeadStores() }\n\nfunc other() {}\n")
		swapBase := map[string]int{
			"cmd/gc/pinned.go\ta\ta:BeadStores":    1,
			"cmd/gc/swap.go\tkeeper\ta:BeadStores": 1,
		}
		if v := scan(t, swapBase); len(v) != 0 {
			t.Fatalf("the pre-swap fixture must be clean: %v", v)
		}
		writeResidencyFixture(t, root, "cmd/gc/swap.go",
			"package main\n\nfunc keeper() {}\n\nfunc other() { _ = BeadStores() }\n")
		v := scan(t, swapBase)
		if len(v) == 0 {
			t.Fatal("a new consumption site paired with a removal in the same file was MASKED; the file-keyed baseline is not enough")
		}
		joined := strings.Join(v, " ")
		if !strings.Contains(joined, "other") || !strings.Contains(joined, "keeper") {
			t.Fatalf("the violation must name both the new function and the retired one: %v", v)
		}
	})
}
