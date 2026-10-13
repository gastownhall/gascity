package citylayout

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/testpolicy/resourcecensus"
)

// exportKeptRepoRuntimeEntries lists top-level .gc/ entries, named anywhere
// in the module's Go source, that ship with the work dir on purpose. Every
// other top-level entry the source names must be excluded by default.
var exportKeptRepoRuntimeEntries = map[string]string{
	"settings.json":     "session-side gc reads the materialized city settings",
	"site.toml":         "session-side gc loads it with city.toml",
	"cache":             "fetched pack sources a session's gc loads with city.toml",
	"system":            "bundled system packs a session's gc loads with city.toml",
	"scripts":           "pack scripts sessions run",
	"tmp":               "prompt and skill snapshots sessions read at start",
	"mcp-managed":       "MCP projections the session's provider reads",
	"molecules":         "molecule step artifacts agents read and write",
	"artifacts":         "convergence iteration artifacts agents read and write",
	"synth":             "staged meta-prompts the slung agent reads",
	"file-beads-layout": "layout marker only; the file store it describes is excluded",
}

// exportHomeOnlyRuntimeEntries lists .gc/ entries the source builds by
// joining ".gc" under the user's home directory (the per-user gc home), never
// under a city or rig scope.
var exportHomeOnlyRuntimeEntries = map[string]string{
	"completions": "shell completion scripts under ~/.gc",
}

// gcEntryInLiteral finds ".gc/<name>" inside a string literal, where .gc is a
// whole path segment.
var gcEntryInLiteral = regexp.MustCompile(`(?:^|[^A-Za-z0-9_.-])(\.gc/([A-Za-z0-9_][A-Za-z0-9_.-]*))`)

// TestEveryRepoRuntimeEntryIsClassified scans every tracked non-test Go file
// in the module for the top-level .gc/ entries it names: as a ".gc/<name>"
// string literal, as consecutive ".gc" (or RuntimeRoot) and "<name>"
// arguments to a call such as filepath.Join, or as RuntimePath(root,
// "<name>", ...). Each entry must be excluded from the work-dir export or kept
// on purpose in exportKeptRepoRuntimeEntries. The guard covers every
// top-level .gc/ entry the module's Go source names; it cannot see a path
// that config places, such as a relocated SQLite binding root.
func TestEveryRepoRuntimeEntryIsClassified(t *testing.T) {
	entries := repoRuntimeEntries(t)
	for _, want := range []string{"agents", "scope-ownership.json", "dispatch-wake", "events.jsonl", "sessions"} {
		if !entries[want] {
			t.Fatalf("source scan found %v, missing .gc/%s; the scanner no longer sees the module's .gc/ entries", sortedKeys(entries), want)
		}
	}
	for name := range entries {
		if _, home := exportHomeOnlyRuntimeEntries[name]; home {
			continue
		}
		_, kept := exportKeptRepoRuntimeEntries[name]
		excluded := excludedByDefault(t, RuntimeRoot+"/"+name)
		switch {
		case kept && excluded:
			t.Errorf(".gc/%s is on exportKeptRepoRuntimeEntries but the default excludes drop it", name)
		case !kept && !excluded:
			t.Errorf(".gc/%s is named in the module's source but is neither excluded from the work-dir export nor on exportKeptRepoRuntimeEntries", name)
		}
	}
	for _, listed := range []map[string]string{exportKeptRepoRuntimeEntries, exportHomeOnlyRuntimeEntries} {
		for name := range listed {
			if !entries[name] {
				t.Errorf(".gc/%s is classified, but the module's source no longer names it", name)
			}
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type censusFile struct {
	dir  string
	file *ast.File
}

func repoRuntimeEntries(t *testing.T) map[string]bool {
	t.Helper()
	root := bazeltest.RepoRoot(t)
	tracked, err := resourcecensus.TrackedGoFiles(root)
	if err != nil {
		t.Fatalf("listing tracked Go files under %s: %v", root, err)
	}
	fset := token.NewFileSet()
	var files []censusFile
	for _, rel := range tracked {
		// test/ holds acceptance and integration harnesses, which write
		// their own logs and reports under the cities they drive.
		if strings.HasSuffix(rel, "_test.go") || strings.HasPrefix(rel, "test/") || strings.Contains("/"+rel, "/testdata/") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("reading %s: %v", rel, err)
		}
		f, err := parser.ParseFile(fset, rel, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		files = append(files, censusFile{dir: path.Dir(rel), file: f})
	}
	if len(files) < 500 {
		t.Fatalf("scanned %d non-test Go files under %s; the module walk is broken", len(files), root)
	}

	// String constants, by package directory for unqualified references and
	// by package name for qualified ones.
	localConsts := map[string]map[string]ast.Expr{}
	qualifiedConsts := map[string]ast.Expr{}
	for _, cf := range files {
		if localConsts[cf.dir] == nil {
			localConsts[cf.dir] = map[string]ast.Expr{}
		}
		for _, decl := range cf.file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						localConsts[cf.dir][name.Name] = vs.Values[i]
						qualifiedConsts[cf.file.Name.Name+"."+name.Name] = vs.Values[i]
					}
				}
			}
		}
	}

	entries := map[string]bool{}
	addEntry := func(rest string) {
		name, _, _ := strings.Cut(rest, "/")
		name = strings.TrimRight(name, ".")
		if name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "%{}*$") {
			entries[name] = true
		}
	}
	for _, cf := range files {
		var eval func(ast.Expr, int) (string, bool)
		eval = func(e ast.Expr, depth int) (string, bool) {
			if depth > 16 {
				return "", false
			}
			switch v := e.(type) {
			case *ast.BasicLit:
				if v.Kind != token.STRING {
					return "", false
				}
				s, err := strconv.Unquote(v.Value)
				return s, err == nil
			case *ast.Ident:
				if c, ok := localConsts[cf.dir][v.Name]; ok {
					return eval(c, depth+1)
				}
			case *ast.SelectorExpr:
				if pkg, ok := v.X.(*ast.Ident); ok {
					if c, ok := qualifiedConsts[pkg.Name+"."+v.Sel.Name]; ok {
						return eval(c, depth+1)
					}
				}
			case *ast.BinaryExpr:
				if v.Op == token.ADD {
					l, lok := eval(v.X, depth+1)
					r, rok := eval(v.Y, depth+1)
					return l + r, lok && rok
				}
			}
			return "", false
		}
		ast.Inspect(cf.file, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.BasicLit:
				if s, ok := eval(v, 0); ok {
					for _, m := range gcEntryInLiteral.FindAllStringSubmatchIndex(s, -1) {
						// "~/.gc/" names the per-user gc home, not a
						// city or rig scope.
						if !strings.HasSuffix(s[:m[2]], "~/") {
							addEntry(s[m[4]:m[5]])
						}
					}
				}
			case *ast.CallExpr:
				var fn string
				switch f := v.Fun.(type) {
				case *ast.Ident:
					fn = f.Name
				case *ast.SelectorExpr:
					fn = f.Sel.Name
				}
				if fn == "RuntimePath" && len(v.Args) >= 2 {
					if s, ok := eval(v.Args[1], 0); ok {
						addEntry(s)
					}
				}
				for i := 0; i+1 < len(v.Args); i++ {
					if s, ok := eval(v.Args[i], 0); !ok || s != RuntimeRoot {
						continue
					}
					if s, ok := eval(v.Args[i+1], 0); ok {
						addEntry(s)
					}
				}
			}
			return true
		})
	}
	return entries
}
