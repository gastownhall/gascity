package scripts_test

import (
	"bufio"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// The residency lookup contract's CI-visible enforcement.
//
// The contract has two halves and one baseline. The GREP half counts the
// store-enumeration vocabulary declared in residency-boundary-patterns.txt; the
// AST half sees what grep cannot — a function SIGNATURE that hands a caller a
// raw store list, whatever its body called to build it. Both ratchet against
// scripts/residency-boundary-baseline.txt, so the two can never disagree about
// what is pinned.
//
// scripts/check-residency-boundary.sh is the shell rendering of the grep half:
// it reads the SAME pattern file, is wired into `make check`, and proves its own
// bite through `--self-test`. This file is the rendering that runs in CI, since
// ./scripts is inside UNIT_COVER_PKGS_NONCMDGC (the route
// check_split_topology_rows_test.go established) — and it deliberately spawns no
// subprocess, because the test-resource census ratchets untagged subprocess call
// sites and a guard is not worth a debt row.
//
// Every control below is falsified with a REAL on-disk edit in a t.TempDir()
// tree, never an overlay: the guard reads files, and an in-memory overlay cannot
// prove a file-reading guard bites.

const residencyAllowMarker = "residency:allow"

// residencyScanDirs are the packages the lookup contract governs. They mirror
// scan_dirs in check-residency-boundary.sh; TestResidencyBoundaryHalvesAgree
// pins that they stay identical.
var residencyScanDirs = []string{"cmd/gc", "internal/api", "internal/sling", "internal/dispatch"}

// residencyAllowlist mirrors the shell guard's. It holds ONE file, and the
// reason it is not four more is the point of the exemption's own design: the
// allowlist filters a file BEFORE counting, so a helper written inside an
// exempt file can re-export the derivation to callers anywhere in the tree and
// no half of this guard ever sees it — the wrapper's body is not censused and
// the wrapper's own name is not vocabulary. The topology constructors and the
// migration tooling do have to enumerate; they are pinned as ordinary
// shrink-only baseline rows instead, which is exemption scoped to the CALL
// SITE rather than to the file around it.
//
// cmd_bd_topology.go stays exempt because it does not exist here: it is the
// fork-only work-axis router, overlaid downstream and orthogonal to class
// residency. It can have no upstream baseline row, so a baseline pin is not
// available to it and dropping the exemption would break the fork the day it
// overlays.
var residencyAllowlist = map[string]bool{
	"cmd/gc/cmd_bd_topology.go": true,
}

// The AST half's pattern names. Every one starts with `ast:`, which is the
// prefix the shell half skips — it cannot parse Go and must leave these rows to
// the Go half rather than read them as grep rows it will never find.
const (
	residencyASTPattern      = "ast:returns-store-list"
	residencyAliasPattern    = "ast:vocabulary-alias"
	residencyLegChainPattern = "ast:plan-leg-store-chain"
	residencyUncountedCall   = "ast:uncounted-call-spelling"
)

// residencyIsASTPattern must be a PREFIX test, not equality against one name.
// The grep half reads every row the predicate accepts and then requires the
// tree to reach it; a second `ast:` name compared by equality would be handed
// to the grep half, found zero times, and fail as stale — so the guard would
// refuse the very rows that close its own holes.
func residencyIsASTPattern(pattern string) bool { return strings.HasPrefix(pattern, "ast:") }

func residencyScriptsDir(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "scripts")
}

func residencyBaselinePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(residencyScriptsDir(t), "residency-boundary-baseline.txt")
}

// ---------------------------------------------------------------------------
// The grep half.

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

// TestResidencyBoundaryHalvesAgree pins that the shell rendering and this one
// police the same tree with the same exceptions.
//
// The strongest agreement proof is not here but in the baseline: its grep rows
// are GENERATED by the shell half's awk pass and ASSERTED by this half's Go
// loop, across every (file, function) key on the real tree. If the two
// enclosing-function state machines ever disagreed about one line, one of them
// would fail against the shared file. What is left to check is the scan scope
// and the allowlist, which live in each half separately.
//
// Both are compared as EXACT SETS, parsed out of the shell's own array
// literals rather than searched for anywhere in the file. A substring search
// over the whole script is the wrong test twice over: it passes on a path that
// appears only in a comment (this file's own prose names three retired
// exemptions), and — the hole that matters — it is one-directional, so the
// shell could exempt a file the Go half still polices and nothing would say so.
// An exemption is a hole in a guard; it must be identical on both sides or the
// weaker side is the real policy.
func TestResidencyBoundaryHalvesAgree(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(residencyScriptsDir(t), "check-residency-boundary.sh"))
	if err != nil {
		t.Fatalf("reading the shell guard: %v", err)
	}
	body := string(script)
	assertResidencySetsMatch(t, "scan dir", residencyShellArray(t, body, "scan_dirs"), residencyScanDirs)
	goAllowed := make([]string, 0, len(residencyAllowlist))
	for path := range residencyAllowlist {
		goAllowed = append(goAllowed, path)
	}
	assertResidencySetsMatch(t, "allowlist entry", residencyShellArray(t, body, "allowlist"), goAllowed)
	if !strings.Contains(body, "residency-boundary-patterns.txt") {
		t.Error("the shell guard does not read the shared pattern file")
	}
	if !strings.Contains(body, "--self-test") {
		t.Error("the shell guard has no --self-test; a guard nothing asserts against can be defanged silently")
	}
	// The pruned directories are the third exemption, and the same argument
	// applies: a tree one half censuses and the other skips is a tree where the
	// baseline cannot be generated by one and asserted by the other. They are
	// pinned by name rather than as a parsed set because the shell states them in
	// a `find -prune` expression, not an array — so this asserts the names appear
	// in the pruning expression itself, not merely somewhere in the file.
	prune, _, ok := strings.Cut(body, "-print0")
	if !ok {
		t.Fatal("the shell guard has no find ... -print0 census walk to check for prunes")
	}
	if _, prune, ok = strings.Cut(prune, "\tfind "); !ok {
		t.Fatal("the shell guard's census no longer starts with find; the prune comparison is stale")
	}
	for _, dir := range residencyPrunedDirs {
		if !strings.Contains(prune, "-name "+dir) {
			t.Errorf("the shell census does not prune %q but the Go scanners do; one half walks a tree the other cannot", dir)
		}
	}
}

// residencyPrunedDirs are the directory names every scanner skips.
//
// They are not Go source this repo owns. The vendored node_modules tree is not
// empty of Go either — an upstream package ships a .go fixture under it — so
// this is a correctness exclusion before it is a cost one: censusing it would
// let an `npm install` move this repo's baseline.
var residencyPrunedDirs = []string{"node_modules", "dist"}

func residencyPruned(name string) bool {
	for _, dir := range residencyPrunedDirs {
		if name == dir {
			return true
		}
	}
	return false
}

