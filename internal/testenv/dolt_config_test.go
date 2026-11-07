package testenv_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const doltConfigFileName = "config_global.json"

// sharedDoltConfigWriter is the one file allowed to write the dolt global
// config, relative to the repository root.
var sharedDoltConfigWriter = filepath.Join("internal", "testutil", "dolt.go")

// TestOnlyTheSharedHelperWritesDoltGlobalConfig keeps the dolt global config to
// one source, testutil.SeedDoltGlobalConfig. A test that writes
// config_global.json itself ships the identity and silently drops
// versioncheck.disabled and metrics.disabled, which exposes its dolt to the
// network: `dolt version` has no timeout of its own on a slow network while the
// pinned bd kills it after 10 s. The guard is syntactic: any os.WriteFile,
// os.Create or os.OpenFile whose arguments contain a string literal naming
// config_global.json, anywhere outside internal/testutil/dolt.go, fails. A path
// assembled from a named constant is not seen.
func TestOnlyTheSharedHelperWritesDoltGlobalConfig(t *testing.T) {
	root := repoRoot(t)
	shared := filepath.Join(root, sharedDoltConfigWriter)
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("the shared dolt global config writer moved: %v\nupdate sharedDoltConfigWriter in this test", err)
	}
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
		if !strings.HasSuffix(path, ".go") || path == shared {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		scanned++
		rel, _ := filepath.Rel(root, path)
		lines, err := doltConfigWriteLines(rel, src)
		if err != nil {
			return err
		}
		for _, line := range lines {
			offenders = append(offenders, rel+":"+strconv.Itoa(line))
		}
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
		t.Fatalf("write config_global.json only through testutil.SeedDoltGlobalConfig, which carries versioncheck.disabled and metrics.disabled:\n  %s",
			strings.Join(offenders, "\n  "))
	}
}

// TestDoltConfigWriteDetection proves the guard above sees the shapes of write
// it exists to catch and ignores reads and the shared helper's callers.
func TestDoltConfigWriteDetection(t *testing.T) {
	tests := []struct {
		name string
		body string
		want []int
	}{
		{
			name: "WriteFile of a joined path",
			body: `os.WriteFile(filepath.Join(home, ".dolt", "config_global.json"), cfg, 0o644)`,
			want: []int{6},
		},
		{
			name: "WriteFile of a literal path",
			body: `os.WriteFile("/tmp/home/.dolt/config_global.json", cfg, 0o644)`,
			want: []int{6},
		},
		{
			name: "Create",
			body: `os.Create(filepath.Join(home, ".dolt", "config_global.json"))`,
			want: []int{6},
		},
		{
			name: "OpenFile",
			body: `os.OpenFile(filepath.Join(home, "config_global.json"), os.O_WRONLY, 0o644)`,
			want: []int{6},
		},
		{
			name: "every offending call is reported",
			body: "os.WriteFile(filepath.Join(a, \"config_global.json\"), cfg, 0o644)\n\tos.WriteFile(filepath.Join(b, \"config_global.json\"), cfg, 0o644)",
			want: []int{6, 7},
		},
		{
			name: "ReadFile is not a write",
			body: `os.ReadFile(filepath.Join(home, ".dolt", "config_global.json"))`,
		},
		{
			name: "the shared helper is the way to seed",
			body: `testutil.SeedDoltGlobalConfig(home)`,
		},
		{
			name: "a write of another file",
			body: `os.WriteFile(filepath.Join(home, ".gitconfig"), cfg, 0o644)`,
		},
		{
			name: "the name in a comment is not a write",
			body: "// os.WriteFile(\"config_global.json\", cfg, 0o644)\n\tx()",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := "package p\n\nimport \"os\"\n\nfunc f() {\n\t" + tt.body + "\n}\n"
			got, err := doltConfigWriteLines("p.go", []byte(src))
			if err != nil {
				t.Fatalf("doltConfigWriteLines: %v\n%s", err, src)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("offending lines = %v, want %v\n%s", got, tt.want, src)
			}
		})
	}
}

// doltConfigWriteLines returns the line of every os.WriteFile, os.Create or
// os.OpenFile call in src whose arguments name config_global.json. name is only
// used to label parse errors and positions.
func doltConfigWriteLines(name string, src []byte) ([]int, error) {
	if !bytes.Contains(src, []byte(doltConfigFileName)) {
		return nil, nil
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	var lines []int
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || !isFileWrite(call) {
			return true
		}
		for _, arg := range call.Args {
			if namesDoltConfigFile(arg) {
				lines = append(lines, fset.Position(call.Pos()).Line)
				break
			}
		}
		return true
	})
	return lines, nil
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
