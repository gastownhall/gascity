// Package formulatest contains helpers for tests that exercise formula behavior
// from outside the formula package.
package formulatest

import (
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

// EnableV2ForTest enables graph.v2 formula compilation for the duration of the
// test, restoring the previous value on cleanup. Callers must not use it in
// tests that run in parallel with other formula-v2 flag mutations because the
// flag is process-global.
func EnableV2ForTest(tb testing.TB) {
	tb.Helper()
	prev := formula.IsFormulaV2Enabled()
	formula.SetFormulaV2Enabled(true)
	tb.Cleanup(func() { formula.SetFormulaV2Enabled(prev) })
}