// residencyShellArray reads one `name=(...)` bash array literal out of the
// script. It fails the test rather than returning empty on a miss: an array it
// cannot find would silently reduce the set comparison above to "the shell
// exempts nothing", which is the agreement failure it exists to catch.
func residencyShellArray(t *testing.T, body, name string) []string {
	t.Helper()
	_, rest, ok := strings.Cut(body, "\n"+name+"=(")
	if !ok {
		t.Fatalf("the shell guard has no %s=( array; the halves can no longer be compared", name)
	}
	// The cut is on the FIRST `)`, not on a line-leading one, because the two
	// arrays are formatted differently: scan_dirs is a single line whose `)`
	// closes it, allowlist spans lines and closes at column 0. Both hold paths
	// and directory names, none of which can contain a parenthesis, so the
	// simple cut is exact for both — and a `#` field ends the scan, so a trailing
	// comment cannot be read as an entry.
	inner, _, ok := strings.Cut(rest, ")")
	if !ok {
		t.Fatalf("the shell guard's %s=( array is unterminated", name)
	}
	var out []string
	for _, f := range strings.Fields(inner) {
		if strings.HasPrefix(f, "#") {
			break
		}
		out = append(out, f)
	}
	return out
}

func assertResidencySetsMatch(t *testing.T, what string, shell, gohalf []string) {
	t.Helper()
	for _, d := range residencySetDifferences(what, shell, gohalf) {
		t.Error(d)
	}
}

// residencySetDifferences reports each way the two sets disagree, in both
// directions.
//
// It is separated from the assertion so its own controls can check the RESULT
// rather than driving a hand-made *testing.T to see whether it failed. A
// zero-valued T records an Errorf, but it is not running under tRunner, so the
// day this helper gains a Fatalf the control's goroutine would simply exit and
// the control would report something other than what it is testing.
func residencySetDifferences(what string, shell, gohalf []string) []string {
	in := func(xs []string, want string) bool {
		for _, x := range xs {
			if x == want {
				return true
			}
		}
		return false
	}
	var out []string
	for _, s := range shell {
		if !in(gohalf, s) {
			out = append(out, fmt.Sprintf("the shell guard has %s %q and this half does not; the weaker half is the real policy", what, s))
		}
	}
	for _, g := range gohalf {
		if !in(shell, g) {
			out = append(out, fmt.Sprintf("this half has %s %q and the shell guard does not; the weaker half is the real policy", what, g))
		}
	}
	return out
}

// TestResidencyHalvesAgreementControls proves the agreement check above is not
// fail-never. A set comparison that silently degrades to comparing nothing is
// the classic way a two-implementation guard drifts apart unnoticed.
func TestResidencyHalvesAgreementControls(t *testing.T) {
	const body = "\nscan_dirs=(cmd/gc internal/api)\n\nallowlist=(\n\tcmd/gc/one.go\n)\n"

	t.Run("the array parser reads both shapes", func(t *testing.T) {
		if got := residencyShellArray(t, body, "scan_dirs"); len(got) != 2 || got[0] != "cmd/gc" || got[1] != "internal/api" {
			t.Fatalf("single-line array: %v", got)
		}
		if got := residencyShellArray(t, body, "allowlist"); len(got) != 1 || got[0] != "cmd/gc/one.go" {
			t.Fatalf("multi-line array: %v", got)
		}
	})

	t.Run("an exemption only the shell holds is reported", func(t *testing.T) {
		d := residencySetDifferences("allowlist entry", []string{"a.go", "b.go"}, []string{"a.go"})
		if len(d) != 1 || !strings.Contains(d[0], `shell guard has allowlist entry "b.go"`) {
			t.Fatalf("the shell exempting a file this half still polices was accepted: %v", d)
		}
	})

	t.Run("an exemption only this half holds is reported", func(t *testing.T) {
		d := residencySetDifferences("allowlist entry", []string{"a.go"}, []string{"a.go", "b.go"})
		if len(d) != 1 || !strings.Contains(d[0], `this half has allowlist entry "b.go"`) {
			t.Fatalf("this half exempting a file the shell still polices was accepted: %v", d)
		}
	})

	t.Run("identical sets pass", func(t *testing.T) {
		if d := residencySetDifferences("allowlist entry", []string{"b.go", "a.go"}, []string{"a.go", "b.go"}); len(d) != 0 {
			t.Fatalf("equal sets in a different order must agree; the check is fail-always: %v", d)
		}
	})
}

// ---------------------------------------------------------------------------
// The AST half.

// TestResidencyResolverBoundary is the check grep cannot make: any non-test,
// non-allowlisted function in cmd/gc or internal/api whose SIGNATURE hands back
// a raw []beads.Store or map[string]beads.Store must be in the baseline or
// carry the marker.
//
// It closes the grep half's residual hole — deleting one baselined call and
// adding a different one of the same pattern in the same file keeps the counts
// level, but a new store-list constructor changes a signature.
//
// Set RESIDENCY_BASELINE_EMIT=1 to print the rows instead of checking them —
// the shrink workflow, same as the shell guard's --emit-baseline.
func TestResidencyResolverBoundary(t *testing.T) {
	root := repoRoot(t)
	found, err := scanStoreListSignatures(root, residencyScanDirs, residencyAllowlist)
	if err != nil {
		t.Fatalf("scanning for store-list signatures: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the AST scan found no store-list signature at all; the guard is evaluating nothing")
	}
	if os.Getenv("RESIDENCY_BASELINE_EMIT") == "1" {
		for _, key := range sortedKeys(found) {
			fmt.Printf("%s\t%d\n", key, found[key])
		}
		t.Skip("emitted AST baseline rows")
	}

	baseline, err := readResidencyBaseline(residencyBaselinePath(t), func(p string) bool { return p == residencyASTPattern })
	if err != nil {
		t.Fatalf("reading the baseline: %v", err)
	}
	if len(baseline) == 0 {
		t.Fatal("the baseline pins no AST row; the ratchet has no denominator")
	}
	assertRatchet(t, found, baseline)
}

