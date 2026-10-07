package containerhost

import (
	"io"
	"os"
	"testing"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// stateParent holds every emulated host this test binary creates. It lives
// directly under /tmp (via the shared tmux socket-parent helper, which also
// sweeps a SIGKILLed run's leftovers): container tmux sockets sit below it
// and must stay under the 108-byte unix socket path limit.
var stateParent string

// TestMain serves the emulated host's tools (docker, pgrep) when this binary
// is started under their names, and otherwise runs the tests.
func TestMain(m *testing.M) {
	RunAsTool()

	dir, sentinel, err := tmuxtest.NewSocketParentDir("/tmp", io.Discard)
	if err != nil {
		panic("containerhost tests: creating state parent: " + err.Error())
	}
	stateParent = dir
	code := m.Run()
	_ = sentinel.Close()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}
