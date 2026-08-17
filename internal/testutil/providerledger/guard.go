package providerledger

import (
	"errors"
	"fmt"
	"go/ast"
	"go/build"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// RuntimeRegistration is one builtin runtime selection key and the exact
// constructors returned by its registry factory.
type RuntimeRegistration struct {
	Key          string
	Constructors []SymbolRef
}

const runtimeDoubleBoundaryFile = "fake.go"

// ReusableDouble is one exported provider-double type from the designated type
// boundary and the package-level constructors that return it.
type ReusableDouble struct {
	Type         SymbolRef
	Constructors []SymbolRef
}

type boundFactory struct {
	definition *ast.Ident
	literal    *ast.FuncLit
}

// bindingInfo uses the Go type checker only for lexical definition/use
// identity. Guard inputs are deliberately single-file source snapshots, and
// tests use type-incomplete fixtures, so unrelated type errors are ignored.
// Every binding that matters to a guard is still required explicitly below;
// a missing object therefore fails closed.
type bindingInfo struct {
	types.Info
}

func newBindingInfo(fset *token.FileSet, file *ast.File) *bindingInfo {
	bindings := &bindingInfo{Info: types.Info{
		Defs:      make(map[*ast.Ident]types.Object),
		Uses:      make(map[*ast.Ident]types.Object),
		Implicits: make(map[ast.Node]types.Object),
	}}
	config := types.Config{
		Importer:                 &emptyPackageImporter{packages: make(map[string]*types.Package)},
		DisableUnusedImportCheck: true,
		Error:                    func(error) {},
	}
	_, _ = config.Check(moduleImportPath+"/cmd/gc", fset, []*ast.File{file}, &bindings.Info)
	return bindings
}

type emptyPackageImporter struct {
	packages map[string]*types.Package
}

type standardOrEmptyImporter struct {
	standard types.Importer
	empty    *emptyPackageImporter
}

func (i *standardOrEmptyImporter) Import(importPath string) (*types.Package, error) {
	// Runtime's module-local imports are currently body-only. Empty packages
	// keep those ignored bodies hermetic; any selector used by a declaration
	// remains unresolved and makes the guard fail closed below.
	if strings.HasPrefix(importPath, moduleImportPath+"/") {
		return i.empty.Import(importPath)
	}
	return i.standard.Import(importPath)
}

// DiscoverRuntimeProviderDoubles discovers every exported concrete type in
// internal/runtime/fake.go that implements runtime.Provider. It scans all
// buildable non-test files in that package for exported receiverless
// constructors whose first result implements Provider as an exact discovered
// type or pointer to one.
func DiscoverRuntimeProviderDoubles(runtimeDir string) ([]ReusableDouble, error) {
	entries, err := os.ReadDir(runtimeDir)
	if err != nil {
		return nil, fmt.Errorf("read runtime package %q: %w", runtimeDir, err)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	var boundary *ast.File
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		matches, err := build.Default.MatchFile(runtimeDir, name)
		if err != nil {
			return nil, fmt.Errorf("match runtime package file %q: %w", name, err)
		}
		if !matches {
			continue
		}
		path := filepath.Join(runtimeDir, name)
		source, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read runtime package file %q: %w", name, err)
		}
		file, err := parser.ParseFile(fset, path, source, parser.SkipObjectResolution)
		if err != nil {
			return nil, fmt.Errorf("parse runtime package file %q: %w", name, err)
		}
		if file.Name.Name != "runtime" {
			if name == runtimeDoubleBoundaryFile {
				return nil, fmt.Errorf("%s must declare package runtime", runtimeDoubleBoundaryFile)
			}
			return nil, fmt.Errorf("runtime package file %s must declare package runtime", name)
		}
		if name == runtimeDoubleBoundaryFile {
			boundary = file
		}
		files = append(files, file)
	}
	if boundary == nil {
		return nil, fmt.Errorf("designated runtime double boundary %s is missing", runtimeDoubleBoundaryFile)
	}

	info := types.Info{Defs: make(map[*ast.Ident]types.Object)}
	var typeProblems []string
	config := types.Config{
		Importer: &standardOrEmptyImporter{
			standard: importer.Default(),
			empty:    &emptyPackageImporter{packages: make(map[string]*types.Package)},
		},
		DisableUnusedImportCheck: true,
		IgnoreFuncBodies:         true,
		Error: func(err error) {
			typeProblems = append(typeProblems, err.Error())
		},
	}
	pkg, _ := config.Check(moduleImportPath+"/internal/runtime", fset, files, &info)
	if len(typeProblems) > 0 {
		sort.Strings(typeProblems)
		return nil, fmt.Errorf("type-check runtime double boundary: %s", strings.Join(typeProblems, "; "))
	}
	if pkg == nil {
		return nil, errors.New("type-check runtime double boundary returned no package")
	}
	providerName, ok := pkg.Scope().Lookup("Provider").(*types.TypeName)
	if !ok || providerName.IsAlias() {
		return nil, errors.New("runtime.Provider must be exactly one declared interface")
	}
	providerNamed, ok := providerName.Type().(*types.Named)
	if !ok {
		return nil, errors.New("runtime.Provider must be exactly one declared interface")
	}
	provider, ok := providerNamed.Underlying().(*types.Interface)
	if !ok {
		return nil, errors.New("runtime.Provider must be exactly one declared interface")
	}
	provider.Complete()

	var doubles []ReusableDouble
	trackedTypes := make(map[*types.TypeName]bool)
	for _, decl := range boundary.Decls {
		declaration, ok := decl.(*ast.GenDecl)
		if !ok || declaration.Tok != token.TYPE {
			continue
		}
		for _, spec := range declaration.Specs {
			typeSpec := spec.(*ast.TypeSpec)
			if !typeSpec.Name.IsExported() || typeSpec.Assign.IsValid() {
				continue
			}
			typeName, ok := info.Defs[typeSpec.Name].(*types.TypeName)
			if !ok {
				return nil, fmt.Errorf("resolve exported type %s in %s", typeSpec.Name.Name, runtimeDoubleBoundaryFile)
			}
			named, ok := typeName.Type().(*types.Named)
			if !ok {
				continue
			}
			if named.TypeParams() != nil && named.TypeParams().Len() > 0 {
				return nil, fmt.Errorf("generic exported type %s in %s cannot be classified as a reusable provider double", typeName.Name(), runtimeDoubleBoundaryFile)
			}
			if _, isInterface := named.Underlying().(*types.Interface); isInterface {
				continue
			}
			if !types.Implements(named, provider) && !types.Implements(types.NewPointer(named), provider) {
				continue
			}

			constructors := runtimeDoubleConstructors(files, info, named, provider)
			typeRef := repoSymbol("internal/runtime", typeName.Name())
			if len(constructors) == 0 {
				return nil, fmt.Errorf("runtime provider double %s has no exported receiverless constructor", renderSymbolRef(typeRef))
			}
			doubles = append(doubles, ReusableDouble{Type: typeRef, Constructors: constructors})
			trackedTypes[named.Obj()] = true
		}
	}
	for _, decl := range boundary.Decls {
		declaration, ok := decl.(*ast.GenDecl)
		if !ok || declaration.Tok != token.TYPE {
			continue
		}
		for _, spec := range declaration.Specs {
			typeSpec := spec.(*ast.TypeSpec)
			if !typeSpec.Name.IsExported() || !typeSpec.Assign.IsValid() {
				continue
			}
			typeName, ok := info.Defs[typeSpec.Name].(*types.TypeName)
			if !ok {
				return nil, fmt.Errorf("resolve exported alias %s in %s", typeSpec.Name.Name, runtimeDoubleBoundaryFile)
			}
			canonical, isProvider := runtimeProviderAliasTarget(typeName.Type(), provider)
			if !isProvider {
				continue
			}
			if canonical == nil || !trackedTypes[canonical.Obj()] {
				return nil, fmt.Errorf("exported provider alias %s in %s resolves to an untracked concrete type", typeName.Name(), runtimeDoubleBoundaryFile)
			}
		}
	}
	if len(doubles) == 0 {
		return nil, fmt.Errorf("%s declares no exported runtime.Provider double", runtimeDoubleBoundaryFile)
	}
	sort.Slice(doubles, func(i, j int) bool { return symbolRefLess(doubles[i].Type, doubles[j].Type) })
	return doubles, nil
}

