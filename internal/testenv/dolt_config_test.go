package testenv_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const doltConfigFileName = "config_global.json"

// TestOnlyTheSharedHelperWritesDoltGlobalConfig keeps the dolt global config to
// one source, test/dolttest.WriteGlobalConfig. A test that writes
// config_global.json itself ships the identity and silently drops
// versioncheck.disabled and metrics.disabled, which is how every real-dolt test
// in the tree was exposed to the network until now: `dolt version` waits up to
// 30 s on a slow network while the pinned bd kills it after 10 s. The guard is
// syntactic: any os.WriteFile, os.Create or os.OpenFile whose arguments name
// config_global.json, anywhere outside test/dolttest, fails.
func TestOnlyTheSharedHelperWritesDoltGlobalConfig(t *testing.T) {
	root := repoRoot(t)
	sharedDir := filepath.Join(root, "test", "dolttest") + string(filepath.Separator)
	var offenders []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipRepoLintDir(path, root, d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasPrefix(path, sharedDir) {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		if !bytes.Contains(src, []byte(doltConfigFileName)) {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isFileWrite(call) {
				return true
			}
			for _, arg := range call.Args {
				if namesDoltConfigFile(arg) {
					offenders = append(offenders, rel+":"+strconv.Itoa(fset.Position(call.Pos()).Line))
					break
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scan for dolt global config writers: %v", err)
	}
	if scanned == 0 {
		t.Fatalf("dolt global config guard scanned zero Go files under %s; it evaluated nothing", root)
	}
	sort.Strings(offenders)
	if len(offenders) > 0 {
		t.Fatalf("write config_global.json only through dolttest.WriteGlobalConfig, which carries versioncheck.disabled and metrics.disabled:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// isFileWrite reports whether call is os.WriteFile, os.Create or os.OpenFile.
func isFileWrite(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "os" {
		return false
	}
	switch sel.Sel.Name {
	case "WriteFile", "Create", "OpenFile":
		return true
	}
	return false
}

// namesDoltConfigFile reports whether expr contains a string literal that
// names config_global.json, so filepath.Join(dir, ".dolt", "config_global.json")
// and a literal path ending in it both match.
func namesDoltConfigFile(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if value, err := strconv.Unquote(lit.Value); err == nil && strings.Contains(value, doltConfigFileName) {
			found = true
		}
		return !found
	})
	return found
}
