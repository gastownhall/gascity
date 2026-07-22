package tmux

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/test/tmuxtest"
)

// TestMain neutralizes ambient tmux/session state for the whole package.
// GC_AGENT_SLICE activates real pane-command wrapping inside any test process
// on hosts that export it, which would break exact-argv and pane-command
// assertions across both the unit and integration tiers. Tests that exercise
// wrapping opt back in per-test with t.Setenv. This file is untagged so the
// neutralization applies to every build of the package.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(AgentSliceEnv)

	// NewSocketParentDir sweeps orphaned siblings left by a prior SIGKILL'd
	// run before creating this run's own dir. tmuxSocketAliveSentinel must
	// stay referenced for the process lifetime: the runtime finalizes
	// unreachable os.Files, which would close the descriptor and release
	// the lock, letting a concurrent sibling's sweep reclaim this still-
	// active directory (ga-djbcqt).
	tmuxSocketParent, sentinel, err := tmuxtest.NewSocketParentDir("/tmp", io.Discard)
	if err != nil {
		panic("tmux tests: creating socket parent: " + err.Error())
	}
	tmuxSocketAliveSentinel = sentinel
	tmuxSocketRoot := filepath.Join(tmuxSocketParent, "tmux")
	if err := tmuxtest.ConfigureProcessEnv(tmuxSocketRoot); err != nil {
		_ = os.RemoveAll(tmuxSocketParent)
		panic("tmux tests: configuring tmux test env: " + err.Error())
	}

	if _, err := exec.LookPath("tmux"); err == nil {
		tmuxtest.KillAllTestSessions(mainTB{})
	}
	code := m.Run()
	if _, err := exec.LookPath("tmux"); err == nil {
		tmuxtest.KillAllTestSessions(mainTB{})
	}
	_ = os.RemoveAll(tmuxSocketParent)
	os.Exit(code)
}

// tmuxSocketAliveSentinel pins the alive-sentinel flock on this process's
// tmux socket parent dir for the binary's lifetime; see TestMain.
var tmuxSocketAliveSentinel *os.File

type mainTB struct{ testing.TB }

func (mainTB) Helper()             {}
func (mainTB) Logf(string, ...any) {}