func runtimeDoubleConstructors(files []*ast.File, info types.Info, doubleType *types.Named, provider *types.Interface) []SymbolRef {
	var constructors []SymbolRef
	for _, file := range files {
		for _, decl := range file.Decls {
			function, ok := decl.(*ast.FuncDecl)
			if !ok || function.Recv != nil || !function.Name.IsExported() {
				continue
			}
			object, ok := info.Defs[function.Name].(*types.Func)
			if !ok {
				continue
			}
			signature, ok := object.Type().(*types.Signature)
			if !ok || signature.Results().Len() == 0 {
				continue
			}
			named, ok := runtimeProviderConstructorType(signature.Results().At(0).Type(), provider)
			if !ok || named.Obj() != doubleType.Obj() {
				continue
			}
			constructors = append(constructors, repoSymbol("internal/runtime", function.Name.Name))
		}
	}
	return normalizeSymbolRefs(constructors)
}

func runtimeProviderAliasTarget(alias types.Type, provider *types.Interface) (*types.Named, bool) {
	target := types.Unalias(alias)
	if _, isInterface := target.Underlying().(*types.Interface); isInterface {
		return nil, false
	}
	implements := types.Implements(target, provider)
	if !implements {
		if _, isPointer := target.(*types.Pointer); !isPointer {
			implements = types.Implements(types.NewPointer(target), provider)
		}
	}
	if !implements {
		return nil, false
	}
	switch target := target.(type) {
	case *types.Named:
		return target, true
	case *types.Pointer:
		named, _ := types.Unalias(target.Elem()).(*types.Named)
		return named, true
	default:
		return nil, true
	}
}

