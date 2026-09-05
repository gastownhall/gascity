package scripts_test

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The residency lookup contract's CI-visible enforcement: the scaffolding its
// four rules share.
//
// The contract has two halves and one baseline. The GREP half counts the
// store-enumeration vocabulary declared in residency-boundary-patterns.txt; the
// AST half sees what grep cannot — a function SIGNATURE that hands a caller a
// raw store list, whatever its body called to build it, and the expression
// spellings that fetch the vocabulary without calling it. Every rule ratchets
// against scripts/residency-boundary-baseline.txt, so no two can disagree about
// what is pinned.
//
//	residency_boundary_test.go          this file: scan scope, the allowlist,
//	                                    the pattern names, the shared walk
//	residency_grep_rule_test.go         rules a-d, the vocabulary census
//	residency_signature_rule_test.go    ast:returns-store-list
//	residency_expression_rules_test.go  ast:vocabulary-alias,
//	                                    ast:plan-leg-store-chain,
//	                                    ast:uncounted-call-spelling
//	residency_halves_agree_test.go      the shell and Go grep halves police the
//	                                    same tree with the same exemptions
//	residency_ratchet_test.go           the one baseline they all ratchet on
//
// scripts/check-residency-boundary.sh is the shell rendering of the grep half:
// it reads the SAME pattern file, is wired into `make check`, and proves its own
// bite through `--self-test`. This package is the rendering that runs in CI,
// since ./scripts is inside UNIT_COVER_PKGS_NONCMDGC (the route
// check_split_topology_rows_test.go established) — and it deliberately spawns no
// subprocess, because the test-resource census ratchets untagged subprocess call
// sites and a guard is not worth a debt row.
//
// Every control across these files is falsified with a REAL on-disk edit in a
// t.TempDir() tree, never an overlay: the guard reads files, and an in-memory
// overlay cannot prove a file-reading guard bites.

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

// residencyParsedFile is one non-test Go file of the scanned tree.
type residencyParsedFile struct {
	rel  string // slash-separated, relative to the repo root
	dir  string // the file's directory, which is its package for alias purposes
	data []byte // the source as read, for the rules that need line positions
	file *ast.File
}

// residencyTree is one walk's worth of parsed source, plus the fileset the
// positions in it are relative to.
type residencyTree struct {
	fset  *token.FileSet
	files []residencyParsedFile
}

// residencyParseTree walks and parses every non-test Go file under dirs, once,
// for every AST rule in this contract.
//
// The walk RECURSES, because the grep half does (`find "${present[@]}" -type f
// -name '*.go'`). A flat read left every subpackage of a governed directory —
// internal/api/dashboardbff, internal/api/genclient — outside this half
// entirely: a store-list constructor added there was caught only if its body
// also spelled the grep half's vocabulary, which a hand-rolled probe list need
// not do. The halves are supposed to cover the same tree by different means,
// not different trees.
//
// It returns the ALLOWLISTED files too, and leaves the filtering to each rule.
// The allowlist exempts a file from being COUNTED; the signature rule still has
// to read one for the type declarations a package's aliases come from, and a
// walk that dropped it here would make an exempt file the one place to mint an
// invisible spelling for the rest of its package.
//
// The result is deliberately NOT memoized across calls. The controls falsify
// each rule by rewriting real files in a t.TempDir() tree and rescanning, so a
// cache keyed by root would hand a control the tree from before its own edit
// and report that the guard bit when it had not been asked. One walk per scan
// is the honest shape, and it is what this delivers: one walk-and-prune
// IMPLEMENTATION shared by both AST rules, each of which still calls it once
// per scan. Removing the duplicated prune logic was worth more than the
// milliseconds the second walk costs.
func residencyParseTree(root string, dirs []string) (residencyTree, error) {
	out := residencyTree{fset: token.NewFileSet()}
	fset := out.fset
	for _, dir := range dirs {
		abs := filepath.Join(root, filepath.FromSlash(dir))
		if _, statErr := os.Stat(abs); os.IsNotExist(statErr) {
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
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			file, err := parser.ParseFile(fset, path, data, parser.ParseComments)
			if err != nil {
				return fmt.Errorf("%s: %w", rel, err)
			}
			out.files = append(out.files, residencyParsedFile{rel: rel, dir: filepath.Dir(path), data: data, file: file})
			return nil
		})
		if walkErr != nil {
			return residencyTree{}, walkErr
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
