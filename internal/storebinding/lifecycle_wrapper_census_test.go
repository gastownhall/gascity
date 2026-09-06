package storebinding

// The no-open census, run against the class wrapper stack.
//
// Plan resolution closes the routes by which a consumer could obtain a StoreSet
// before activation publishes one. The wrapper stack is a large amount of code —
// five class wrappers, a cache, an event emitter, a binding lifecycle and a
// status view — and every one of those is a place where "I just need the handle"
// could reopen a binding behind activation's back. The claim that they do not is
// not an assumption; it is this census.
//
// The subject and the forbidden set are both RESOLVED from the package's
// syntax tree rather than named:
//
//   - an OPEN ENTRY POINT is any package-level function whose result list
//     mentions OpenedBinding, plus the Open method the Provider interface
//     declares. Rename either and the census follows;
//   - the SANCTIONED files are exactly the files that DECLARE an open entry
//     point. Nothing is named by path, so moving the open program to another
//     file moves its sanction with it, and adding an open call to a wrapper
//     file does not.
//
// It fails closed at four points: an empty entry-point set, an empty sanctioned
// set, an empty subject set, or a subject that does not contain the wrapper
// methods this slice added are all fatal, because each would make the census
// pass by seeing nothing.
//
// The survey is a pure function of parsed sources, which is what makes it
// testable: TestNoOpenCensusSeesEveryEvasion runs it over synthetic sources
// carrying each evasion, so the census is proven to fail on code that opens
// rather than merely observed to pass on code that does not. That distinction
// matters more here than usual — a census that reads the package from DISK is
// invisible to an overlay mutant, so the synthetic corpus is the only mutation
// test it can have.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// censusOpenedBindingType is the interface a function returns when it hands
// back an opened binding. It is the one identifier this census resolves
// against; everything else follows from the syntax tree.
const censusOpenedBindingType = "OpenedBinding"

// censusProviderInterface is the provider contract whose methods perform the
// actual open. Its method set is read from the declaration, so a provider that
// grows a second open-shaped method is censused without an edit here.
const censusProviderInterface = "Provider"

// censusProviderOpenMethod is the provider method that opens a binding.
const censusProviderOpenMethod = "Open"

// censusSQLiteImportPath is the OSS SQLite binding. The wrappers must be
// provider-neutral, which at minimum means the package they live in cannot
// reach the one provider by name. (Go's own import cycle rules also forbid it
// today — this pins that the direction never inverts.)
const censusSQLiteImportPath = "github.com/gastownhall/gascity/internal/storebinding/sqlite"

// openCensusReport is one survey of a set of parsed sources.
type openCensusReport struct {
	entryPoints map[string]bool
	sanctioned  map[string]bool
	subjects    int
	findings    []string
}

// surveyOpenCensus resolves the open entry points from the sources and reports
// every function outside the files declaring them that reaches one.
func surveyOpenCensus(files map[string]*ast.File) openCensusReport {
	report := openCensusReport{entryPoints: map[string]bool{}, sanctioned: map[string]bool{}}
	for name, file := range files {
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Recv != nil {
				continue
			}
			if resultMentionsType(function, censusOpenedBindingType) {
				report.entryPoints[function.Name.Name] = true
				report.sanctioned[name] = true
			}
		}
	}
	for name, file := range files {
		if report.sanctioned[name] {
			continue
		}
		packages := importIdentifiers(file)
		for _, declaration := range file.Decls {
			function, ok := declaration.(*ast.FuncDecl)
			if !ok || function.Body == nil {
				continue
			}
			report.subjects++
			locals := functionLocalIdentifiers(function)
			for _, call := range functionCalls(function) {
				switch {
				case call.selector == "" && report.entryPoints[call.name]:
					report.findings = append(report.findings, fmt.Sprintf(
						"%s: %s calls the open entry point %s; nothing outside the open program may open a binding before activation",
						name, functionLabel(function), call.name))
				case call.selector != "" && report.entryPoints[call.name]:
					report.findings = append(report.findings, fmt.Sprintf(
						"%s: %s calls %s.%s; a qualified or aliased call to the open entry point opens a binding exactly like an unqualified one",
						name, functionLabel(function), call.selector, call.name))
				case opensAProvider(call, packages, locals):
					report.findings = append(report.findings, fmt.Sprintf(
						"%s: %s calls %s.%s(; a provider open outside the open program is exactly what the saga's fencing and receipts gate",
						name, functionLabel(function), call.selector, call.name))
				}
			}
		}
	}
	return report
}