// TestResidencyResolverBoundaryControls falsifies the AST guard with real
// on-disk files.
func TestResidencyResolverBoundaryControls(t *testing.T) {
	root := t.TempDir()
	writeResidencyFixture(t, root, "cmd/gc/pinned.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

func pinned() []beads.Store { return nil }
`)
	base := map[string]int{"cmd/gc/pinned.go\tpinned\t" + residencyASTPattern: 1}
	scan := func(t *testing.T, baseline map[string]int) []string {
		t.Helper()
		found, err := scanStoreListSignatures(root, residencyScanDirs, nil)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return ratchetViolations(found, baseline)
	}

	t.Run("baselined signature passes", func(t *testing.T) {
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("a baselined signature was reported: %v", v)
		}
	})

	t.Run("new signature fails", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

func eleventh() map[string]beads.Store { return nil }
`)
		v := scan(t, base)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "eleventh.go") {
			t.Fatalf("the AST guard accepted a NEW store-list signature: %v", v)
		}
	})

	t.Run("marker suppresses", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

// eleventh enumerates by definition.
// `+residencyAllowMarker+` migration tooling
func eleventh() map[string]beads.Store { return nil }
`)
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("the marker did not suppress the signature: %v", v)
		}
	})

	t.Run("a non-store list is not a signature", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", "package main\n\nfunc eleventh() []string { return nil }\n")
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("an unrelated slice return was reported: %v", v)
		}
	})

	// The two spellings that change the result type's TEXT without changing what
	// the caller is handed. Both were live holes: cmd_wait.go's dependency store
	// set used the first one and this rule had never seen it.
	t.Run("a local type name for the list is still the list", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

type storeSet []beads.Store

func eleventh() storeSet { return nil }
`)
		v := scan(t, base)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "eleventh") {
			t.Fatalf("a one-line type alias hid the store list from the signature rule: %v", v)
		}
	})

	t.Run("an alias declared in another package is not resolved", func(t *testing.T) {
		// The counterpart that must stay silent. The alias pass is scoped to the
		// declaring directory, so this states the rule's honest edge rather than
		// implying a cross-package resolution it does not do.
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", "package main\n\nfunc eleventh() otherpkg.StoreSet { return nil }\n")
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("a qualified type this scanner cannot resolve was reported: %v", v)
		}
	})

	t.Run("a package var holding the constructor is a signature", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

var eleventh = func() []beads.Store { return nil }
`)
		v := scan(t, base)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "eleventh") {
			t.Fatalf("a func literal on a package var is not a FuncDecl, and slipped the rule: %v", v)
		}
	})

	t.Run("a package var declared with the func type is a signature", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

var eleventh func() map[string]beads.Store
`)
		v := scan(t, base)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "eleventh") {
			t.Fatalf("a func-typed package var slipped the rule: %v", v)
		}
	})

	t.Run("a plain store var is not a signature", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

var eleventh []beads.Store
`)
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("a variable that IS a store list, rather than a function handing one out, was reported: %v", v)
		}
	})

	t.Run("a subpackage is not a hiding place", func(t *testing.T) {
		writeResidencyFixture(t, root, "internal/api/dashboardbff/nested.go", `package dashboardbff

import "github.com/gastownhall/gascity/internal/beads"

func nested() []beads.Store { return nil }
`)
		defer func() {
			if err := os.Remove(filepath.Join(root, "internal/api/dashboardbff/nested.go")); err != nil {
				t.Fatalf("remove: %v", err)
			}
		}()
		v := scan(t, base)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "dashboardbff/nested.go") {
			t.Fatalf("the AST guard accepted a store-list signature one directory down: %v", v)
		}
	})

	t.Run("stale baseline forces a shrink", func(t *testing.T) {
		if err := os.Remove(filepath.Join(root, "cmd/gc/eleventh.go")); err != nil {
			t.Fatalf("remove: %v", err)
		}
		stale := map[string]int{
			"cmd/gc/pinned.go\tpinned\t" + residencyASTPattern: 1,
			"cmd/gc/gone.go\tgone\t" + residencyASTPattern:     1,
		}
		v := scan(t, stale)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "gone.go") {
			t.Fatalf("the AST guard accepted a baseline entry the tree no longer reaches: %v", v)
		}
	})
}

// scanStoreListSignatures counts, per file, the non-test functions whose
// results include a raw []beads.Store or map[string]beads.Store.
//
// The walk RECURSES, because the grep half does (`find "${present[@]}" -type f
// -name '*.go'`). A flat read here left every subpackage of a governed
// directory — internal/api/dashboardbff, internal/api/genclient — outside this
// half entirely: a store-list constructor added there was caught only if its
// body also spelled the grep half's vocabulary, which a hand-rolled probe list
// need not do. The halves are supposed to cover the same tree by different
// means, not different trees.
// The rule reads the SPELLING of the result type, which two legal spellings can
// change without changing what is handed over. `type storeList []beads.Store`
// and then `func f() storeList` is a one-line evasion of a rule about handing
// callers a raw store list; so is hanging the same signature off a package
// `var` as a func literal, which is not a FuncDecl and so never reached the
// result loop at all. Both are closed below — the alias pass first, then the
// count pass — because a rule this cheap to sidestep is not a rule.
func scanStoreListSignatures(root string, dirs []string, allowlist map[string]bool) (map[string]int, error) {
	parsed, err := residencyParseTree(root, dirs)
	if err != nil {
		return nil, err
	}

	// Aliases are collected from EVERY file, including allowlisted ones. The
	// allowlist exempts a file from being COUNTED; letting it also hide a type
	// declaration would make it the one place to mint an invisible spelling for
	// the rest of the package.
	aliases := map[string]map[string]bool{}
	for _, pf := range parsed {
		for _, decl := range pf.file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name == nil || !isRawStoreList(ts.Type, nil) {
					continue
				}
				if aliases[pf.dir] == nil {
					aliases[pf.dir] = map[string]bool{}
				}
				aliases[pf.dir][ts.Name.Name] = true
			}
		}
	}

	found := map[string]int{}
	for _, pf := range parsed {
		if allowlist[pf.rel] {
			continue
		}
		local := aliases[pf.dir]
		for _, decl := range pf.file.Decls {
			switch typed := decl.(type) {
			case *ast.FuncDecl:
				if typed.Doc != nil && strings.Contains(typed.Doc.Text(), residencyAllowMarker) {
					continue
				}
				if residencyFuncTypeYieldsStoreList(typed.Type, local) {
					found[pf.rel+"\t"+typed.Name.Name+"\t"+residencyASTPattern]++
				}
			case *ast.GenDecl:
				if typed.Tok != token.VAR || (typed.Doc != nil && strings.Contains(typed.Doc.Text(), residencyAllowMarker)) {
					continue
				}
				for _, spec := range typed.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || (vs.Doc != nil && strings.Contains(vs.Doc.Text(), residencyAllowMarker)) {
						continue
					}
					for i, name := range vs.Names {
						if residencyValueSpecYieldsStoreList(vs, i, local) {
							found[pf.rel+"\t"+name.Name+"\t"+residencyASTPattern]++
						}
					}
				}
			}
		}
	}
	return found, nil
}

// residencyFuncTypeYieldsStoreList reports whether any result of ft is a raw
// store list, under the locally-declared aliases for its package.
func residencyFuncTypeYieldsStoreList(ft *ast.FuncType, aliases map[string]bool) bool {
	if ft == nil || ft.Results == nil {
		return false
	}
	for _, result := range ft.Results.List {
		if isRawStoreList(result.Type, aliases) {
			return true
		}
	}
	return false
}

// residencyValueSpecYieldsStoreList reports whether the i'th name of vs is a
// func value handing back a raw store list — `var f func() []beads.Store` by
// declared type, or `var f = func() []beads.Store { ... }` by literal.
func residencyValueSpecYieldsStoreList(vs *ast.ValueSpec, i int, aliases map[string]bool) bool {
	if ft, ok := vs.Type.(*ast.FuncType); ok && residencyFuncTypeYieldsStoreList(ft, aliases) {
		return true
	}
	if i >= len(vs.Values) {
		return false
	}
	lit, ok := residencyUnparen(vs.Values[i]).(*ast.FuncLit)
	return ok && residencyFuncTypeYieldsStoreList(lit.Type, aliases)
}

