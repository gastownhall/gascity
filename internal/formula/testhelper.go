package formula

import "testing"

// EnableV2ForTest enables graph.v2 formula compilation for the duration
// of the test, restoring the previous value on cleanup. Safe for use in
// tests and benchmarks; callers must not t.Parallel because the flag is
// process-global.
func EnableV2ForTest(tb testing.TB) {
	tb.Helper()
	prev := IsFormulaV2Enabled()
	SetFormulaV2Enabled(true)
	tb.Cleanup(func() { SetFormulaV2Enabled(prev) })
}
