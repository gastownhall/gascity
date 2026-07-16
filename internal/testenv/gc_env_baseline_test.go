package testenv_test

import (
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

// gcVarNamePattern matches a bare GC_ environment-variable NAME (not a command
// string or assignment prefix that merely starts with GC_).
var gcVarNamePattern = regexp.MustCompile(`^GC_[A-Z0-9_]+$`)

// TestGCEnvReadBaseline freezes the VOCABULARY of GC_* environment-variable names
// NON-TEST code references — both direct reads (os.Getenv/os.LookupEnv with a
// string literal) and the const-idiom (const gcX = "GC_X"; os.Getenv(gcX)),
// captured via const/var declaration values so an intermediate constant cannot
// slip a new var past the freeze. Adding a new GC_* var must be a deliberate
// change that updates testdata/gc_env_read_baseline.golden — so a rollout gate (or
// any new capability) cannot quietly grow an ad-hoc env knob instead of going
// through internal/rollout + config. It freezes the distinct var-name SET (not
// per-site counts, which churn on reformatting). SCOPE: non-test .go only — test
// files legitimately reference many GC_* vars.
func TestGCEnvReadBaseline(t *testing.T) {
	root := repoRoot(t)
	got := map[string]bool{}
	fset := token.NewFileSet()

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// testdata/ holds uncompiled fixture .go that go build never sees;
			// a fixture with a GC_ literal would inject a phantom vocabulary entry.
			if skipRepoLintDir(d.Name()) || d.Name() == "testdata" || (path != root && isNestedWorktreeRoot(path)) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return nil // skip unparseable/generated files
		}
		record := func(lit *ast.BasicLit) {
			if lit == nil || lit.Kind != token.STRING {
				return
			}
			// Only bare env-var NAMES — not command strings ("GC_X=1 gc prime …")
			// or assignment prefixes ("GC_X=") that also start with GC_.
			if v, err := strconv.Unquote(lit.Value); err == nil && gcVarNamePattern.MatchString(v) {
				got[v] = true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				// os.Getenv("GC_...") / os.LookupEnv("GC_...")
				if len(x.Args) > 0 && isEnvReadCall(x.Fun) {
					if lit, ok := x.Args[0].(*ast.BasicLit); ok {
						record(lit)
					}
				}
			case *ast.ValueSpec:
				// const/var GC_NAME = "GC_..." — the const-idiom used by
				// os.Getenv(gcName); freezing the definition catches a new var read
				// through an intermediate constant.
				for _, val := range x.Values {
					if lit, ok := val.(*ast.BasicLit); ok {
						record(lit)
					}
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	want := readBaselineGolden(t, filepath.Join("testdata", "gc_env_read_baseline.golden"))
	gotList := sortedSet(got)
	if !equalStringSlices(gotList, want) {
		added, removed := diffStringSets(want, gotList)
		t.Fatalf("GC_* env-read vocabulary changed vs testdata/gc_env_read_baseline.golden.\n"+
			"ADDED (new env reads — add to internal/rollout/config instead of an ad-hoc GC_ var, or update the golden deliberately):\n  %s\n"+
			"REMOVED (update the golden):\n  %s",
			strings.Join(added, "\n  "), strings.Join(removed, "\n  "))
	}
}

// isEnvReadCall reports whether fun is os.Getenv or os.LookupEnv.
func isEnvReadCall(fun ast.Expr) bool {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok || pkg.Name != "os" {
		return false
	}
	return sel.Sel.Name == "Getenv" || sel.Sel.Name == "LookupEnv"
}

func readBaselineGolden(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if s := strings.TrimSpace(line); s != "" {
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func diffStringSets(want, got []string) (added, removed []string) {
	w := map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	g := map[string]bool{}
	for _, s := range got {
		g[s] = true
	}
	for _, s := range got {
		if !w[s] {
			added = append(added, s)
		}
	}
	for _, s := range want {
		if !g[s] {
			removed = append(removed, s)
		}
	}
	sort.Strings(added)
	sort.Strings(removed)
	return added, removed
}
