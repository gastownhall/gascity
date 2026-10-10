package workertest

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/session"
)

// TestMain fails the run on a start or stop refused for want of a city that
// no test expected (session.FailOnNoCityRefusals).
func TestMain(m *testing.M) { os.Exit(session.FailOnNoCityRefusals(m.Run(), os.Stderr)) }
