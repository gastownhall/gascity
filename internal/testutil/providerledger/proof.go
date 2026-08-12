package providerledger

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ValidateProofRefs verifies that every proved claim names one runnable test
// whose final top-level statement invokes the declared contract runner with an
// inline factory that returns the exact constructor directly. The deliberately
// narrow shape makes pre-run gates and silent skips visible instead of trying
// to infer arbitrary helper behavior.
func ValidateProofRefs(root string, entries []Entry) error {
	var problems []string
	for _, entry := range entries {
		for _, claim := range entry.Claims {
			if claim.Disposition != DispositionProved {
				continue
			}
			if claim.Proof == nil {
				problems = append(problems, fmt.Sprintf("entry %q contract %s: proved claim has no proof", entry.ID, claim.Contract))
				continue
			}
			if err := validateProofRef(root, claim.Constructor, *claim.Proof); err != nil {
				problems = append(problems, fmt.Sprintf("entry %q contract %s: %v", entry.ID, claim.Contract, err))
			}
		}
	}
	return joinProblems(problems)
}

func validateProofRef(root string, constructor SymbolRef, proof ProofRef) error {
	clean := filepath.ToSlash(filepath.Clean(proof.File))
	if filepath.IsAbs(proof.File) || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("proof file %q must be repository-relative", proof.File)
	}
	if !strings.HasSuffix(filepath.Base(proof.File), "_test.go") {
		return fmt.Errorf("proof file %q must name a _test.go file", proof.File)
	}

	path := filepath.Join(root, proof.File)
	source, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read proof file %q: %w", proof.File, err)
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, source, 0)
	if err != nil {
		return fmt.Errorf("parse proof file %q: %w", proof.File, err)
	}
	imports, err := importAliases(file)
	if err != nil {
		return fmt.Errorf("parse proof imports in %q: %w", proof.File, err)
	}
	localImportPath := moduleImportPath
	if dir := filepath.ToSlash(filepath.Dir(proof.File)); dir != "." {
		localImportPath += "/" + dir
	}
	// Go compiles an external test package under a distinct synthetic import
	// identity. Preserve that distinction so a package-local wrapper in
	// runtime_test cannot masquerade as internal/runtime.NewFake.
	if strings.HasSuffix(file.Name.Name, "_test") {
		localImportPath += "_test"
	}

	var matches []*ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == proof.Test {
			matches = append(matches, fn)
		}
	}
	if len(matches) != 1 {
		return fmt.Errorf("proof test %s must appear exactly once in %s; found %d", proof.Test, proof.File, len(matches))
	}
	return validateProofFunction(matches[0], constructor, proof, imports, localImportPath)
}

