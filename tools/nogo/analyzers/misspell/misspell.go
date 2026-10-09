// Package misspell exposes github.com/golangci/misspell as a nogo analyzer.
// misspell is a library with no analysis.Analyzer; this mirrors
// golangci-lint's wrapper with the US locale and the default (non-restricted)
// mode, which checks the whole file text, not only comments.
package misspell

import (
	"fmt"
	"go/token"
	"os"
	"strings"

	"github.com/golangci/misspell"
	"golang.org/x/tools/go/analysis"
)

// Analyzer reports commonly misspelled English words, US spelling.
var Analyzer = &analysis.Analyzer{
	Name: "misspell",
	Doc:  "reports commonly misspelled English words (US locale)",
	Run:  run,
}

var replacer = newReplacer()

func newReplacer() *misspell.Replacer {
	r := &misspell.Replacer{Replacements: misspell.DictMain}
	r.AddRuleList(misspell.DictAmerican)
	r.Compile()
	return r
}

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		tf := pass.Fset.File(file.Pos())
		// cgo rewrites sources into generated files whose positions map back
		// to the originals; only real .go sources are checked.
		if !strings.HasSuffix(tf.Name(), ".go") {
			continue
		}
		content, err := os.ReadFile(tf.Name())
		if err != nil {
			return nil, fmt.Errorf("misspell: reading %s: %w", tf.Name(), err)
		}
		_, diffs := replacer.Replace(string(content))
		for _, diff := range diffs {
			if diff.Line < 1 || diff.Line > tf.LineCount() {
				continue
			}
			start := tf.LineStart(diff.Line) + token.Pos(diff.Column)
			end := start + token.Pos(len(diff.Original))
			pass.Report(analysis.Diagnostic{
				Pos:     start,
				End:     end,
				Message: fmt.Sprintf("`%s` is a misspelling of `%s`", diff.Original, diff.Corrected),
				SuggestedFixes: []analysis.SuggestedFix{{
					Message:   "fix spelling",
					TextEdits: []analysis.TextEdit{{Pos: start, End: end, NewText: []byte(diff.Corrected)}},
				}},
			})
		}
	}
	return nil, nil
}
