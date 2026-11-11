// Package errorlint exposes go-errorlint as a nogo analyzer configured with
// golangci-lint's defaults (errorf, errorf-multi, asserts and comparison all
// enabled; the upstream analyzer leaves errorf off).
package errorlint

import (
	"fmt"

	"codeberg.org/polyfloyd/go-errorlint/errorlint"
	"golang.org/x/tools/go/analysis"
)

// Analyzer reports error comparisons, type assertions and fmt.Errorf calls
// that break Go 1.13 error wrapping.
var Analyzer = newAnalyzer()

func newAnalyzer() *analysis.Analyzer {
	a := errorlint.NewAnalyzer()
	for name, value := range map[string]string{
		"errorf":       "true",
		"errorf-multi": "true",
		"asserts":      "true",
		"comparison":   "true",
	} {
		if err := a.Flags.Set(name, value); err != nil {
			panic(fmt.Sprintf("errorlint: setting -%s=%s: %v", name, value, err))
		}
	}
	return a
}