func runtimeProviderConstructorType(result types.Type, provider *types.Interface) (*types.Named, bool) {
	result = types.Unalias(result)
	if !types.Implements(result, provider) {
		return nil, false
	}
	switch result := result.(type) {
	case *types.Named:
		return result, true
	case *types.Pointer:
		named, ok := types.Unalias(result.Elem()).(*types.Named)
		return named, ok
	default:
		return nil, false
	}
}

func (i *emptyPackageImporter) Import(importPath string) (*types.Package, error) {
	if imported := i.packages[importPath]; imported != nil {
		return imported, nil
	}
	imported := types.NewPackage(importPath, pathpkg.Base(importPath))
	imported.MarkComplete()
	i.packages[importPath] = imported
	return imported, nil
}

// DiscoverRuntimeCatalog returns literal runtime registry keys and the exact
// constructor symbols returned by their factories inside
// cmd/gc.buildRuntimeRegistry. Dynamic per-city registrations are out of scope
// by design. A fallback is accepted only when its constructor set is already
// owned by one of the explicit registrations.
func DiscoverRuntimeCatalog(source []byte) ([]RuntimeRegistration, error) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "runtime_registry.go", source, parser.SkipObjectResolution)
	if err != nil {
		return nil, fmt.Errorf("parse runtime registry: %w", err)
	}
	var targets []*ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "buildRuntimeRegistry" && fn.Recv == nil {
			targets = append(targets, fn)
		}
	}
	if len(targets) != 1 || targets[0].Body == nil {
		return nil, errors.New("buildRuntimeRegistry must be exactly one receiverless top-level function with a body")
	}
	target := targets[0]
	bindings := newBindingInfo(fset, file)
	imports, err := importAliases(file)
	if err != nil {
		return nil, err
	}
	registryObject, registryDefinition, err := findRuntimeRegistryBinding(target.Body, imports, bindings)
	if err != nil {
		return nil, err
	}
	registryReturn, err := findRuntimeRegistryReturn(target.Body, registryObject, bindings)
	if err != nil {
		return nil, err
	}
	factories := localFactoryLiterals(target.Body, bindings)
	topLevelCalls := directTopLevelCalls(target.Body)
	allowedRegistryUses := map[*ast.Ident]bool{registryDefinition: true, registryReturn: true}
	allowedFactoryUses := make(map[*ast.Ident]bool)
	usedFactories := make(map[types.Object]boundFactory)

	seen := make(map[string]bool)
	var registrations []RuntimeRegistration
	var fallbackConstructors [][]SymbolRef
	var discoverErr error
	ast.Inspect(target.Body, func(node ast.Node) bool {
		if discoverErr != nil {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if selector.Sel.Name != "SetFallback" && selector.Sel.Name != "Register" && selector.Sel.Name != "RegisterPrefix" {
			return true
		}
		receiver, ok := selector.X.(*ast.Ident)
		if !ok || bindings.ObjectOf(receiver) != registryObject {
			discoverErr = fmt.Errorf("catalog mutation receiver is not the bound registry at %s", fset.Position(call.Pos()))
			return false
		}
		if !topLevelCalls[call] {
			discoverErr = fmt.Errorf("catalog mutation at %s must be a direct top-level operation", fset.Position(call.Pos()))
			return false
		}
		allowedRegistryUses[receiver] = true

		switch selector.Sel.Name {
		case "SetFallback":
			if len(call.Args) != 1 {
				discoverErr = fmt.Errorf("SetFallback call at %s must have exactly one factory", fset.Position(call.Pos()))
				return false
			}
			factory, binding, use, err := resolveFactoryLiteral(call.Args[0], factories, bindings)
			if err != nil {
				discoverErr = fmt.Errorf("SetFallback factory at %s: %w", fset.Position(call.Args[0].Pos()), err)
				return false
			}
			recordFactoryUse(binding, use, factories, allowedFactoryUses, usedFactories)
			constructors, err := discoverFactoryConstructors(fset, factory, imports, bindings)
			if err != nil {
				discoverErr = fmt.Errorf("SetFallback factory: %w", err)
				return false
			}
			fallbackConstructors = append(fallbackConstructors, constructors)
			return true
		case "Register", "RegisterPrefix":
		default:
			return true
		}

		if len(call.Args) < 2 {
			discoverErr = fmt.Errorf("%s call at %s must have a catalog key and factory", selector.Sel.Name, fset.Position(call.Pos()))
			return false
		}
		literal, ok := call.Args[0].(*ast.BasicLit)
		if !ok || literal.Kind != token.STRING {
			discoverErr = fmt.Errorf("%s key at %s must be a literal string", selector.Sel.Name, fset.Position(call.Args[0].Pos()))
			return false
		}
		value, err := strconv.Unquote(literal.Value)
		if err != nil {
			discoverErr = fmt.Errorf("unquote %s key at %s: %w", selector.Sel.Name, fset.Position(literal.Pos()), err)
			return false
		}
		kind := "exact:"
		if selector.Sel.Name == "RegisterPrefix" {
			kind = "prefix:"
		}
		key := kind + value
		if seen[key] {
			discoverErr = fmt.Errorf("runtime catalog key %s is registered more than once", key)
			return false
		}
		seen[key] = true
		factory, binding, use, err := resolveFactoryLiteral(call.Args[1], factories, bindings)
		if err != nil {
			discoverErr = fmt.Errorf("runtime catalog key %s factory at %s: %w", key, fset.Position(call.Args[1].Pos()), err)
			return false
		}
		recordFactoryUse(binding, use, factories, allowedFactoryUses, usedFactories)
		constructors, err := discoverFactoryConstructors(fset, factory, imports, bindings)
		if err != nil {
			discoverErr = fmt.Errorf("runtime catalog key %s factory: %w", key, err)
			return false
		}
		registrations = append(registrations, RuntimeRegistration{Key: key, Constructors: constructors})
		return true
	})
	if discoverErr != nil {
		return nil, discoverErr
	}
	if err := validateBoundObjectUses(target.Body, registryObject, allowedRegistryUses, bindings, "registry binding escapes direct catalog operations"); err != nil {
		return nil, err
	}
	for object, factory := range usedFactories {
		allowedFactoryUses[factory.definition] = true
		if err := validateBoundObjectUses(target.Body, object, allowedFactoryUses, bindings, "factory binding escapes direct catalog use"); err != nil {
			return nil, err
		}
	}
	if len(registrations) == 0 {
		return nil, errors.New("no literal runtime registrations found in buildRuntimeRegistry")
	}
	if len(fallbackConstructors) != 1 {
		return nil, fmt.Errorf("buildRuntimeRegistry must contain exactly one SetFallback call, found %d", len(fallbackConstructors))
	}
	for _, fallback := range fallbackConstructors {
		owned := false
		for _, registration := range registrations {
			if equalSymbolRefs(fallback, registration.Constructors) {
				owned = true
				break
			}
		}
		if !owned {
			return nil, fmt.Errorf("runtime fallback constructor set [%s] is not owned by an explicit registration", renderSymbolRefs(fallback))
		}
	}
	sort.Slice(registrations, func(i, j int) bool { return registrations[i].Key < registrations[j].Key })
	return registrations, nil
}