// isRawStoreList reports whether expr is []beads.Store, map[string]beads.Store
// or []storeref.Leg — the shapes that hand a caller a probe list it will read
// itself — or a local type name declared as one of those.
//
// The first two carry no leg order and no error policy at all. []storeref.Leg
// is the subtler one: a consumer that enumerated a plan through EachLeg and
// then RETURNS the bare legs has stripped the policy back off and handed the
// next caller the same unpoliced list, one function further from the resolver.
// The grep half ratchets the EachLeg call; this ratchets the shape it can be
// laundered into.
//
// aliases is nil when deciding whether a type DECLARATION is itself a store
// list, which is what stops `type a b; type b []beads.Store` from resolving
// through a chain the rule never claimed to follow. One hop is what a
// one-line evasion costs; a chain is a deliberate act with a paper trail.
func isRawStoreList(expr ast.Expr, aliases map[string]bool) bool {
	switch typed := expr.(type) {
	case *ast.ArrayType:
		return typed.Len == nil && (isBeadsStore(typed.Elt) || isStorerefLeg(typed.Elt))
	case *ast.MapType:
		key, ok := typed.Key.(*ast.Ident)
		return ok && key.Name == "string" && (isBeadsStore(typed.Value) || isStorerefLeg(typed.Value))
	case *ast.Ident:
		return aliases[typed.Name]
	default:
		return false
	}
}

// residencyParsedFile is one non-test Go file of the scanned tree.
type residencyParsedFile struct {
	rel  string // slash-separated, relative to the repo root
	dir  string // the file's directory, which is its package for alias purposes
	file *ast.File
}

