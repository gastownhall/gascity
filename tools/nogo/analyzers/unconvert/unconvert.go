// Package unconvert exposes golangci-lint's fork of mdempsky/unconvert as a
// nogo analyzer. Upstream unconvert is a standalone command with no
// analysis.Analyzer; the golangci fork exposes Run(pass) instead.
package unconvert

import (
	"github.com/golangci/unconvert"
	"golang.org/x/tools/go/analysis"

	"github.com/gastownhall/gascity/tools/nogo/analyzers/internal/position"
)

// Analyzer reports type conversions whose operand already has the target type.
var Analyzer = &analysis.Analyzer{
	Name: "unconvert",
	Doc:  "reports unnecessary type conversions",
	Run:  run,
}

func run(pass *analysis.Pass) (any, error) {
	idx := position.NewIndex(pass)
	for _, p := range unconvert.Run(pass) {
		pos, ok := idx.Pos(p)
		if !ok {
			continue
		}
		pass.Report(analysis.Diagnostic{Pos: pos, Message: "unnecessary conversion"})
	}
	return nil, nil
}