func importAliases(file *ast.File) (map[string]string, error) {
	aliases := make(map[string]string)
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, fmt.Errorf("unquote import path %s: %w", spec.Path.Value, err)
		}
		name := pathpkg.Base(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "." {
			return nil, fmt.Errorf("dot import %q prevents exact constructor identity", importPath)
		}
		if name == "_" {
			continue
		}
		aliases[name] = importPath
	}
	return aliases, nil
}

func findRuntimeRegistryBinding(body *ast.BlockStmt, imports map[string]string, bindings *bindingInfo) (types.Object, *ast.Ident, error) {
	want := repoSymbol("internal/runtime/registry", "New")
	var object types.Object
	var definition *ast.Ident
	for _, stmt := range body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			continue
		}
		call, ok := unparen(assign.Rhs[0]).(*ast.CallExpr)
		if !ok {
			continue
		}
		ref, err := resolveCallSymbol(call, imports, moduleImportPath+"/cmd/gc", bindings)
		if err != nil || ref != want {
			continue
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || bindings.Defs[ident] == nil || assign.Tok != token.DEFINE {
			return nil, nil, errors.New("runtime registry must be one direct local binding of registry.New")
		}
		if object != nil {
			return nil, nil, errors.New("buildRuntimeRegistry declares more than one runtime registry binding")
		}
		object, definition = bindings.Defs[ident], ident
	}
	if object == nil {
		return nil, nil, errors.New("buildRuntimeRegistry has no direct runtime registry binding")
	}
	return object, definition, nil
}

func findRuntimeRegistryReturn(body *ast.BlockStmt, object types.Object, bindings *bindingInfo) (*ast.Ident, error) {
	if len(body.List) == 0 {
		return nil, errors.New("buildRuntimeRegistry does not return its bound registry")
	}
	ret, ok := body.List[len(body.List)-1].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return nil, errors.New("buildRuntimeRegistry must directly return its bound registry")
	}
	ident, ok := ret.Results[0].(*ast.Ident)
	if !ok || bindings.ObjectOf(ident) != object {
		return nil, errors.New("buildRuntimeRegistry must directly return its bound registry")
	}
	return ident, nil
}

func directTopLevelCalls(body *ast.BlockStmt) map[*ast.CallExpr]bool {
	calls := make(map[*ast.CallExpr]bool)
	for _, stmt := range body.List {
		expr, ok := stmt.(*ast.ExprStmt)
		if !ok {
			continue
		}
		ast.Inspect(expr.X, func(node ast.Node) bool {
			if _, nested := node.(*ast.FuncLit); nested {
				return false
			}
			if call, ok := node.(*ast.CallExpr); ok {
				calls[call] = true
			}
			return true
		})
	}
	return calls
}