// residencyParseTree walks and parses every non-test Go file under dirs.
//
// The walk RECURSES, because the grep half does (`find "${present[@]}" -type f
// -name '*.go'`). A flat read left every subpackage of a governed directory —
// internal/api/dashboardbff, internal/api/genclient — outside this half
// entirely: a store-list constructor added there was caught only if its body
// also spelled the grep half's vocabulary, which a hand-rolled probe list need
// not do. The halves are supposed to cover the same tree by different means,
// not different trees.
func residencyParseTree(root string, dirs []string) ([]residencyParsedFile, error) {
	var out []residencyParsedFile
	fset := token.NewFileSet()
	for _, dir := range dirs {
		abs := filepath.Join(root, filepath.FromSlash(dir))
		if _, err := os.Stat(abs); os.IsNotExist(err) {
			continue
		}
		walkErr := filepath.WalkDir(abs, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			name := entry.Name()
			if entry.IsDir() {
				// node_modules and the built dashboard bundle are not Go source
				// we own. The vendored tree is not empty of Go either — some
				// upstream package ships a .go test fixture — so this is a
				// correctness prune before it is a cost one: censusing it would
				// let a dependency bump move this repo's baseline. All four
				// scanners prune identically, which the halves-agree test pins.
				if path != abs && residencyPruned(name) {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				return nil
			}
			relPath, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(relPath)
			file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			out = append(out, residencyParsedFile{rel: rel, dir: filepath.Dir(path), file: file})
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	return out, nil
}

func isBeadsStore(expr ast.Expr) bool { return isQualifiedType(expr, "beads", "Store") }

func isStorerefLeg(expr ast.Expr) bool { return isQualifiedType(expr, "storeref", "Leg") }

func isQualifiedType(expr ast.Expr, pkg, name string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok || sel.Sel == nil || sel.Sel.Name != name {
		return false
	}
	ident, ok := sel.X.(*ast.Ident)
	return ok && ident.Name == pkg
}

// ---------------------------------------------------------------------------
// The AST expression rules: the two evasions that are invisible to a
// line-oriented census by construction.
//
// (a) FUNCTION-VALUE ALIASING. Every call-shaped grep row demands a literal `(`
//     after the name, so `f := routes.storeFor` followed by `f(c)` names
//     nothing the census knows: the derivation is fetched under one name and
//     performed under another. `(routes.storeFor)(c)` evades the same row for
//     the same reason — the character after the name is `)`.
//
// (d) LINE-SPLIT FIELD ACCESS. `c:plan-leg-store-access` is anchored to one
//     line, and gofmt preserves a chain broken across three, so `pl.` / `Leg.` /
//     `Store` walks past it.
//
// Both belong here rather than in the pattern file because grep is the wrong
// instrument for each: a value-position row would have to match a name NOT
// followed by `(`, which fires inside strings and, worse, cannot tell a
// reference from a declaration — and the tree has both (`rigBeadStores` is a
// parameter name, `BeadStores()` an interface method). The AST knows the
// difference and is layout-blind.

// residencyGuardedNames is the identifier vocabulary the alias rule watches,
// DERIVED from the shared pattern file rather than restated here.
//
// A second hand-kept list of names would be this guard's own bug class one
// level up — the pattern file's header says as much about the two grep halves —
// and it would fail in a specific way: a vocabulary row added to the file would
// be aliasable from the day it landed, because only the census that counts
// CALLS would have learned the name.
type residencyGuardedNames struct {
	bare         map[string]bool // BeadStores, cliSoleClassBinding, ...
	selector     map[string]bool // the selector half of a receiver-blind row like `\.storeFor\(`
	qualified    map[string]bool // "storeref.EachLeg", "routes.storeFor"
	qualifiedSel map[string]bool // "EachLeg" — the qualified rows' selector halves
	unreduced    []string        // call-shaped rows that did not reduce to an identifier
}

func (n residencyGuardedNames) empty() bool {
	return len(n.bare) == 0 && len(n.selector) == 0 && len(n.qualified) == 0
}

var (
	residencyOptionalGroupRe = regexp.MustCompile(`\(([A-Za-z0-9_]+)\)\?`)
	residencyIdentRe         = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// residencyDeriveGuardedNames reduces the call-shaped pattern rows to the
// identifiers whose call they count.
//
// A row that is NOT call-shaped is skipped deliberately, not silently: a
// `[]beads.Store{` literal and a `.Leg.Store` field access have no name to
// alias, and `d:NudgeQueueIDPrefix` already matches a bare mention because it
// has no call parenthesis in the first place. A row that IS call-shaped but does
// not reduce to an identifier is recorded in unreduced, because dropping it
// would be the fail-open this rule exists to prevent.
//
// CALL-SHAPEDNESS IS DECIDED BY CONTAINING `\(`, NOT BY ENDING IN IT. The two
// tests differ on rows nobody has written yet, which is exactly when it matters:
// `openLegacyStores\([^)]` or `ReservedPrefixFor\(ctx` are plainly call rows the
// grep census would count, and under a suffix test they would fall out of the
// alias vocabulary with no record — the pattern file's promise that a new row
// extends both censuses would quietly stop being true. Under a containment test
// they reach the reduction, fail it, and land in unreduced, where
// TestResidencyGuardedNameDerivation refuses them until the shape is handled.
func residencyDeriveGuardedNames(patterns []residencyPattern) residencyGuardedNames {
	names := residencyGuardedNames{
		bare:         map[string]bool{},
		selector:     map[string]bool{},
		qualified:    map[string]bool{},
		qualifiedSel: map[string]bool{},
	}
	for _, p := range patterns {
		expr := strings.TrimPrefix(p.regex.String(), `(^|[^A-Za-z0-9_])`)
		if !strings.Contains(expr, `\(`) {
			continue
		}
		switch {
		case strings.HasSuffix(expr, `\(\)`):
			expr = strings.TrimSuffix(expr, `\(\)`)
		case strings.HasSuffix(expr, `\(`):
			expr = strings.TrimSuffix(expr, `\(`)
		default:
			names.unreduced = append(names.unreduced, p.name)
			continue
		}
		for _, spelling := range residencyExpandOptional(expr) {
			spelling = strings.ReplaceAll(spelling, `\.`, ".")
			pkg, sel, qualified := strings.Cut(spelling, ".")
			switch {
			case qualified && pkg == "":
				// `\.storeFor\(` — matched on ANY receiver, so the alias rule
				// watches the selector wherever it is fetched from too.
				if residencyIdentRe.MatchString(sel) {
					names.selector[sel] = true
					continue
				}
			case qualified:
				if residencyIdentRe.MatchString(pkg) && residencyIdentRe.MatchString(sel) {
					names.qualified[spelling] = true
					names.qualifiedSel[sel] = true
					continue
				}
			default:
				if residencyIdentRe.MatchString(spelling) {
					names.bare[spelling] = true
					continue
				}
			}
			names.unreduced = append(names.unreduced, p.name)
		}
	}
	return names
}

// residencyExpandOptional spells out every alternative of an `(X)?` group, so
// `cliSoleClassBinding(Store)?` guards both spellings the grep row guards.
func residencyExpandOptional(expr string) []string {
	out := []string{expr}
	for {
		grew := false
		var next []string
		for _, candidate := range out {
			m := residencyOptionalGroupRe.FindStringSubmatchIndex(candidate)
			if m == nil {
				next = append(next, candidate)
				continue
			}
			grew = true
			next = append(next,
				candidate[:m[0]]+candidate[m[2]:m[3]]+candidate[m[1]:],
				candidate[:m[0]]+candidate[m[1]:])
		}
		out = next
		if !grew {
			return out
		}
	}
}

// residencyMarkerLines is the escape hatch for expression rules. The signature
// rule reads a function's DOC comment, which is the right granularity for a
// whole declaration; an expression needs the line it sits on, the same way the
// grep half reads it. A doc-comment marker therefore does not suppress an
// expression inside the function — suppressing every site in a body from one
// line above it is not a reviewable exemption.
func residencyMarkerLines(data []byte) map[int]bool {
	out := map[int]bool{}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, residencyAllowMarker) {
			out[i+1] = true
		}
	}
	return out
}

// residencyMarked reads ONE line: the one the decisive token sits on — the
// selector's `.Sel` for a selector, the identifier itself for an identifier.
//
// Scanning the node's whole Pos..End span instead would hand a multi-line
// expression an exemption its author never asked for. In
//
//	s := resolveBinding(
//		cityPath, // residency:allow reviewed for the hit on THIS line
//	).Leg.Store
//
// the marker was written and reviewed for the argument line, and a span check
// would silently extend it over the `.Leg.Store` chain three lines later. A
// marker is a reviewed exemption for a site; it has to name one line the way
// the grep half does, or it stops being reviewable.
func residencyMarked(fset *token.FileSet, markers map[int]bool, pos token.Pos) bool {
	return len(markers) != 0 && markers[fset.Position(pos).Line]
}

// residencyIsLegStoreChain reports whether sel is the `.Leg.Store` chain,
// however it is laid out across lines or parenthesised.
//
// The parentheses matter as much as the line breaks: gofmt does not strip a
// redundant `(pl.Leg).Store`, so it survives review looking like ordinary code
// while the text rule — which spells the chain `\.Leg\.Store` — reads `.Leg)`
// and sees nothing. Unwrapping is one loop; leaving it out would have made the
// docs beside this rule false on the day they were written.
func residencyIsLegStoreChain(sel *ast.SelectorExpr) bool {
	if sel.Sel == nil || sel.Sel.Name != "Store" {
		return false
	}
	inner, ok := residencyUnparen(sel.X).(*ast.SelectorExpr)
	return ok && inner.Sel != nil && inner.Sel.Name == "Leg"
}

// residencyGrepCouldNotCount reports whether sel is a CALL to guarded
// vocabulary written in a spelling the text census provably cannot match.
//
// Calls are normally grep's business and this rule stands down for them. Two
// spellings take that away, and both survive gofmt exactly as written:
//
//	routes.          storeref.          import sr ".../storeref"
//		storeFor(c)      EachLeg(p, fn)     sr.EachLeg(p, fn)
//
// The first two split the chain at the dot, so no single line holds the dot,
// the name and the parenthesis that every b/c row requires together. The third
// keeps them on one line but spells the package half something the row does not
// know — `storeref\.EachLeg\(` cannot match `sr.EachLeg(`. In all three the
// call happens, the resolver is bypassed, and both halves stay silent.
//
// This is the line-split evasion the bead names, applied to the call families
// rather than to `.Leg.Store`, plus the import-rename twin it shares a fix
// with. It needs no type resolution: the question is not what the receiver IS,
// it is whether the text census had the three tokens on one line under the
// spelling it was given.
func residencyGrepCouldNotCount(fset *token.FileSet, sel *ast.SelectorExpr, names residencyGuardedNames) bool {
	if sel.Sel == nil {
		return false
	}
	x, qualifiedByIdent := residencyUnparen(sel.X).(*ast.Ident)
	switch {
	case qualifiedByIdent && names.qualified[x.Name+"."+sel.Sel.Name]:
		// Spelled the way the row expects — grep counts it if it is on one line.
	case names.selector[sel.Sel.Name]:
		// A receiver-blind row: `\.storeFor\(` needs only the dot and the name.
	case names.qualifiedSel[sel.Sel.Name]:
		// A qualified row reached through some OTHER qualifier — a renamed
		// import, or a variable holding the package's own seam. No row can match
		// this line however it is laid out, so the split test below is moot.
		return true
	default:
		return false
	}
	return fset.Position(sel.X.End()).Line != fset.Position(sel.Sel.Pos()).Line
}

func residencyUnparen(expr ast.Expr) ast.Expr {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}
		expr = paren.X
	}
}

