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

// residencyASTScanDirs are the packages whose store-list SIGNATURES are
// governed. internal/sling and internal/dispatch hold no store-list constructor
// today, and one added there lands as a new grep hit instead.
var residencyASTScanDirs = []string{"cmd/gc", "internal/api"}

// residencyAllowlist mirrors the shell guard's: the topology constructors
// (which build the Topology and must enumerate), the migration tooling (which
// creates topology by definition), and the fork-only work-axis router.
var residencyAllowlist = map[string]bool{
	"cmd/gc/residency_topology.go":       true,
	"internal/api/residency_topology.go": true,
	"cmd/gc/infra_class_migrate.go":      true,
	"cmd/gc/cmd_storage.go":              true,
	"cmd/gc/cmd_bd_topology.go":          true,
}

const residencyASTPattern = "ast:returns-store-list"

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
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
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
	baseline, err := readResidencyBaseline(residencyBaselinePath(t), func(p string) bool { return p != residencyASTPattern })
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
// loop, over 89 distinct (file, function) keys on the real tree. If the two
// enclosing-function state machines ever disagreed about one line, one of them
// would fail against the shared file. What is left to check is the scan scope
// and the allowlist, which live in each half separately.
func TestResidencyBoundaryHalvesAgree(t *testing.T) {
	script, err := os.ReadFile(filepath.Join(residencyScriptsDir(t), "check-residency-boundary.sh"))
	if err != nil {
		t.Fatalf("reading the shell guard: %v", err)
	}
	body := string(script)
	for _, dir := range residencyScanDirs {
		if !strings.Contains(body, dir) {
			t.Errorf("the shell guard does not scan %q", dir)
		}
	}
	for path := range residencyAllowlist {
		if !strings.Contains(body, path) {
			t.Errorf("the shell guard does not allowlist %q", path)
		}
	}
	if !strings.Contains(body, "residency-boundary-patterns.txt") {
		t.Error("the shell guard does not read the shared pattern file")
	}
	if !strings.Contains(body, "--self-test") {
		t.Error("the shell guard has no --self-test; a guard nothing asserts against can be defanged silently")
	}
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
	found, err := scanStoreListSignatures(root, residencyASTScanDirs, residencyAllowlist)
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
		found, err := scanStoreListSignatures(root, residencyASTScanDirs, nil)
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
func scanStoreListSignatures(root string, dirs []string, allowlist map[string]bool) (map[string]int, error) {
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
				// node_modules and the built dashboard bundle hold no Go and
				// are large enough that walking them is a real cost.
				if path != abs && (name == "node_modules" || name == "dist") {
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
			file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Type.Results == nil {
					continue
				}
				if fn.Doc != nil && strings.Contains(fn.Doc.Text(), residencyAllowMarker) {
					continue
				}
				for _, result := range fn.Type.Results.List {
					if isRawStoreList(result.Type) {
						found[rel+"\t"+fn.Name.Name+"\t"+residencyASTPattern]++
						break
					}
				}
			}
			return nil
		})
		if walkErr != nil {
			return nil, walkErr
		}
	}
	return found, nil
}

// isRawStoreList reports whether expr is []beads.Store, map[string]beads.Store
// or []storeref.Leg — the shapes that hand a caller a probe list it will read
// itself.
//
// The first two carry no leg order and no error policy at all. []storeref.Leg
// is the subtler one: a consumer that enumerated a plan through EachLeg and
// then RETURNS the bare legs has stripped the policy back off and handed the
// next caller the same unpoliced list, one function further from the resolver.
// The grep half ratchets the EachLeg call; this ratchets the shape it can be
// laundered into.
func isRawStoreList(expr ast.Expr) bool {
	switch typed := expr.(type) {
	case *ast.ArrayType:
		return typed.Len == nil && (isBeadsStore(typed.Elt) || isStorerefLeg(typed.Elt))
	case *ast.MapType:
		key, ok := typed.Key.(*ast.Ident)
		return ok && key.Name == "string" && (isBeadsStore(typed.Value) || isStorerefLeg(typed.Value))
	default:
		return false
	}
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
			out = append(out, fmt.Sprintf("%s: %d sites, baseline %d — shrink the baseline in the same commit that retires a site (scripts/check-residency-boundary.sh --emit-baseline, plus RESIDENCY_BASELINE_EMIT=1 go test ./scripts -run TestResidencyResolverBoundary).",
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
