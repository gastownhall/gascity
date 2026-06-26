package tmux

import (
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

	tmuxSocketParent, err := os.MkdirTemp("/tmp", "gct-")
	if err != nil {
		panic("tmux tests: creating socket parent: " + err.Error())
	}
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

type mainTB struct{ testing.TB }

func (mainTB) Helper()             {}
func (mainTB) Logf(string, ...any) {}