func validateProofFunction(fn *ast.FuncDecl, constructor SymbolRef, proof ProofRef, imports map[string]string, localImportPath string) error {
	if fn.Recv != nil || fn.Body == nil {
		return fmt.Errorf("proof %s must be a top-level function with a body", proof.Test)
	}
	if !isRunnableProofTestName(fn.Name.Name) {
		return fmt.Errorf("proof %s is not a runnable Go test name", proof.Test)
	}
	if fn.Type.TypeParams != nil && len(fn.Type.TypeParams.List) != 0 {
		return fmt.Errorf("proof %s must not declare type parameters", proof.Test)
	}
	testParam, err := exactProofTestParam(fn.Type.Params, imports, "proof test")
	if err != nil {
		return fmt.Errorf("proof %s: %w", proof.Test, err)
	}
	if fn.Type.Results != nil && len(fn.Type.Results.List) != 0 {
		return fmt.Errorf("proof %s must not return results", proof.Test)
	}
	if forbidden := forbiddenProofCall(fn.Body, imports, localImportPath); forbidden != "" {
		return fmt.Errorf("proof %s directly calls %s", proof.Test, forbidden)
	}
	if len(fn.Body.List) == 0 {
		return fmt.Errorf("proof %s has no contract runner call", proof.Test)
	}
	for _, stmt := range fn.Body.List[:len(fn.Body.List)-1] {
		declStmt, ok := stmt.(*ast.DeclStmt)
		if !ok {
			return fmt.Errorf("proof %s: only zero-value var declarations may precede the contract runner", proof.Test)
		}
		decl, ok := declStmt.Decl.(*ast.GenDecl)
		if !ok || decl.Tok != token.VAR || proofDeclarationHasValues(decl) {
			return fmt.Errorf("proof %s: only zero-value var declarations may precede the contract runner", proof.Test)
		}
	}

	exprStmt, ok := fn.Body.List[len(fn.Body.List)-1].(*ast.ExprStmt)
	if !ok {
		return fmt.Errorf("proof %s final statement must call contract runner %s", proof.Test, renderSymbolRef(proof.Runner))
	}
	runner, ok := unparen(exprStmt.X).(*ast.CallExpr)
	if !ok {
		return fmt.Errorf("proof %s final statement must call contract runner %s", proof.Test, renderSymbolRef(proof.Runner))
	}
	runnerRef, err := resolveProofCallSymbol(runner, imports, localImportPath)
	if err != nil || runnerRef != proof.Runner {
		return fmt.Errorf("proof %s final statement must call contract runner %s", proof.Test, renderSymbolRef(proof.Runner))
	}
	if len(runner.Args) != 2 {
		return fmt.Errorf("proof %s contract runner requires the test parameter and one inline factory", proof.Test)
	}
	runnerTest, ok := unparen(runner.Args[0]).(*ast.Ident)
	if !ok || runnerTest.Obj != testParam.Obj {
		return fmt.Errorf("proof %s contract runner must receive its test parameter directly", proof.Test)
	}
	factory, ok := unparen(runner.Args[1]).(*ast.FuncLit)
	if !ok {
		return fmt.Errorf("proof %s contract runner factory must be an inline function literal", proof.Test)
	}
	return validateProofFactory(factory, constructor, proof, imports, localImportPath)
}

func validateProofFactory(factory *ast.FuncLit, constructor SymbolRef, proof ProofRef, imports map[string]string, localImportPath string) error {
	factoryParam, err := exactProofTestParam(factory.Type.Params, imports, "runner factory")
	if err != nil {
		return err
	}
	if len(factory.Body.List) != 1 {
		return fmt.Errorf("runner factory must contain exactly one direct return statement")
	}
	ret, ok := factory.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) == 0 {
		return fmt.Errorf("runner factory must contain exactly one direct return statement")
	}
	constructorCall, ok := unparen(ret.Results[0]).(*ast.CallExpr)
	if !ok {
		return fmt.Errorf("factory must return constructor %s directly", renderSymbolRef(constructor))
	}
	constructorRef, err := resolveProofCallSymbol(constructorCall, imports, localImportPath)
	if err != nil || constructorRef != constructor {
		return fmt.Errorf("factory must return constructor %s directly", renderSymbolRef(constructor))
	}

	allowed := map[SymbolRef]bool{constructor: true}
	for _, call := range proof.AllowedCalls {
		allowed[call] = true
	}
	usedAllowed := make(map[SymbolRef]bool)
	constructorCalls := 0
	var callProblem string
	ast.Inspect(factory.Body, func(node ast.Node) bool {
		if callProblem != "" {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := unparen(call.Fun).(*ast.SelectorExpr); ok {
			if receiver, ok := unparen(selector.X).(*ast.Ident); ok && receiver.Obj == factoryParam.Obj {
				switch selector.Sel.Name {
				case "Name", "TempDir":
					return true
				default:
					callProblem = "runner factory test method " + selector.Sel.Name + " is not allowed"
					return false
				}
			}
		}
		ref, err := resolveProofCallSymbol(call, imports, localImportPath)
		if err != nil || !allowed[ref] {
			callProblem = fmt.Sprintf("runner factory callee %s is not allowed", renderSymbolRef(ref))
			return false
		}
		if ref == constructor {
			constructorCalls++
		} else {
			usedAllowed[ref] = true
		}
		return true
	})
	if callProblem != "" {
		return fmt.Errorf("%s", callProblem)
	}
	if constructorCalls != 1 {
		return fmt.Errorf("runner factory must call constructor %s exactly once; found %d", renderSymbolRef(constructor), constructorCalls)
	}
	for _, allowedCall := range proof.AllowedCalls {
		if !usedAllowed[allowedCall] {
			return fmt.Errorf("allowed proof call %s is not used", renderSymbolRef(allowedCall))
		}
	}
	return nil
}

