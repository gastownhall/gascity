package worker

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// Any write of a session-row key the session field registry does not have
// fails this package's tests (session.GuardSessionKeys).
func init() { session.GuardSessionKeys(func(msg string) { panic(msg) }) }

// TestMain fails the run on a violation a recover() swallowed, and on a start
// or stop refused for want of a city that no test expected.
func TestMain(m *testing.M) {
	os.Exit(session.FailOnNoCityRefusals(session.FailOnKeyViolations(m.Run(), os.Stderr), os.Stderr))
}