func validateBoundObjectUses(body *ast.BlockStmt, object types.Object, allowed map[*ast.Ident]bool, bindings *bindingInfo, message string) error {
	if object == nil {
		return fmt.Errorf("%s: required binding is unresolved", message)
	}
	invalid := false
	ast.Inspect(body, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if ok && bindings.ObjectOf(ident) == object && !allowed[ident] {
			invalid = true
			return false
		}
		return true
	})
	if invalid {
		return errors.New(message)
	}
	return nil
}

func localFactoryLiterals(body *ast.BlockStmt, bindings *bindingInfo) map[types.Object]boundFactory {
	factories := make(map[types.Object]boundFactory)
	for _, stmt := range body.List {
		assign, ok := stmt.(*ast.AssignStmt)
		if !ok || assign.Tok != token.DEFINE {
			continue
		}
		for i, lhs := range assign.Lhs {
			if i >= len(assign.Rhs) {
				break
			}
			name, ok := lhs.(*ast.Ident)
			if !ok {
				continue
			}
			factory, ok := assign.Rhs[i].(*ast.FuncLit)
			if object := bindings.Defs[name]; ok && object != nil {
				factories[object] = boundFactory{definition: name, literal: factory}
			}
		}
	}
	return factories
}

func resolveFactoryLiteral(expr ast.Expr, factories map[types.Object]boundFactory, bindings *bindingInfo) (*ast.FuncLit, types.Object, *ast.Ident, error) {
	switch expr := expr.(type) {
	case *ast.FuncLit:
		return expr, nil, nil, nil
	case *ast.Ident:
		object := bindings.ObjectOf(expr)
		factory, ok := factories[object]
		if !ok {
			return nil, nil, nil, fmt.Errorf("factory is not a function literal declared directly in buildRuntimeRegistry")
		}
		return factory.literal, object, expr, nil
	default:
		return nil, nil, nil, fmt.Errorf("factory must be an inline or local function literal, got %T", expr)
	}
}

func recordFactoryUse(object types.Object, use *ast.Ident, factories map[types.Object]boundFactory, allowed map[*ast.Ident]bool, used map[types.Object]boundFactory) {
	if object == nil {
		return
	}
	allowed[use] = true
	used[object] = factories[object]
}

func discoverFactoryConstructors(fset *token.FileSet, factory *ast.FuncLit, imports map[string]string, bindings *bindingInfo) ([]SymbolRef, error) {
	var refs []SymbolRef
	var discoverErr error
	ast.PreorderStack(factory.Body, nil, func(node ast.Node, stack []ast.Node) bool {
		if discoverErr != nil {
			return false
		}
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		ret, ok := node.(*ast.ReturnStmt)
		if !ok {
			return true
		}
		if len(ret.Results) == 0 {
			discoverErr = fmt.Errorf("provider return at %s must directly call its constructor", fset.Position(ret.Pos()))
			return false
		}
		if ident, ok := unparen(ret.Results[0]).(*ast.Ident); ok && ident.Name == "nil" {
			if len(ret.Results) != 2 {
				discoverErr = fmt.Errorf("provider return at %s returns nil without a non-nil error", fset.Position(ret.Pos()))
				return false
			}
			if isNilIdentifier(ret.Results[1], bindings) {
				discoverErr = fmt.Errorf("provider return at %s returns nil provider with nil error", fset.Position(ret.Pos()))
				return false
			}
			if !nilProviderErrorIsGuarded(ret, ret.Results[1], stack, bindings) {
				discoverErr = fmt.Errorf("provider return at %s returns nil provider without a proven non-nil error guard", fset.Position(ret.Pos()))
				return false
			}
			return true
		}
		call, ok := unparen(ret.Results[0]).(*ast.CallExpr)
		if !ok {
			discoverErr = fmt.Errorf("provider return at %s must directly call its constructor", fset.Position(ret.Results[0].Pos()))
			return false
		}
		ref, err := resolveCallSymbol(call, imports, moduleImportPath+"/cmd/gc", bindings)
		if err != nil {
			discoverErr = fmt.Errorf("constructor return at %s: %w", fset.Position(call.Pos()), err)
			return false
		}
		refs = append(refs, ref)
		return true
	})
	if discoverErr != nil {
		return nil, discoverErr
	}
	refs = normalizeSymbolRefs(refs)
	if len(refs) == 0 {
		return nil, errors.New("factory has no direct provider-constructor return")
	}
	return refs, nil
}

func nilProviderErrorIsGuarded(ret *ast.ReturnStmt, errorExpr ast.Expr, stack []ast.Node, bindings *bindingInfo) bool {
	errorIdent, ok := unparen(errorExpr).(*ast.Ident)
	if !ok {
		return false
	}
	errorObject := bindings.ObjectOf(errorIdent)
	if errorObject == nil || errorObject == types.Universe.Lookup("nil") {
		return false
	}
	for i := len(stack) - 1; i >= 0; i-- {
		ifStmt, ok := stack[i].(*ast.IfStmt)
		if !ok || len(ifStmt.Body.List) != 1 || ifStmt.Body.List[0] != ret {
			continue
		}
		if conditionProvesNonNil(ifStmt.Cond, errorObject, bindings) {
			return true
		}
	}
	return false
}

