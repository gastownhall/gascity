package exec

import (
	"os"
	"testing"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// hostTmuxIsolationRoot is the private TMUX_TMPDIR TestMain points this test
// binary at; empty until TestMain has isolated the process.
var hostTmuxIsolationRoot string

// TestMain confines every tmux client this package's tests start to a private
// socket root. The exec provider drives its in-box session ("main") by sending
// tmux commands over the pack's exec op, and fixtures here (the runtimecontract
// golden reference among them) run that op on the test host, so without this a
// developer's own tmux server — a session or window named "main" — would
// receive the tests' send-keys. TestExecOpTmuxClientsCannotReachTheHostServer
// guards it.
func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "gc-exec-tmux-")
	if err != nil {
		panic("exec tests: creating private tmux socket root: " + err.Error())
	}
	if err := tmuxtest.ConfigureProcessEnv(root); err != nil {
		_ = os.RemoveAll(root)
		panic("exec tests: isolating tmux from the host server: " + err.Error())
	}
	hostTmuxIsolationRoot = root
	code := m.Run()
	_ = os.RemoveAll(root)
	os.Exit(code)
}
