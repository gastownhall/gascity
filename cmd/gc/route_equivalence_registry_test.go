package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// routedReadRegistration records how a command that can read through the
// supervisor API proves it prints the same JSON on every route (#8453).
// Exactly one field is set.
type routedReadRegistration struct {
	// equivalenceTest names a Test function in this package that runs the
	// command on the local route and on the API route against the same seeded
	// state and requires identical output, modulo additive keys the command's
	// schema declares.
	equivalenceTest string
	// exemptReason explains why the command has no two routes to compare.
	exemptReason string
}

// routedReadRegistry lists every command name the non-test cmd/gc sources pass
// to the route-logging helpers. TestEveryRoutedReadHasARouteEquivalenceTest
// keeps it in step with the sources in both directions.
var routedReadRegistry = map[string]routedReadRegistration{
	"beads list":    {equivalenceTest: "TestBeadsListRouteEquivalence"},
	"beads show":    {equivalenceTest: "TestBeadsShowRouteEquivalence"},
	"convoy list":   {equivalenceTest: "TestConvoyListRouteEquivalence"},
	"convoy status": {equivalenceTest: "TestConvoyStatusRouteEquivalence"},
	"mail check":    {equivalenceTest: "TestMailCheckRouteEquivalence"},
	"mail count":    {equivalenceTest: "TestMailCountRouteEquivalence"},
	"mail peek":     {equivalenceTest: "TestMailPeekRouteEquivalence"},
	"order history": {equivalenceTest: "TestOrderHistoryRouteEquivalence"},
	"ready":         {equivalenceTest: "TestReadyRoutesThroughTheController"},
	"rig list":      {equivalenceTest: "TestRigListRouteEquivalence"},
	"rig status":    {equivalenceTest: "TestRigStatusJSONRouteEquivalence"},
	"session list":  {equivalenceTest: "TestSessionListRouteEquivalence"},
	"session peek":  {equivalenceTest: "TestSessionPeekRouteEquivalence"},
	"status":        {equivalenceTest: "TestCityStatusJSONRouteEquivalence"},
	"wait inspect":  {equivalenceTest: "TestWaitInspectRouteEquivalence"},
	"wait list":     {equivalenceTest: "TestWaitListRouteEquivalence"},

	"convoy check":        {exemptReason: "always reads the live local store because it may close convoys; it never takes the API route"},
	"nudge-dispatch-tick": {exemptReason: "controller-internal dispatch tick, not a user command; prints nothing"},
	"step-reminder":       {exemptReason: "hook step lookup injected into an agent turn as text, not a user read with a JSON contract"},
}

// routeLogHelpers are the functions whose command-name argument identifies a
// routed read. The value is the zero-based index of that argument.
var routeLogHelpers = map[string]int{
	"routeRead":             1,
	"routeReadCmd":          0,
	"routeReadCmdWithHooks": 0,
	"logRoute":              1,
	"logRouteTo":            1,
}

// TestEveryRoutedReadHasARouteEquivalenceTest is the #8453 registry: a command
// that can read through the supervisor API must print the same JSON on that
// route as on the local route, and a route-equivalence test must prove it.
// The test scans the non-test sources for every command name handed to the
// route-logging helpers and requires each to be registered in
// routedReadRegistry with a Test function that exists, or with a reason it has
// no two routes. Registry entries no source routes any more fail too.
func TestEveryRoutedReadHasARouteEquivalenceTest(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := gcCallerDir(currentFile)
	routed, tests := scanRoutedReads(t, dir)

	var problems []string
	for _, name := range routedRegistryKeys(routed) {
		reg, ok := routedReadRegistry[name]
		switch {
		case !ok:
			problems = append(problems, "command "+strconv.Quote(name)+" ("+strings.Join(routed[name], ", ")+
				") reads through the supervisor API but has no route-equivalence test. Add a test in a"+
				" cmd/gc/route_equivalence_<command>_test.go file that runs it on the local route and on the API route"+
				" against the same seeded state and requires identical JSON (modulo additive keys its schema declares),"+
				" then register it in routedReadRegistry (cmd/gc/route_equivalence_registry_test.go)")
		case (reg.equivalenceTest == "") == (reg.exemptReason == ""):
			problems = append(problems, "routedReadRegistry["+strconv.Quote(name)+"] must set exactly one of equivalenceTest or exemptReason")
		case reg.equivalenceTest != "" && !tests[reg.equivalenceTest]:
			problems = append(problems, "routedReadRegistry["+strconv.Quote(name)+"] names "+reg.equivalenceTest+", but no such Test function exists in cmd/gc")
		}
	}
	for _, name := range routedRegistryKeys(routedReadRegistry) {
		if _, ok := routed[name]; !ok {
			problems = append(problems, "routedReadRegistry lists "+strconv.Quote(name)+", but no non-test cmd/gc source routes a command by that name; remove the entry")
		}
	}
	if len(problems) > 0 {
		t.Fatalf("route-equivalence registry (#8453):\n  %s", strings.Join(problems, "\n  "))
	}
}