func conditionProvesNonNil(expr ast.Expr, object types.Object, bindings *bindingInfo) bool {
	expr = unparen(expr)
	binary, ok := expr.(*ast.BinaryExpr)
	if !ok {
		return false
	}
	if binary.Op != token.NEQ {
		return false
	}
	return (isBoundIdentifier(binary.X, object, bindings) && isNilIdentifier(binary.Y, bindings)) ||
		(isNilIdentifier(binary.X, bindings) && isBoundIdentifier(binary.Y, object, bindings))
}

func isBoundIdentifier(expr ast.Expr, object types.Object, bindings *bindingInfo) bool {
	ident, ok := unparen(expr).(*ast.Ident)
	return ok && bindings.ObjectOf(ident) == object
}

func isNilIdentifier(expr ast.Expr, bindings *bindingInfo) bool {
	ident, ok := unparen(expr).(*ast.Ident)
	return ok && ident.Name == "nil" && bindings.ObjectOf(ident) == types.Universe.Lookup("nil")
}

func unparen(expr ast.Expr) ast.Expr {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}
		expr = paren.X
	}
}

// CompareRuntimeCatalog checks discovered production registrations against the
// ledger in both directions.
func CompareRuntimeCatalog(entries []Entry, discovered []RuntimeRegistration) error {
	ledger := make(map[string][]SymbolRef)
	var problems []string
	for _, entry := range entries {
		if entry.Catalog == nil {
			continue
		}
		if entry.Catalog.Name != RuntimeBuiltinCatalog {
			problems = append(problems, fmt.Sprintf("entry %q has unknown catalog %q", entry.ID, entry.Catalog.Name))
			continue
		}
		if !hasRole(entry.Roles, RoleProductionProvider) {
			problems = append(problems, fmt.Sprintf("entry %q catalog binding requires role production_provider", entry.ID))
			continue
		}
		ledger[entry.Catalog.Key] = entry.Constructors
	}
	production := make(map[string][]SymbolRef)
	for _, registration := range discovered {
		production[registration.Key] = registration.Constructors
	}

	for key, constructors := range production {
		declared, ok := ledger[key]
		if !ok {
			problems = append(problems, fmt.Sprintf("runtime builtin %s is missing from the ledger", key))
			continue
		}
		if !equalSymbolRefs(declared, constructors) {
			problems = append(problems, fmt.Sprintf(
				"runtime builtin %s constructor set is [%s], ledger declares [%s]",
				key,
				renderSymbolRefs(constructors),
				renderSymbolRefs(declared),
			))
		}
	}
	for key := range ledger {
		if _, ok := production[key]; !ok {
			problems = append(problems, fmt.Sprintf("ledger runtime builtin %s is not registered in buildRuntimeRegistry", key))
		}
	}
	sort.Strings(problems)
	return joinProblems(problems)
}

// CompareReusableDoubles checks discovered reusable-double constructors and
// their concrete types against reusable-double ledger ownership in both
// directions.
func CompareReusableDoubles(entries []Entry, discovered []ReusableDouble) error {
	type owner struct {
		entryID    string
		doubleType SymbolRef
	}

	ledger := make(map[SymbolRef][]owner)
	var problems []string
	for _, entry := range entries {
		if !hasRole(entry.Roles, RoleReusableDouble) {
			continue
		}
		if entry.DoubleType == nil {
			problems = append(problems, fmt.Sprintf("entry %q reusable_double role requires a double type", entry.ID))
			continue
		}
		if entry.DoubleBoundary != runtimeDoubleBoundaryPath {
			problems = append(problems, fmt.Sprintf("entry %q reusable double boundary is %q, want %q", entry.ID, entry.DoubleBoundary, runtimeDoubleBoundaryPath))
			continue
		}
		for _, constructor := range entry.Constructors {
			ledger[constructor] = append(ledger[constructor], owner{entryID: entry.ID, doubleType: *entry.DoubleType})
		}
	}

	production := make(map[SymbolRef][]SymbolRef)
	seenTypes := make(map[SymbolRef]bool)
	for _, double := range discovered {
		if seenTypes[double.Type] {
			problems = append(problems, fmt.Sprintf("runtime provider double %s is discovered more than once", renderSymbolRef(double.Type)))
		}
		seenTypes[double.Type] = true
		if len(double.Constructors) == 0 {
			problems = append(problems, fmt.Sprintf("runtime provider double %s has no exported receiverless constructor", renderSymbolRef(double.Type)))
		}
		seenConstructors := make(map[SymbolRef]bool)
		for _, constructor := range double.Constructors {
			if seenConstructors[constructor] {
				problems = append(problems, fmt.Sprintf("runtime provider double %s repeats constructor %s", renderSymbolRef(double.Type), renderSymbolRef(constructor)))
			}
			seenConstructors[constructor] = true
			production[constructor] = append(production[constructor], double.Type)
		}
	}

	for constructor, doubleTypes := range production {
		if len(doubleTypes) > 1 {
			problems = append(problems, fmt.Sprintf("reusable double %s constructs multiple declared types: %s", renderSymbolRef(constructor), renderSymbolRefs(doubleTypes)))
			continue
		}
		owners := ledger[constructor]
		sort.Slice(owners, func(i, j int) bool { return owners[i].entryID < owners[j].entryID })
		switch len(owners) {
		case 0:
			problems = append(problems, fmt.Sprintf("reusable double %s is missing from the ledger", renderSymbolRef(constructor)))
		case 1:
			if owners[0].doubleType != doubleTypes[0] {
				problems = append(problems, fmt.Sprintf(
					"reusable double %s constructs %s, ledger declares %s",
					renderSymbolRef(constructor),
					renderSymbolRef(doubleTypes[0]),
					renderSymbolRef(owners[0].doubleType),
				))
			}
		default:
			ids := make([]string, len(owners))
			for i, owner := range owners {
				ids[i] = strconv.Quote(owner.entryID)
			}
			problems = append(problems, fmt.Sprintf("reusable double %s is owned by multiple ledger entries: %s", renderSymbolRef(constructor), strings.Join(ids, ", ")))
		}
	}
	for constructor := range ledger {
		if _, ok := production[constructor]; !ok {
			problems = append(problems, fmt.Sprintf("ledger reusable double %s is not discovered for type boundary %s", renderSymbolRef(constructor), runtimeDoubleBoundaryPath))
		}
	}
	return joinProblems(problems)
}

