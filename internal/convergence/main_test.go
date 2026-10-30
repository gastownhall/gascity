package convergence

import (
	"os"
	"testing"
)

// TestMain gives the package's tests a private GC_HOME. conditionPATH writes
// its gate-tool link directory under GC_HOME, and a test must never write
// into the operator's real ~/.gc. Tests that need a specific GC_HOME still
// set their own.
func TestMain(m *testing.M) {
	home, err := os.MkdirTemp("", "convergence-gc-home-")
	if err != nil {
		panic(err)
	}
	if err := os.Setenv("GC_HOME", home); err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(home)
	os.Exit(code)
}
