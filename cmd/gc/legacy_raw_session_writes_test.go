package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// legacyRawSessionWritesBaseline is the per-file count TestLegacyRawSessionWrites
// may not exceed, and must be lowered to when a file's count falls.
const legacyRawSessionWritesBaseline = "testdata/legacy_raw_session_writes.golden"

// rawWriteVerb is a raw row write: where its patch (or key) is, and whether
// an unresolved patch counts (a session-only verb) or only a resolved premise
// key does (the generic bead writes, which work beads share).
type rawWriteVerb struct {
	patch       int
	sessionOnly bool
}

// rawSessionWriteVerbs are the raw row writes R8 replaces with Commit, and
// the wrappers that forward a caller's patch to one. A function that forwards
// its caller's patch to a verb here must be listed itself.
var rawSessionWriteVerbs = map[string]rawWriteVerb{
	"ApplyPatch": {1, true}, "ApplyPatchInfo": {1, true}, "SetMarker": {1, true},
	"ApplyKeepingUserHold": {1, true},
	"applyStore":           {2, true}, "applySleepOptimistic": {2, true},
	"setMetaBatch": {2, true}, "writeSessionMetadata": {2, true},
	"setMeta": {2, true}, "setMetadataValue": {1, true},
	"SetMetadataBatch": {1, false}, "SetMetadata": {1, false}, "setWaitTerminalState": {1, false},
}

// rawWriteKeyVerbs take one key, not a patch.
var rawWriteKeyVerbs = []string{"SetMarker", "SetMetadata", "setMeta", "setMetadataValue"}

// rawWriteFenced are the fenced primitives: they write only on a fresh read
// that matched, at its revision. The verb a body forwards its patch to is the
// fallback for a store without conditional writes, the documented residual.
var rawWriteFenced = map[string]string{
	"commitIf":                    "Store.Commit's and ApplyPatchIfLifecycleUnchanged's loop",
	"CommitStartedIfCurrentUnder": "the start commit (v5 S2)",
	"applyPatchIfClosed":          "writes only a row still closed",
}

// rawWriteScanDirs are the legacy and operator packages the ratchet scans.
var rawWriteScanDirs = []string{"cmd/gc", "internal/session", "internal/api"}

// TestLegacyRawSessionWrites is R8's ratchet: per production file in the
// legacy and operator packages, the raw writes of a premise key
// (Field.InPremise) that do not go through Commit. A patch's keys are its map
// literal's, a string key's, or its builder's (the registry's writer and
// clear sites by function name). Keys match by name, without types, so a
// work or wait bead write of a same-named key counts too. A wrapper's own
// forwarding write is not
// counted, its callers' writes are; an unlisted wrapper fails. The count may
// only fall: a new raw premise write fails, and a removed one must lower the
// baseline.
func TestLegacyRawSessionWrites(t *testing.T) {
	root := repoRootForLint(t)
	files := map[string]*ast.File{}
	fset := token.NewFileSet()
	for _, dir := range rawWriteScanDirs {
		paths, err := filepath.Glob(filepath.Join(root, dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range paths {
			if strings.HasSuffix(p, "_test.go") {
				continue
			}
			f, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				t.Fatalf("parse %s: %v", p, err)
			}
			files[filepath.ToSlash(filepath.Join(dir, filepath.Base(p)))] = f
		}
	}
	consts := stringConsts(files)
	builders := builderKeys()
	got := map[string]int{}
	for rel, f := range files {
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			_, listed := rawSessionWriteVerbs[fn.Name.Name]
			_, fenced := rawWriteFenced[fn.Name.Name]
			params := paramNames(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				name := calleeName(call.Fun)
				verb, isVerb := rawSessionWriteVerbs[name]
				if !isVerb || len(call.Args) <= verb.patch {
					return true // not a row write: Info.ApplyPatch folds a snapshot
				}
				if id, ok := ast.Unparen(call.Args[verb.patch]).(*ast.Ident); ok && params[id.Name] {
					if !listed && !fenced {
						t.Errorf("%s: %s forwards its caller's patch to %s: list it in rawSessionWriteVerbs", rel, fn.Name.Name, name)
					}
					return true // a wrapper's forward: its callers' writes are counted
				}
				keys, resolved := patchKeys(call.Args[verb.patch], name, consts, builders)
				if slices.ContainsFunc(keys, premiseKey) || (!resolved && verb.sessionOnly) {
					got[rel]++
				}
				return true
			})
		}
	}

	want := readRawWritesBaseline(t)
	total, wantTotal := 0, 0
	for rel, n := range got {
		total += n
		if n > want[rel] {
			t.Errorf("%s: %d raw premise writes, baseline %d: write through session.Store.Commit on a Decided", rel, n, want[rel])
		}
	}
	for rel, n := range want {
		wantTotal += n
		if got[rel] < n {
			t.Errorf("%s: %d raw premise writes, below its baseline %d: lower %s", rel, got[rel], n, legacyRawSessionWritesBaseline)
		}
	}
	if t.Failed() {
		var lines []string
		for rel, n := range got {
			lines = append(lines, fmt.Sprintf("%s %d", rel, n))
		}
		slices.Sort(lines)
		t.Logf("counted %d (baseline %d):\n%s", total, wantTotal, strings.Join(lines, "\n"))
	}
	if total == 0 || len(files) < 100 {
		t.Fatalf("counted %d writes in %d files; the scan missed the tree", total, len(files))
	}
}