func exactProofTestParam(params *ast.FieldList, imports map[string]string, owner string) (*ast.Ident, error) {
	if params == nil || len(params.List) != 1 || len(params.List[0].Names) != 1 {
		return nil, fmt.Errorf("%s must have one named testing parameter", owner)
	}
	field := params.List[0]
	ptr, ok := field.Type.(*ast.StarExpr)
	if !ok {
		return nil, fmt.Errorf("%s parameter must be exactly *testing.T", owner)
	}
	selector, ok := ptr.X.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != "T" {
		return nil, fmt.Errorf("%s parameter must be exactly *testing.T", owner)
	}
	qualifier, ok := selector.X.(*ast.Ident)
	if !ok || imports[qualifier.Name] != "testing" {
		return nil, fmt.Errorf("%s parameter must be exactly *testing.T", owner)
	}
	return field.Names[0], nil
}

func forbiddenProofCall(root ast.Node, imports map[string]string, localImportPath string) string {
	var forbidden string
	ast.Inspect(root, func(node ast.Node) bool {
		if forbidden != "" {
			return false
		}
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if selector, ok := unparen(call.Fun).(*ast.SelectorExpr); ok {
			switch selector.Sel.Name {
			case "Skip", "Skipf", "SkipNow":
				receiver := "<expression>"
				if ident, ok := unparen(selector.X).(*ast.Ident); ok {
					receiver = ident.Name
				}
				forbidden = receiver + "." + selector.Sel.Name
				return false
			}
		}
		if ref, err := resolveProofCallSymbol(call, imports, localImportPath); err == nil && ref == (SymbolRef{ImportPath: "testing", Name: "Short"}) {
			forbidden = "testing.Short"
			return false
		}
		return true
	})
	return forbidden
}

func resolveProofCallSymbol(call *ast.CallExpr, imports map[string]string, localImportPath string) (SymbolRef, error) {
	switch fun := unparen(call.Fun).(type) {
	case *ast.Ident:
		if fun.Obj != nil && fun.Obj.Kind != ast.Fun {
			return SymbolRef{}, fmt.Errorf("%s resolves to a local %s, not a declared function", fun.Name, fun.Obj.Kind)
		}
		return SymbolRef{ImportPath: localImportPath, Name: fun.Name}, nil
	case *ast.SelectorExpr:
		qualifier, ok := unparen(fun.X).(*ast.Ident)
		if !ok || (qualifier.Obj != nil && qualifier.Obj.Kind != ast.Pkg) {
			return SymbolRef{}, fmt.Errorf("selector receiver is not an imported package")
		}
		importPath := imports[qualifier.Name]
		if importPath == "" {
			return SymbolRef{}, fmt.Errorf("selector receiver %s is not an imported package", qualifier.Name)
		}
		return SymbolRef{ImportPath: importPath, Name: fun.Sel.Name}, nil
	default:
		return SymbolRef{}, fmt.Errorf("callee must be a direct function call, got %T", call.Fun)
	}
}

func proofDeclarationHasValues(decl *ast.GenDecl) bool {
	for _, spec := range decl.Specs {
		values, ok := spec.(*ast.ValueSpec)
		if !ok || len(values.Values) != 0 {
			return true
		}
	}
	return false
}

func isRunnableProofTestName(name string) bool {
	if !strings.HasPrefix(name, "Test") {
		return false
	}
	if len(name) == len("Test") {
		return true
	}
	r, _ := utf8.DecodeRuneInString(name[len("Test"):])
	return !unicode.IsLower(r)
}