// scanResidencyASTExpressions counts, per (path, enclosing function, pattern),
// the alias and leg-chain sites in the tree the grep half scans.
//
// It covers all of residencyScanDirs, as every rule here now does: this one
// COMPLEMENTS grep on grep's own subject — the vocabulary — so scanning less
// than grep would leave the alias reachable exactly where the vocabulary is
// still counted.
//
// THE HONEST RESIDUAL: an access through an intermediate variable —
// `l := pl.Leg; use(l.Store)`, or `r := routes; r.storeFor(c)` — is invisible
// to an untyped AST, the same class of residual as the grep half's
// swap-within-one-function. Closing it needs go/types, and full type-checking
// cmd/gc on every commit costs more than the hole is worth.
func scanResidencyASTExpressions(root string, dirs []string, allowlist map[string]bool, names residencyGuardedNames) (map[string]int, error) {
	found := map[string]int{}
	fset := token.NewFileSet()
	for _, dir := range dirs {
		abs := filepath.Join(root, filepath.FromSlash(dir))
		if _, err := os.Stat(abs); os.IsNotExist(err) {
			continue
		}
		walkErr := filepath.WalkDir(abs, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
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
			relPath, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			rel := filepath.ToSlash(relPath)
			if allowlist[rel] {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			file, err := parser.ParseFile(fset, path, data, parser.ParseComments)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			countResidencyDeclExpressions(fset, file, rel, residencyMarkerLines(data), names, found)
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	return found, nil
}

// countResidencyDeclExpressions attributes hits to the enclosing top-level
// declaration, the same key the other two rules use.
//
// It is name-based, not type-resolved, so a local or parameter SPELLED like a
// guarded accessor is counted wherever it is read. That is deliberate rather
// than tolerated: a variable named rigBeadStores reads at its use sites exactly
// like a call to the enumerator it shadows, which is the confusion this whole
// vocabulary exists to make visible. The two such parameters in cmd/gc — six
// occurrences across the reaper and the agent-home sweep — were renamed rather
// than marked. Resolving them properly needs go/types on a
// package that does not compile in isolation here, and the cost of the name
// rule is one rename; the cost of go/types is a guard slow enough to be
// skipped.
func countResidencyDeclExpressions(fset *token.FileSet, file *ast.File, rel string, markers map[int]bool, names residencyGuardedNames, found map[string]int) {
	for _, decl := range file.Decls {
		fn := residencyFileScope
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name != nil {
			fn = fd.Name.Name
		}

		// A call's Fun is grep's business, not this rule's — but only when it is
		// the Fun DIRECTLY. `(routes.storeFor)(c)` puts a ParenExpr there, which
		// is what makes the grep row miss it, so the selector inside stays
		// countable. Sel idents and declaration names are excluded so a
		// selector is counted once, as a selector, and never as its own
		// declaration.
		callFuns := map[ast.Node]bool{}
		selIdents := map[*ast.Ident]bool{}
		declIdents := map[*ast.Ident]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch typed := n.(type) {
			case *ast.CallExpr:
				callFuns[typed.Fun] = true
			case *ast.SelectorExpr:
				if typed.Sel != nil {
					selIdents[typed.Sel] = true
				}
			case *ast.FuncDecl:
				if typed.Name != nil {
					declIdents[typed.Name] = true
				}
			case *ast.Field:
				for _, fieldName := range typed.Names {
					declIdents[fieldName] = true
				}
			}
			return true
		})

		count := func(pattern string) { found[rel+"\t"+fn+"\t"+pattern]++ }
		ast.Inspect(decl, func(n ast.Node) bool {
			switch typed := n.(type) {
			case *ast.SelectorExpr:
				if residencyMarked(fset, markers, typed.Sel.Pos()) {
					return true
				}
				if residencyIsLegStoreChain(typed) {
					count(residencyLegChainPattern)
					return true
				}
				if callFuns[ast.Node(typed)] {
					if residencyGrepCouldNotCount(fset, typed, names) {
						count(residencyUncountedCall)
					}
					return true
				}
				if x, ok := typed.X.(*ast.Ident); ok && names.qualified[x.Name+"."+typed.Sel.Name] {
					count(residencyAliasPattern)
					return true
				}
				if names.selector[typed.Sel.Name] || names.bare[typed.Sel.Name] || names.qualifiedSel[typed.Sel.Name] {
					count(residencyAliasPattern)
				}
			case *ast.Ident:
				if selIdents[typed] || declIdents[typed] || callFuns[ast.Node(typed)] {
					return true
				}
				if names.bare[typed.Name] && !residencyMarked(fset, markers, typed.Pos()) {
					count(residencyAliasPattern)
				}
			}
			return true
		})
	}
}

// TestResidencyGuardedNameDerivation pins the reduction from pattern rows to
// guarded identifiers.
//
// Without it a regex edit could empty the alias rule silently — the rule would
// still run, still find nothing, and still pass, which is the failure mode the
// derivation was chosen to avoid in the first place.
func TestResidencyGuardedNameDerivation(t *testing.T) {
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	if len(names.unreduced) != 0 {
		t.Fatalf("call-shaped pattern rows did not reduce to an identifier, so the alias rule does not guard them: %v", names.unreduced)
	}
	if names.empty() {
		t.Fatal("the derivation produced no guarded name; the alias rule is evaluating nothing")
	}
	for _, want := range []string{"BeadStores", "cliSoleClassBinding"} {
		if !names.bare[want] {
			t.Errorf("%q is grep vocabulary but not alias vocabulary", want)
		}
	}
	for _, want := range []string{"storeref.EachLeg", "storeref.ClassCandidates", "routes.storeFor"} {
		if !names.qualified[want] {
			t.Errorf("%q is grep vocabulary but not alias vocabulary", want)
		}
	}
	// The selector half of every qualified row is guarded on its own, because the
	// grep row names one package qualifier and an import can be renamed. storeFor
	// is the case that pays for itself today: the tree reaches it through
	// cs.storageRoutes, through cliStorageRoutes(cityPath), and through
	// graphStores, none of which the `routes.` row can match.
	for _, want := range []string{"EachLeg", "ClassCandidates", "storeFor"} {
		if !names.qualifiedSel[want] {
			t.Errorf("%q is not guarded on its own, so a renamed import spells the same call past both halves", want)
		}
	}

	t.Run("an unhandled call shape lands in unreduced rather than vanishing", func(t *testing.T) {
		// The fail-open this classification exists to prevent: a plainly
		// call-shaped row the reduction has no case for. It must be REPORTED,
		// not skipped like the deliberately non-call rows beside it.
		synthetic := []residencyPattern{
			{name: "x:mid-call", regex: regexp.MustCompile(`(^|[^A-Za-z0-9_])openLegacyStores\([^)]`)},
			{name: "x:no-call", regex: regexp.MustCompile(`\[\]beads\.Store\{`)},
		}
		got := residencyDeriveGuardedNames(synthetic)
		if len(got.unreduced) != 1 || got.unreduced[0] != "x:mid-call" {
			t.Fatalf("unreduced = %v, want exactly [x:mid-call]: a call-shaped row must be refused, a non-call row skipped", got.unreduced)
		}
		if !got.empty() {
			t.Error("the unhandled row was also reduced to a name; the classification is reporting and guarding the same row")
		}
	})
}

// TestResidencyUncountedCallControls falsifies ast:uncounted-call-spelling —
// the rule for calls the grep census cannot see.
//
// Every grep row for a method or qualified call keys on a dot, a name and an
// open parenthesis appearing TOGETHER ON ONE LINE. Two legal spellings break
// that up, and gofmt preserves both: writing the dot at the end of one line and
// the name at the start of the next, and renaming the import so the qualifier
// in the row never appears. Neither is exotic — gofmt produces the first
// itself on a long chain — and the alias rule deliberately stands down on a
// call's Fun, so before this rule both walked past both halves of the guard.
func TestResidencyUncountedCallControls(t *testing.T) {
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	count := func(t *testing.T, body string) int {
		t.Helper()
		root := t.TempDir()
		writeResidencyFixture(t, root, "cmd/gc/calls.go", body)
		found, err := scanResidencyASTExpressions(root, residencyScanDirs, nil, names)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return found["cmd/gc/calls.go\tuse\t"+residencyUncountedCall]
	}

	fires := []struct{ label, body string }{
		{
			"a receiver-blind selector split across the dot",
			"package main\n\nfunc use(routes cliStorageRoutes, c coordclass.Class) {\n\t_, _ = routes.\n\t\tstoreFor(c)\n}\n",
		},
		{
			"a qualified call split across the dot",
			"package main\n\nfunc use(p storeref.ResolvedPlan, fn func()) {\n\tstoreref.\n\t\tEachLeg(p, fn)\n}\n",
		},
		{
			"a renamed import spelling a qualified call",
			"package main\n\nfunc use(p storeref.ResolvedPlan, fn func()) {\n\tsr.EachLeg(p, fn)\n}\n",
		},
	}
	for _, tc := range fires {
		if got := count(t, tc.body); got != 1 {
			t.Errorf("%s: counted %d, want 1 — the call is invisible to the grep census and now to this rule too", tc.label, got)
		}
	}

	silent := []struct{ label, body string }{
		{
			"the same receiver-blind call on one line",
			"package main\n\nfunc use(routes cliStorageRoutes, c coordclass.Class) {\n\t_, _ = routes.storeFor(c)\n}\n",
		},
		{
			"the same qualified call on one line",
			"package main\n\nfunc use(p storeref.ResolvedPlan, fn func()) {\n\tstoreref.EachLeg(p, fn)\n}\n",
		},
		{
			"a split call on a name that is not vocabulary",
			"package main\n\nfunc use(routes cliStorageRoutes) {\n\t_ = routes.\n\t\tstoreForPoolAssignment()\n}\n",
		},
		{
			"a marked split call",
			"package main\n\nfunc use(routes cliStorageRoutes, c coordclass.Class) {\n\t_, _ = routes.\n\t\tstoreFor(c) // " + residencyAllowMarker + " tested escape hatch\n}\n",
		},
	}
	for _, tc := range silent {
		if got := count(t, tc.body); got != 0 {
			t.Errorf("%s: counted %d, want 0 — the grep census already sees this, so counting it here double-books the site", tc.label, got)
		}
	}
}

// TestResidencyMarkerIsLineScoped pins that `// residency:allow` exempts the
// LINE it sits on and nothing else.
//
// The marker used to be tested against the enclosing declaration's whole span,
// which silenced every site in a function because one line in it was annotated.
// An escape hatch that wide is not an escape hatch, it is an off switch, and it
// would be reached for exactly when a function holds one reviewed exception and
// several unreviewed ones.
func TestResidencyMarkerIsLineScoped(t *testing.T) {
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	root := t.TempDir()
	writeResidencyFixture(t, root, "cmd/gc/scope.go",
		"package main\n\nfunc use(pl storeref.PlanLeg, other storeref.PlanLeg) {\n"+
			"\t_ = pl.Leg.Store // "+residencyAllowMarker+" reviewed\n"+
			"\t_ = other.Leg.Store\n}\n")
	found, err := scanResidencyASTExpressions(root, residencyScanDirs, nil, names)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := found["cmd/gc/scope.go\tuse\t"+residencyLegChainPattern]; got != 1 {
		t.Fatalf("counted %d chains, want 1: the marked line must be exempt and the unmarked one beside it must not", got)
	}
}

// TestResidencyASTExpressionRatchet is the CI-visible half of the two
// expression rules: no new alias, no new leg chain, no stale pin.
//
// Set RESIDENCY_BASELINE_EMIT=1 to print the rows instead of checking them.
func TestResidencyASTExpressionRatchet(t *testing.T) {
	root := repoRoot(t)
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	if names.empty() {
		t.Fatal("the derivation produced no guarded name; the alias rule is evaluating nothing")
	}
	found, err := scanResidencyASTExpressions(root, residencyScanDirs, residencyAllowlist, names)
	if err != nil {
		t.Fatalf("scanning for aliased vocabulary: %v", err)
	}
	if os.Getenv("RESIDENCY_BASELINE_EMIT") == "1" {
		for _, key := range sortedKeys(found) {
			fmt.Printf("%s\t%d\n", key, found[key])
		}
		t.Skip("emitted AST expression baseline rows")
	}

	// All three patterns this scan emits, or a row it CAN produce has no
	// baseline to be compared against: the emitter would print it, the file
	// would hold it, and the ratchet would still read the census as growth
	// from zero.
	baseline, err := readResidencyBaseline(residencyBaselinePath(t), func(p string) bool {
		return p == residencyAliasPattern || p == residencyLegChainPattern || p == residencyUncountedCall
	})
	if err != nil {
		t.Fatalf("reading the baseline: %v", err)
	}
	// A ZERO census is the GOAL for every rule here, so unlike the grep and
	// signature ratchets this one must not fail closed on an empty baseline. The
	// denominator that proves it is evaluating something is names.empty() above —
	// the derived vocabulary — not the row count. Pinning the row count instead
	// would book a guaranteed deadlock: the expression baseline holds one row, and
	// retiring it is the migration this whole lane exists to drive, so success
	// would turn CI red.
	assertRatchet(t, found, baseline)
}

// TestResidencyVocabularyAliasControls falsifies the alias rule on real files:
// four ways to fetch the vocabulary without calling it must fire, and five
// shapes that merely look like it must not.
func TestResidencyVocabularyAliasControls(t *testing.T) {
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	scan := func(t *testing.T, root string) map[string]int {
		t.Helper()
		found, err := scanResidencyASTExpressions(root, residencyScanDirs, nil, names)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return found
	}
	fires := func(t *testing.T, label, body string) {
		t.Helper()
		root := t.TempDir()
		writeResidencyFixture(t, root, "cmd/gc/alias.go", body)
		if found := scan(t, root); found["cmd/gc/alias.go\tevade\t"+residencyAliasPattern] == 0 {
			t.Errorf("%s: the alias rule did not fire; the vocabulary was fetched under one name and called under another", label)
		}
	}
	silent := func(t *testing.T, label, body string) {
		t.Helper()
		root := t.TempDir()
		writeResidencyFixture(t, root, "cmd/gc/quiet.go", body)
		for key, n := range scan(t, root) {
			if strings.Contains(key, residencyAliasPattern) && n > 0 {
				t.Errorf("%s: the alias rule fired on a shape that is not an alias (%s)", label, strings.ReplaceAll(key, "\t", " "))
			}
		}
	}

	fires(t, "a method value", "package main\n\nfunc evade(routes cliStorageRoutes, c coordclass.Class) {\n\tf := routes.storeFor\n\t_, _ = f(c)\n}\n")
	fires(t, "a package-level function value", "package main\n\nfunc evade() {\n\tg := BeadStores\n\t_ = g()\n}\n")
	fires(t, "a qualified function value", "package main\n\nfunc evade(c coordclass.Class) {\n\th := storeref.ClassCandidates\n\t_ = h(c)\n}\n")
	fires(t, "a parenthesized call target", "package main\n\nfunc evade(routes cliStorageRoutes, c coordclass.Class) {\n\t_, _ = (routes.storeFor)(c)\n}\n")

	silent(t, "a plain call", "package main\n\nfunc quiet(routes cliStorageRoutes, c coordclass.Class) {\n\t_, _ = routes.storeFor(c)\n}\n")
	silent(t, "the accessor's own declaration", "package main\n\nfunc (r cliStorageRoutes) storeFor(c coordclass.Class) (beads.Store, bool) {\n\treturn nil, false\n}\n")
	silent(t, "an interface method", "package main\n\ntype apiState interface {\n\tBeadStores() map[string]beads.Store\n}\n")
	silent(t, "a distinct identifier", "package main\n\nfunc quiet() {\n\t_ = storeForPoolAssignment\n}\n")
	silent(t, "a marked line", "package main\n\nfunc quiet(routes cliStorageRoutes) {\n\tf := routes.storeFor // "+residencyAllowMarker+" tested escape hatch\n\t_ = f\n}\n")

	t.Run("a new alias is a ratchet violation", func(t *testing.T) {
		root := t.TempDir()
		writeResidencyFixture(t, root, "cmd/gc/alias.go", "package main\n\nfunc evade() {\n\tg := BeadStores\n\t_ = g()\n}\n")
		v := ratchetViolations(scan(t, root), map[string]int{})
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), residencyAliasPattern) {
			t.Fatalf("an unpinned alias was not reported as growth: %v", v)
		}
	})
}

