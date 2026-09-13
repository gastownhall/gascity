// Package formulatest contains helpers for tests that exercise formula behavior
// from outside the formula package.
package formulatest

import (
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/formula"
)

var v2Mu sync.Mutex

// LockV2ForTest acquires exclusive access to the process-global formula_v2
// flag for the duration of the test and returns a setter for in-test updates.
// It is non-reentrant: call it at most once per test goroutine.
func LockV2ForTest(tb testing.TB) func(enabled bool) {
	tb.Helper()
	v2Mu.Lock()
	prev := formula.IsFormulaV2Enabled()
	tb.Cleanup(func() {
		defer v2Mu.Unlock()
		formula.SetFormulaV2Enabled(prev)
	})
	return func(enabled bool) {
		formula.SetFormulaV2Enabled(enabled)
	}
}

// HoldV2ForTest serializes a test against other formula_v2 mutators while
// preserving the current flag value.
func HoldV2ForTest(tb testing.TB) {
	tb.Helper()
	_ = LockV2ForTest(tb)
}

// SetV2ForTest sets graph.v2 formula compilation for the duration of the test,
// restoring the previous value during cleanup.
func SetV2ForTest(tb testing.TB, enabled bool) {
	tb.Helper()
	LockV2ForTest(tb)(enabled)
}

// EnableV2ForTest enables graph.v2 formula compilation for the duration of the
// test, restoring the previous value during cleanup.
func EnableV2ForTest(tb testing.TB) {
	tb.Helper()
	SetV2ForTest(tb, true)
}