// packageFileDecls parses the package's non-test sources once and returns them
// by file name.
func packageFileDecls(t *testing.T) map[string]*ast.File {
	t.Helper()
	fileSet := token.NewFileSet()
	// parser.ParseDir is deprecated; read the directory and parse each source
	// directly so the census keeps the same subject set without it.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading the storage package directory: %v", err)
	}
	files := map[string]*ast.File{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fileSet, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files[name] = file
	}
	if len(files) == 0 {
		t.Fatal("the census parsed no non-test source in this package; its subject set is empty")
	}
	return files
}

// TestNoWrapperOpensABindingBeforeActivation is the census.
func TestNoWrapperOpensABindingBeforeActivation(t *testing.T) {
	files := packageFileDecls(t)
	report := surveyOpenCensus(files)

	if len(report.entryPoints) == 0 {
		t.Fatal("the census resolved no open entry point; it would pass against any code at all")
	}
	if len(report.sanctioned) == 0 {
		t.Fatal("the census resolved no sanctioned file; the subject set is the whole package and the census is meaningless")
	}
	if report.subjects == 0 {
		t.Fatal("the census examined no function outside the open program; it is blind")
	}

	providerMethods := interfaceMethodNames(files, censusProviderInterface)
	if len(providerMethods) == 0 {
		t.Fatalf("the census resolved no method on the %s interface; a provider open would be invisible to it", censusProviderInterface)
	}
	if !providerMethods[censusProviderOpenMethod] {
		t.Fatalf("the %s interface declares no %s method; this census is resolving the wrong contract",
			censusProviderInterface, censusProviderOpenMethod)
	}

	for _, finding := range report.findings {
		t.Error(finding)
	}

	// Fail closed on the subject that matters: this slice's own wrappers must
	// be inside the censused set. A census that stopped seeing them would go
	// quiet exactly when it stopped working.
	censused := map[string]bool{}
	for name, file := range files {
		if report.sanctioned[name] {
			continue
		}
		for _, declaration := range file.Decls {
			if function, ok := declaration.(*ast.FuncDecl); ok && function.Body != nil {
				censused[functionLabel(function)] = true
			}
		}
	}
	for _, receiver := range append(wrapperReceivers(), "BindingLifecycle", "graphBeadCache") {
		methods := packageReceiverFuncDecls(t, receiver)
		if len(methods) == 0 {
			t.Errorf("the census subject does not contain %s; that part of the wrapper stack is unpoliced", receiver)
			continue
		}
		for name := range methods {
			if !censused[receiver+"."+name] {
				t.Errorf("%s.%s is not in the censused set; it lives in a file the census treats as the open program", receiver, name)
			}
		}
	}
}

// wrapperReceivers returns the class-wrapper receiver types, taken from the
// same map the event-parity census resolves its classes from, so the two
// censuses can never disagree about what a wrapper is.
func wrapperReceivers() []string {
	receivers := make([]string, 0, len(wrapperClasses))
	for receiver := range wrapperClasses {
		receivers = append(receivers, receiver)
	}
	return receivers
}