// TestResidencyPlanLegChainControls falsifies the leg-chain rule. The split
// form is the one the grep row cannot see; the single-line form proves this
// rule subsumes its shape rather than merely complementing it.
func TestResidencyPlanLegChainControls(t *testing.T) {
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	scan := func(t *testing.T, body string) int {
		t.Helper()
		root := t.TempDir()
		writeResidencyFixture(t, root, "cmd/gc/legs.go", body)
		found, err := scanResidencyASTExpressions(root, residencyScanDirs, nil, names)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return found["cmd/gc/legs.go\tuse\t"+residencyLegChainPattern]
	}

	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\ts := pl.\n\t\tLeg.\n\t\tStore\n\t_ = s\n}\n"); got != 1 {
		t.Errorf("the line-split chain the grep row cannot see was counted %d times, want 1", got)
	}
	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\t_ = pl.Leg.Store\n}\n"); got != 1 {
		t.Errorf("the single-line chain was counted %d times, want 1", got)
	}
	// The parenthesized form is the third spelling and the one that defeats BOTH
	// halves as written: gofmt preserves the parens, the grep row reads `.Leg)`
	// and stops, and an AST rule that required the chain's inner node to be a
	// selector directly walked past the ParenExpr. Nested parens are legal Go and
	// gofmt keeps those too, so the unwrap has to be a loop.
	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\t_ = (pl.Leg).Store\n}\n"); got != 1 {
		t.Errorf("the parenthesized chain was counted %d times, want 1", got)
	}
	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\t_ = ((pl.Leg)).Store\n}\n"); got != 1 {
		t.Errorf("the doubly-parenthesized chain was counted %d times, want 1", got)
	}
	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\t_ = pl.Leg.StoreDir\n}\n"); got != 0 {
		t.Errorf("a different field on the same leg was counted %d times, want 0", got)
	}
	if got := scan(t, "package main\n\nfunc use(pl storeref.ResolvedPlan) {\n\t_ = pl.Legs.Store\n}\n"); got != 0 {
		t.Errorf("a different field holding the chain's outer name was counted %d times, want 0", got)
	}
	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\t_ = pl.Leg.Store // "+residencyAllowMarker+" tested escape hatch\n}\n"); got != 0 {
		t.Errorf("the marker did not suppress the chain: counted %d times", got)
	}
	if got := scan(t, "package main\n\nfunc use() {}\n"); got != 0 {
		t.Errorf("a clean file was counted %d times", got)
	}
}

