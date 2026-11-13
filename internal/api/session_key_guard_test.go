package api

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/session"
)

// Any write of a session-row key the session field registry does not have
// fails this package's tests (session.GuardSessionKeys), as does a hand-built
// write of a disposition class key (session.GuardDispositionWrites).
func init() {
	session.GuardSessionKeys(func(msg string) { panic(msg) })
	session.GuardDispositionWrites(func(msg string) { panic(msg) })
}

// TestMain fails the run on a violation a recover() swallowed, and on a start
// or stop refused for want of a city that no test expected. Under
// GC_TEST_SPLIT_STRICT=1 it also runs the split-strict ratchet: a violation
// site the checked-in baseline does not list fails the run
// (split_strict_gate_test.go). The guards answer apart from the tests there
// (splittest.GateChecksBase), so a known test failure cannot hide theirs.
func TestMain(m *testing.M) {
	splittest.GateStart()
	tests := 0
	checks := session.FailOnNoCityRefusals(func() int {
		tests = m.Run()
		return session.FailOnKeyViolations(splittest.GateChecksBase(tests), os.Stderr)
	}, os.Stderr)
	os.Exit(splittest.GateMain(tests, checks, splitStrictFiles, os.Stderr))
}