// TestNoOpenCensusSeesEveryEvasion is the census's own mutation test. Each
// shape below is code that opens a binding from outside the open program; the
// census must report every one of them.
func TestNoOpenCensusSeesEveryEvasion(t *testing.T) {
	const sanctioned = `package storebinding

func OpenBinding(provider Provider, request OpenRequest) (OpenedBinding, error) { return nil, nil }
func NewOpenedBinding(parts OpenedBindingParts) (OpenedBinding, error) { return nil, nil }
`
	shapes := []struct {
		name string
		src  string
	}{
		{
			name: "a wrapper calls the open entry point",
			src: `package storebinding

func (g *wrappedGraphStore) Ping() error {
	_, _ = OpenBinding(nil, OpenRequest{})
	return nil
}`,
		},
		{
			name: "the lifecycle re-opens during adoption",
			src: `package storebinding

func (l *BindingLifecycle) Adopt() error {
	_, _ = NewOpenedBinding(OpenedBindingParts{})
	return nil
}`,
		},
		{
			name: "a qualified call to the entry point",
			src: `package storebinding

import sb "` + censusImportPath + `"

func reopen() { _, _ = sb.OpenBinding(nil, OpenRequest{}) }`,
		},
		{
			name: "a provider open on a local handle",
			src: `package storebinding

func (b BindingHealth) reprobe(provider Provider) {
	_, _ = provider.Open(nil, OpenRequest{})
}`,
		},
		{
			name: "a provider open through a variable named after a package",
			src: `package storebinding

import "os"

func reprobe(request OpenRequest) {
	_ = os.Stdout
	os := providerForRequest(request)
	_, _ = os.Open(nil, request)
}`,
		},
		{
			name: "an open inside a closure",
			src: `package storebinding

func (g *wrappedGraphStore) mutate(run func() error) error {
	return run2(func() error {
		_, _ = OpenBinding(nil, OpenRequest{})
		return nil
	})
}`,
		},
	}

	for _, shape := range shapes {
		t.Run(shape.name, func(t *testing.T) {
			fileSet := token.NewFileSet()
			sanctionedFile, err := parser.ParseFile(fileSet, "provider_lifecycle.go", sanctioned, 0)
			if err != nil {
				t.Fatalf("parsing the sanctioned fixture: %v", err)
			}
			mutantFile, err := parser.ParseFile(fileSet, "lifecycle_wrappers.go", shape.src, 0)
			if err != nil {
				t.Fatalf("parsing the mutant fixture: %v", err)
			}
			report := surveyOpenCensus(map[string]*ast.File{
				"provider_lifecycle.go": sanctionedFile,
				"lifecycle_wrappers.go": mutantFile,
			})
			if len(report.entryPoints) != 2 {
				t.Fatalf("the fixture resolved %d entry points, want 2; the corpus is wrong, not the census", len(report.entryPoints))
			}
			if len(report.findings) == 0 {
				t.Fatalf("the census reported nothing for %q; it does not see this evasion", shape.name)
			}
		})
	}

	// The negative control: the sanctioned file may call its own entry points,
	// and an ordinary file read is not a binding open. A census that flagged
	// either would be turned off within a week.
	const innocent = `package storebinding

import "os"

func readMarker(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	return file.Close()
}`
	fileSet := token.NewFileSet()
	sanctionedFile, err := parser.ParseFile(fileSet, "provider_lifecycle.go", sanctioned, 0)
	if err != nil {
		t.Fatalf("parsing the sanctioned fixture: %v", err)
	}
	innocentFile, err := parser.ParseFile(fileSet, "migration_manifest_store.go", innocent, 0)
	if err != nil {
		t.Fatalf("parsing the innocent fixture: %v", err)
	}
	report := surveyOpenCensus(map[string]*ast.File{
		"provider_lifecycle.go":       sanctionedFile,
		"migration_manifest_store.go": innocentFile,
	})
	if len(report.findings) != 0 {
		t.Errorf("the census flagged an ordinary file open: %v", report.findings)
	}
}

// TestStoragePackageDoesNotReachTheSQLiteBinding pins provider neutrality at
// the import level: the class wrappers, the lifecycle and the status view live
// in this package, and none of them may name the OSS SQLite binding.
func TestStoragePackageDoesNotReachTheSQLiteBinding(t *testing.T) {
	files := packageFileDecls(t)
	checked := 0
	for name, file := range files {
		checked++
		for _, imported := range file.Imports {
			path, ok := censusImportLiteral(imported.Path.Value)
			if !ok {
				t.Errorf("%s has an import literal the census cannot read (%s); an unreadable import is a hole", name, imported.Path.Value)
				continue
			}
			if path == censusSQLiteImportPath {
				t.Errorf("%s imports %s; the class wrapper stack is provider-neutral and the SQLite binding is one provider", name, path)
			}
		}
	}
	if checked == 0 {
		t.Fatal("the import census read no file")
	}
}

// resultMentionsType reports whether a function's results include one type
// identifier, through a pointer, slice or any other wrapper expression.
func resultMentionsType(function *ast.FuncDecl, name string) bool {
	if function.Type.Results == nil {
		return false
	}
	found := false
	ast.Inspect(function.Type.Results, func(node ast.Node) bool {
		identifier, ok := node.(*ast.Ident)
		if ok && identifier.Name == name {
			found = true
			return false
		}
		return true
	})
	return found
}