// TestScanRoutedReadsFindsPackageLevelFuncLiterals pins that the registry scan
// collects a route-logging call inside a package-level function literal, the
// cmd/gc seam idiom (var fooAPIClient = func(...)), as it does one inside a
// function.
func TestScanRoutedReadsFindsPackageLevelFuncLiterals(t *testing.T) {
	dir := t.TempDir()
	src := `package main

import "io"

var probeAPIClient = func(stderr io.Writer) {
	const cmdName = "probe var"
	logRoute(stderr, cmdName, "api", "")
}

func probeCmd(stderr io.Writer) {
	logRoute(stderr, "probe func", "api", "")
}
`
	if err := os.WriteFile(filepath.Join(dir, "probe.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	routed, _ := scanRoutedReads(t, dir)
	for _, name := range []string{"probe var", "probe func"} {
		if _, ok := routed[name]; !ok {
			t.Errorf("scanRoutedReads missed %q; collected %q", name, routedRegistryKeys(routed))
		}
	}
}

// scanRoutedReads parses every .go file in dir. It returns the command names
// non-test files pass to routeLogHelpers (with the file:line of each use) and
// the names of the Test functions test files declare.
func scanRoutedReads(t *testing.T, dir string) (map[string][]string, map[string]bool) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}
	fset := token.NewFileSet()
	routed := map[string][]string{}
	tests := map[string]bool{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		if strings.HasSuffix(name, "_test.go") {
			for _, decl := range file.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
					tests[fn.Name.Name] = true
				}
			}
			continue
		}
		for _, decl := range file.Decls {
			// A package-level declaration is scanned too: a var holding a
			// function literal (the cmd/gc seam idiom) can route a read.
			var scope ast.Node = decl
			if fn, ok := decl.(*ast.FuncDecl); ok {
				if fn.Body == nil {
					continue
				}
				if _, helper := routeLogHelpers[fn.Name.Name]; helper {
					// The helpers forward their own cmdName parameter.
					continue
				}
				scope = fn.Body
			}
			consts := localStringConsts(scope)
			ast.Inspect(scope, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				ident, ok := call.Fun.(*ast.Ident)
				if !ok {
					return true
				}
				argIdx, ok := routeLogHelpers[ident.Name]
				if !ok || argIdx >= len(call.Args) {
					return true
				}
				pos := fset.Position(call.Pos())
				where := filepath.Base(pos.Filename) + ":" + strconv.Itoa(pos.Line)
				cmdName, ok := stringArg(call.Args[argIdx], consts)
				if !ok {
					t.Errorf("%s: %s names its command with an expression this registry cannot read; pass a string literal or a function-local const", where, ident.Name)
					return true
				}
				routed[cmdName] = append(routed[cmdName], where)
				return true
			})
		}
	}
	return routed, tests
}

// localStringConsts maps each string const declared in scope to its value.
func localStringConsts(scope ast.Node) map[string]string {
	out := map[string]string{}
	ast.Inspect(scope, func(n ast.Node) bool {
		gen, ok := n.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			return true
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, ident := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				if s, ok := stringLiteral(vs.Values[i]); ok {
					out[ident.Name] = s
				}
			}
		}
		return true
	})
	return out
}

// stringArg resolves a command-name argument: a string literal or an
// identifier naming a const in consts.
func stringArg(expr ast.Expr, consts map[string]string) (string, bool) {
	if s, ok := stringLiteral(expr); ok {
		return s, true
	}
	if ident, ok := expr.(*ast.Ident); ok {
		s, ok := consts[ident.Name]
		return s, ok
	}
	return "", false
}

func stringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	s, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", false
	}
	return s, true
}

func routedRegistryKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
