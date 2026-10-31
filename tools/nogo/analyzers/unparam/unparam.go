// Package unparam exposes mvdan.cc/unparam as a nogo analyzer, mirroring
// golangci-lint's wrapper (exported functions are not checked).
package unparam

import (
	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/passes/buildssa"
	"golang.org/x/tools/go/packages"
	"mvdan.cc/unparam/check"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/testunit"
)

// Analyzer reports function parameters and results that are always unused or
// always receive the same value.
var Analyzer = &analysis.Analyzer{
	Name:     "unparam",
	Doc:      "reports unused function parameters and results",
	Requires: []*analysis.Analyzer{buildssa.Analyzer},
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	// Library-only units would count test-only uses as unused; see testunit.
	if !testunit.HasTestFiles(pass) {
		return nil, nil
	}
	ssa := pass.ResultOf[buildssa.Analyzer].(*buildssa.SSA)
	pkg := &packages.Package{
		Fset:      pass.Fset,
		Syntax:    pass.Files,
		Types:     pass.Pkg,
		TypesInfo: pass.TypesInfo,
	}
	c := &check.Checker{}
	c.CheckExportedFuncs(false)
	c.Packages([]*packages.Package{pkg})
	c.ProgramSSA(ssa.Pkg.Prog)
	issues, err := c.Check()
	if err != nil {
		return nil, err
	}
	for _, issue := range issues {
		pass.Report(analysis.Diagnostic{Pos: issue.Pos(), Message: issue.Message()})
	}
	return nil, nil
}