func premiseKey(k string) bool {
	f, ok := session.LookupField(k)
	return ok && f.InPremise()
}

// calleeName is a call's function or method name.
func calleeName(fun ast.Expr) string {
	switch fn := fun.(type) {
	case *ast.Ident:
		return fn.Name
	case *ast.SelectorExpr:
		return fn.Sel.Name
	}
	return ""
}

// paramNames are fn's parameter names.
func paramNames(fn *ast.FuncDecl) map[string]bool {
	out := map[string]bool{}
	for _, field := range fn.Type.Params.List {
		for _, name := range field.Names {
			out[name.Name] = true
		}
	}
	return out
}

// patchKeys resolves the keys a raw write's patch (or key) arg writes, and
// whether it could.
func patchKeys(arg ast.Expr, verb string, consts map[string]string, builders map[string][]string) ([]string, bool) {
	if slices.Contains(rawWriteKeyVerbs, verb) {
		k, ok := stringValue(arg, consts)
		return []string{k}, ok
	}
	switch patch := ast.Unparen(arg).(type) {
	case *ast.CompositeLit:
		var keys []string
		for _, el := range patch.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				return keys, false
			}
			k, ok := stringValue(kv.Key, consts)
			if !ok {
				return keys, false
			}
			keys = append(keys, k)
		}
		return keys, true
	case *ast.CallExpr:
		keys, ok := builders[calleeName(patch.Fun)]
		return keys, ok
	}
	return nil, false
}

// stringValue is a string literal's or a string constant's value.
func stringValue(e ast.Expr, consts map[string]string) (string, bool) {
	switch v := ast.Unparen(e).(type) {
	case *ast.BasicLit:
		s, err := strconv.Unquote(v.Value)
		return s, err == nil
	case *ast.Ident:
		s, ok := consts[v.Name]
		return s, ok
	case *ast.SelectorExpr:
		s, ok := consts[v.Sel.Name]
		return s, ok
	}
	return "", false
}

// stringConsts are the files' string constants by name.
func stringConsts(files map[string]*ast.File) map[string]string {
	out := map[string]string{}
	for _, f := range files {
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, name := range vs.Names {
					if i < len(vs.Values) {
						if s, ok := stringValue(vs.Values[i], nil); ok {
							out[name.Name] = s
						}
					}
				}
			}
		}
	}
	return out
}

// builderKeys are, per function name, the keys the registry says it writes
// or clears.
func builderKeys() map[string][]string {
	out := map[string][]string{}
	for _, f := range session.Fields() {
		for _, s := range slices.Concat(f.Writers, f.Clears) {
			name := s.Func[strings.LastIndex(s.Func, ".")+1:]
			if name != "" && !slices.Contains(out[name], f.Key) {
				out[name] = append(out[name], f.Key)
			}
		}
	}
	return out
}

func readRawWritesBaseline(t *testing.T) map[string]int {
	t.Helper()
	data, err := os.ReadFile(legacyRawSessionWritesBaseline)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rel, n, ok := strings.Cut(line, " ")
		count, err := strconv.Atoi(n)
		if !ok || err != nil {
			t.Fatalf("%s: bad line %q", legacyRawSessionWritesBaseline, line)
		}
		out[rel] = count
	}
	return out
}