// ValidateSourceRefs checks production compositions that live outside an
// explicit registry against their exact source function and constructor flow.
func ValidateSourceRefs(root string, entries []Entry) error {
	var problems []string
	for _, entry := range entries {
		if entry.Source == nil {
			continue
		}
		if err := validateSourceRef(root, entry.Constructors, *entry.Source); err != nil {
			problems = append(problems, fmt.Sprintf("entry %q source binding: %v", entry.ID, err))
		}
	}
	return joinProblems(problems)
}

func validateSourceRef(root string, constructors []SymbolRef, source SourceRef) error {
	if filepath.IsAbs(source.File) || strings.HasPrefix(filepath.ToSlash(filepath.Clean(source.File)), "../") {
		return fmt.Errorf("source file %q must be repository-relative", source.File)
	}
	path := filepath.Join(root, source.File)
	contents, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read source file %q: %w", source.File, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, contents, parser.SkipObjectResolution)
	if err != nil {
		return fmt.Errorf("parse source file %q: %w", source.File, err)
	}
	imports, err := importAliases(file)
	if err != nil {
		return fmt.Errorf("parse source imports in %q: %w", source.File, err)
	}
	bindings := newBindingInfo(fset, file)
	var targets []*ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == source.Function && fn.Recv == nil {
			targets = append(targets, fn)
		}
	}
	if len(targets) != 1 || targets[0].Body == nil {
		return fmt.Errorf("source function %s must be exactly one top-level function with a body", source.Function)
	}
	target := targets[0]

	expected := normalizeSymbolRefs(append([]SymbolRef(nil), constructors...))
	expectedPaths := make(map[string]bool)
	for _, constructor := range expected {
		expectedPaths[constructor.ImportPath] = true
	}
	var calls []*ast.CallExpr
	var discovered []SymbolRef
	ast.Inspect(target.Body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		ref, err := resolveCallSymbol(call, imports, moduleImportPath+"/cmd/gc", bindings)
		if err == nil && expectedPaths[ref.ImportPath] {
			calls = append(calls, call)
			discovered = append(discovered, ref)
		}
		return true
	})
	if len(expected) == 1 && len(calls) != 1 {
		return fmt.Errorf("source function %s requires exactly one constructor call to %s, found [%s]", source.Function, renderSymbolRef(expected[0]), renderSymbolRefs(discovered))
	}
	if len(calls) != len(expected) || !equalSymbolRefs(discovered, expected) {
		return fmt.Errorf("source function %s constructor calls are [%s], want [%s]", source.Function, renderSymbolRefs(discovered), renderSymbolRefs(expected))
	}
	for i, call := range calls {
		if err := validateSourceConstructorFlow(target.Body, call, expected[i], bindings); err != nil {
			return err
		}
	}
	return nil
}