// ---------------------------------------------------------------------------
// The shared baseline and ratchet.

// readResidencyBaseline reads the
// `path <TAB> function <TAB> pattern <TAB> count` rows whose pattern the
// predicate accepts, keyed "path\tfunction\tpattern".
func readResidencyBaseline(path string, want func(pattern string) bool) (map[string]int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close() //nolint:errcheck // read-only

	out := map[string]int{}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, "\t")
		if len(parts) != 4 {
			return nil, fmt.Errorf("baseline row %q is not path<TAB>function<TAB>pattern<TAB>count", line)
		}
		if !want(parts[2]) {
			continue
		}
		n, err := strconv.Atoi(strings.TrimSpace(parts[3]))
		if err != nil {
			return nil, fmt.Errorf("baseline row %q: bad count %q", line, parts[3])
		}
		out[parts[0]+"\t"+parts[1]+"\t"+parts[2]] = n
	}
	return out, scanner.Err()
}

// ratchetViolations reports growth and un-shrunk staleness, in that order.
func ratchetViolations(found, baseline map[string]int) []string {
	var out []string
	for _, key := range sortedKeys(found) {
		if found[key] > baseline[key] {
			out = append(out, fmt.Sprintf("%s: %d sites, baseline %d — a NEW store-enumeration site. Consume internal/storeref (Plan/ResolveOwner/Union), or annotate it `%s <reason>`.",
				strings.ReplaceAll(key, "\t", " "), found[key], baseline[key], residencyAllowMarker))
		}
	}
	for _, key := range sortedKeys(baseline) {
		if found[key] < baseline[key] {
			out = append(out, fmt.Sprintf("%s: %d sites, baseline %d — shrink the baseline in the same commit that retires a site. Follow the regeneration procedure in the header of scripts/residency-boundary-baseline.txt: three emitters feed that one file, so running any of them alone over it drops the other two halves' rows.",
				strings.ReplaceAll(key, "\t", " "), found[key], baseline[key]))
		}
	}
	return out
}

func assertRatchet(t *testing.T, found, baseline map[string]int) {
	t.Helper()
	for _, v := range ratchetViolations(found, baseline) {
		t.Errorf("RESIDENCY-BOUNDARY: %s", v)
	}
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func writeResidencyFixture(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", rel, err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}
