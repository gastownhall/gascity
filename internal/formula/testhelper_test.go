package formula

import "testing"

func enableV2ForTest(tb testing.TB) {
	tb.Helper()
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	tb.Cleanup(func() { SetFormulaV2Enabled(prev) })
}