func validateSourceConstructorFlow(body *ast.BlockStmt, constructorCall *ast.CallExpr, constructor SymbolRef, bindings *bindingInfo) error {
	var definition *ast.Ident
	var bindingBlock *ast.BlockStmt
	bindingIndex := -1
	directReturn := false
	ast.Inspect(body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return true
		}
		for i, stmt := range block.List {
			switch stmt := stmt.(type) {
			case *ast.AssignStmt:
				if len(stmt.Lhs) == 1 && len(stmt.Rhs) == 1 && unparen(stmt.Rhs[0]) == constructorCall && stmt.Tok == token.DEFINE {
					if ident, ok := stmt.Lhs[0].(*ast.Ident); ok && bindings.Defs[ident] != nil {
						definition = ident
						bindingBlock = block
						bindingIndex = i
					}
				}
			case *ast.ReturnStmt:
				if len(stmt.Results) > 0 && unparen(stmt.Results[0]) == constructorCall {
					directReturn = true
				}
			}
		}
		return true
	})
	if directReturn {
		return nil
	}
	if definition == nil {
		return fmt.Errorf("source constructor %s must directly return or bind its constructor result", renderSymbolRef(constructor))
	}
	if bindingIndex >= len(bindingBlock.List)-1 {
		return fmt.Errorf("source constructor %s result is not returned", renderSymbolRef(constructor))
	}
	finalReturn, ok := bindingBlock.List[len(bindingBlock.List)-1].(*ast.ReturnStmt)
	if !ok || len(finalReturn.Results) == 0 {
		if sourceObjectIsReturned(bindingBlock, bindings.Defs[definition], bindings) {
			return fmt.Errorf("source constructor %s requires an unconditional direct return in the same lexical block", renderSymbolRef(constructor))
		}
		return fmt.Errorf("source constructor %s result is not returned", renderSymbolRef(constructor))
	}
	returnedIdent, ok := unparen(finalReturn.Results[0]).(*ast.Ident)
	if !ok || bindings.ObjectOf(returnedIdent) != bindings.Defs[definition] {
		if sourceObjectIsReturned(bindingBlock, bindings.Defs[definition], bindings) {
			return fmt.Errorf("source constructor %s requires an unconditional direct return in the same lexical block", renderSymbolRef(constructor))
		}
		return fmt.Errorf("source constructor %s result is not returned", renderSymbolRef(constructor))
	}
	for _, stmt := range bindingBlock.List[bindingIndex+1 : len(bindingBlock.List)-1] {
		earlyReturn := false
		ast.Inspect(stmt, func(node ast.Node) bool {
			if _, nested := node.(*ast.FuncLit); nested {
				return false
			}
			if _, ok := node.(*ast.ReturnStmt); ok {
				earlyReturn = true
				return false
			}
			return true
		})
		if earlyReturn {
			return fmt.Errorf("source constructor %s requires an unconditional direct return in the same lexical block", renderSymbolRef(constructor))
		}
	}

	allowedUses := map[*ast.Ident]bool{definition: true, returnedIdent: true}
	ast.PreorderStack(body, nil, func(node ast.Node, stack []ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		if call, ok := node.(*ast.CallExpr); ok {
			for _, ancestor := range stack {
				if _, asynchronous := ancestor.(*ast.GoStmt); asynchronous {
					return true
				}
			}
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
				if receiver, ok := selector.X.(*ast.Ident); ok && bindings.ObjectOf(receiver) == bindings.Defs[definition] {
					allowedUses[receiver] = true
				}
			}
		}
		return true
	})
	if err := validateBoundObjectUses(body, bindings.Defs[definition], allowedUses, bindings, "source constructor result escapes its direct return path"); err != nil {
		return err
	}
	return nil
}

func sourceObjectIsReturned(body *ast.BlockStmt, object types.Object, bindings *bindingInfo) bool {
	returned := false
	ast.Inspect(body, func(node ast.Node) bool {
		if _, nested := node.(*ast.FuncLit); nested {
			return false
		}
		ret, ok := node.(*ast.ReturnStmt)
		if !ok || len(ret.Results) == 0 {
			return true
		}
		if ident, ok := unparen(ret.Results[0]).(*ast.Ident); ok && bindings.ObjectOf(ident) == object {
			returned = true
			return false
		}
		return true
	})
	return returned
}

func resolveCallSymbol(call *ast.CallExpr, imports map[string]string, localImportPath string, bindings *bindingInfo) (SymbolRef, error) {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		if object := bindings.ObjectOf(fun); object != nil {
			if _, ok := object.(*types.Func); !ok {
				return SymbolRef{}, fmt.Errorf("%s resolves to a local %T, not a declared function", fun.Name, object)
			}
		}
		return SymbolRef{ImportPath: localImportPath, Name: fun.Name}, nil
	case *ast.SelectorExpr:
		qualifier, ok := fun.X.(*ast.Ident)
		if !ok {
			return SymbolRef{}, fmt.Errorf("constructor selector receiver must be an import identifier")
		}
		pkgName, ok := bindings.ObjectOf(qualifier).(*types.PkgName)
		if !ok {
			return SymbolRef{}, fmt.Errorf("selector receiver %s is not an imported package", qualifier.Name)
		}
		importPath := pkgName.Imported().Path()
		if importPath == "" || imports[qualifier.Name] != importPath {
			return SymbolRef{}, fmt.Errorf("selector receiver %s is not an imported package", qualifier.Name)
		}
		return SymbolRef{ImportPath: importPath, Name: fun.Sel.Name}, nil
	default:
		return SymbolRef{}, fmt.Errorf("constructor must be a direct function call, got %T", call.Fun)
	}
}
