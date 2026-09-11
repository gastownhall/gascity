package session

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// startedConfigHashKey and the priming keys as raw metadata strings. The gate
// below works on source text, so it matches both the raw string literals and
// the exported constants that resolve to them.
const startedConfigHashKey = "started_config_hash"

var primingKeyValues = map[string]bool{
	"primed_at":            true,
	"priming_attempted_at": true,
	"prompt_hash":          true,
}

// primingConstToValue maps the exported priming-key constants to their metadata
// string, so a clear written as sessionpkg.PrimedAtMetadataKey is recognized the
// same as the raw "primed_at".
var primingConstToValue = map[string]string{
	"PrimedAtMetadataKey":           "primed_at",
	"PrimingAttemptedAtMetadataKey": "priming_attempted_at",
	"PromptHashMetadataKey":         "prompt_hash",
}

// TestEveryStartedConfigHashClearAlsoClearsPriming is the repo-wide LIFETIME-RULE
// gate (S19 Stage 2 §C). Every non-test function that clears started_config_hash
// to "" MUST also clear all three priming markers in the same function, so a
// future clear site cannot silently strand a stale confirmation pair on a fresh
// incarnation (spec risk #5). The gate parses the two source trees that own the
// clear sites (internal/session, cmd/gc) and fails if any clearing function skips
// the priming reset.
func TestEveryStartedConfigHashClearAlsoClearsPriming(t *testing.T) {
	root := repoRoot(t)
	trees := []string{
		filepath.Join(root, "internal", "session"),
		filepath.Join(root, "cmd", "gc"),
	}

	var clearingFuncs int
	for _, dir := range trees {
		files, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("reading %s: %v", dir, err)
		}
		for _, f := range files {
			if f.IsDir() || !strings.HasSuffix(f.Name(), ".go") || strings.HasSuffix(f.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, f.Name())
			fset := token.NewFileSet()
			file, err := parser.ParseFile(fset, path, nil, 0)
			if err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				cleared := collectEmptyStringClears(fn.Body)
				usesHelper := funcUsesPrimingHelper(fn.Body)
				if !cleared[startedConfigHashKey] {
					continue
				}
				clearingFuncs++
				if usesHelper {
					continue
				}
				missing := []string{}
				for v := range primingKeyValues {
					if !cleared[v] {
						missing = append(missing, v)
					}
				}
				if len(missing) > 0 {
					rel, _ := filepath.Rel(root, path)
					t.Errorf("%s: %s clears started_config_hash=\"\" but does not clear priming markers %v "+
						"(S19 Stage 2 lifetime rule: every started_config_hash clear must clearPrimingMarkers)",
						rel, fn.Name.Name, missing)
				}
			}
		}
	}

	// Sanity: the gate must actually be scanning real clear sites, else a parse
	// or path regression would make it silently vacuous.
	if clearingFuncs < 7 {
		t.Fatalf("gate found only %d started_config_hash clear sites, expected the 7 known C-sites; scan is stale", clearingFuncs)
	}
}

// funcUsesPrimingHelper reports whether the function body clears the priming
// markers through the shared helper — a clearPrimingMarkers(...) call or a range
// over primingResetKeys — either of which clears all three keys at once.
func funcUsesPrimingHelper(body *ast.BlockStmt) bool {
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.CallExpr:
			if id, ok := node.Fun.(*ast.Ident); ok && id.Name == "clearPrimingMarkers" {
				found = true
			}
		case *ast.RangeStmt:
			if id, ok := node.X.(*ast.Ident); ok && id.Name == "primingResetKeys" {
				found = true
			}
		}
		return true
	})
	return found
}

// collectEmptyStringClears returns the set of metadata keys the function clears
// to "" — via map composite literals, index assignments, or SetMetadata calls.
// Keys named by the priming constants are normalized to their string value.
func collectEmptyStringClears(body *ast.BlockStmt) map[string]bool {
	cleared := map[string]bool{}
	record := func(keyNode, valNode ast.Expr) {
		if !isEmptyStringLit(valNode) {
			return
		}
		if k, ok := metadataKeyName(keyNode); ok {
			cleared[k] = true
		}
	}
	ast.Inspect(body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.KeyValueExpr:
			record(node.Key, node.Value)
		case *ast.AssignStmt:
			for i, lhs := range node.Lhs {
				idx, ok := lhs.(*ast.IndexExpr)
				if !ok || i >= len(node.Rhs) {
					continue
				}
				record(idx.Index, node.Rhs[i])
			}
		case *ast.CallExpr:
			// SetMetadata(id, key, value) — the trailing two args are (key, value).
			if sel, ok := node.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "SetMetadata" && len(node.Args) >= 3 {
				n := len(node.Args)
				record(node.Args[n-2], node.Args[n-1])
			}
		}
		return true
	})
	return cleared
}

// metadataKeyName resolves a key expression to its metadata string value:
// a raw string literal, or one of the known priming/started-config constants
// (bare or package-qualified).
func metadataKeyName(e ast.Expr) (string, bool) {
	switch node := e.(type) {
	case *ast.BasicLit:
		if node.Kind == token.STRING {
			if s, err := strconv.Unquote(node.Value); err == nil {
				return s, true
			}
		}
	case *ast.Ident:
		if v, ok := primingConstToValue[node.Name]; ok {
			return v, true
		}
	case *ast.SelectorExpr:
		if v, ok := primingConstToValue[node.Sel.Name]; ok {
			return v, true
		}
	}
	return "", false
}

func isEmptyStringLit(e ast.Expr) bool {
	lit, ok := e.(*ast.BasicLit)
	return ok && lit.Kind == token.STRING && (lit.Value == `""` || lit.Value == "``")
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate repo root (no go.mod found walking up)")
		}
		dir = parent
	}
}
