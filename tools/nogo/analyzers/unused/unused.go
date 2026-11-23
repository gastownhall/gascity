// Package unused exposes honnef.co/go/tools/unused as a nogo analyzer. The
// upstream analyzer only returns its result (staticcheck's driver turns it
// into diagnostics); this wrapper reports the unused objects the way
// golangci-lint's unused linter does.
package unused

import (
	"fmt"

	"golang.org/x/tools/go/analysis"
	"honnef.co/go/tools/unused"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/position"
	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/testunit"
)

// Analyzer reports unused constants, variables, functions, fields and types.
var Analyzer = &analysis.Analyzer{
	Name:     "unused",
	Doc:      "reports unused constants, variables, functions and types",
	Requires: unused.Analyzer.Analyzer.Requires,
	Run:      run,
}

func run(pass *analysis.Pass) (any, error) {
	// Library-only units would count test-only uses as unused; see testunit.
	if !testunit.HasTestFiles(pass) {
		return nil, nil
	}
	// unused.Analyzer runs with unused.DefaultOptions, which equal
	// golangci-lint's unused settings. (Building the graph through
	// SerializedGraph.Merge, as golangci-lint does, traces every node to
	// stderr.)
	out, err := unused.Analyzer.Analyzer.Run(pass)
	if err != nil {
		return nil, err
	}
	res := out.(unused.Result)

	used := make(map[string]bool, len(res.Used))
	for _, obj := range res.Used {
		used[objectKey(obj)] = true
	}
	idx := position.NewIndex(pass)
	for _, obj := range res.Unused {
		if obj.Kind == "type param" || used[objectKey(obj)] {
			continue
		}
		pos, ok := idx.Pos(obj.Position)
		if !ok {
			continue
		}
		pass.Report(analysis.Diagnostic{
			Pos:     pos,
			Message: fmt.Sprintf("%s %s is unused", obj.Kind, obj.Name),
		})
	}
	return nil, nil
}

func objectKey(obj unused.Object) string {
	return fmt.Sprintf("%s %d %s", obj.Position.Filename, obj.Position.Line, obj.Name)
}