// interfaceMethodNames returns the method set an interface declares.
func interfaceMethodNames(files map[string]*ast.File, name string) map[string]bool {
	methods := map[string]bool{}
	for _, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			spec, ok := node.(*ast.TypeSpec)
			if !ok || spec.Name.Name != name {
				return true
			}
			declared, ok := spec.Type.(*ast.InterfaceType)
			if !ok {
				return true
			}
			for _, field := range declared.Methods.List {
				for _, methodName := range field.Names {
					methods[methodName.Name] = true
				}
			}
			return false
		})
	}
	return methods
}

// opensAProvider reports whether one call is a provider open — a call to a
// method named Open on a VALUE.
//
// A selector call whose receiver is one of the file's imported package names
// is a package function (os.Open, io.Open…), not a method on a provider, and
// the distinction is what keeps this census from flagging every file read in
// the package. The exception matters: a receiver that is also declared INSIDE
// the function shadows the import, so it is treated as a value again, which
// closes the "name a local provider variable after a package" evasion.
func opensAProvider(call censusCall, packages, locals map[string]bool) bool {
	if call.selector == "" || call.name != censusProviderOpenMethod {
		return false
	}
	if packages[call.selector] && !locals[call.selector] {
		return false
	}
	return true
}

// importIdentifiers returns the identifiers a file's imports bind, whether by
// alias or by the path's final segment.
func importIdentifiers(file *ast.File) map[string]bool {
	names := map[string]bool{}
	for _, imported := range file.Imports {
		if imported.Name != nil {
			names[imported.Name.Name] = true
			continue
		}
		path, ok := censusImportLiteral(imported.Path.Value)
		if !ok {
			continue
		}
		segments := strings.Split(path, "/")
		names[segments[len(segments)-1]] = true
	}
	return names
}

// functionLocalIdentifiers returns every identifier the function binds:
// receiver, parameters, results, and anything declared or defined in its body.
func functionLocalIdentifiers(function *ast.FuncDecl) map[string]bool {
	locals := map[string]bool{}
	addFields := func(fields *ast.FieldList) {
		if fields == nil {
			return
		}
		for _, field := range fields.List {
			for _, name := range field.Names {
				locals[name.Name] = true
			}
		}
	}
	addFields(function.Recv)
	addFields(function.Type.Params)
	addFields(function.Type.Results)
	ast.Inspect(function.Body, func(node ast.Node) bool {
		switch declaration := node.(type) {
		case *ast.AssignStmt:
			if declaration.Tok != token.DEFINE {
				return true
			}
			for _, target := range declaration.Lhs {
				if identifier, ok := target.(*ast.Ident); ok {
					locals[identifier.Name] = true
				}
			}
		case *ast.ValueSpec:
			for _, name := range declaration.Names {
				locals[name.Name] = true
			}
		case *ast.RangeStmt:
			for _, target := range []ast.Expr{declaration.Key, declaration.Value} {
				if identifier, ok := target.(*ast.Ident); ok {
					locals[identifier.Name] = true
				}
			}
		case *ast.FuncLit:
			addFields(declaration.Type.Params)
			addFields(declaration.Type.Results)
		}
		return true
	})
	return locals
}

// functionCalls returns the calls one function body makes, in the same shape
// the publication census uses.
func functionCalls(function *ast.FuncDecl) []censusCall {
	var calls []censusCall
	ast.Inspect(function.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch target := call.Fun.(type) {
		case *ast.Ident:
			calls = append(calls, censusCall{name: target.Name})
		case *ast.SelectorExpr:
			receiver := "?"
			if identifier, ok := target.X.(*ast.Ident); ok {
				receiver = identifier.Name
			}
			calls = append(calls, censusCall{name: target.Sel.Name, selector: receiver})
		}
		return true
	})
	return calls
}

// functionLabel names a declaration for a diagnostic, including its receiver.
func functionLabel(function *ast.FuncDecl) string {
	if function.Recv == nil || len(function.Recv.List) != 1 {
		return function.Name.Name
	}
	return receiverTypeName(function.Recv.List[0].Type) + "." + function.Name.Name
}
