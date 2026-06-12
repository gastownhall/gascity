package tmux

import (
	"os"
	"testing"
)

// TestMain neutralizes ambient GC_AGENT_SLICE for the whole package. The
// variable activates real pane-command wrapping inside any test process on
// hosts that export it, which would break exact-argv and pane-command
// assertions across both the unit and integration tiers. Tests that exercise
// wrapping opt back in per-test with t.Setenv. This file is untagged so the
// neutralization applies to every build of the package.
func TestMain(m *testing.M) {
	_ = os.Unsetenv(AgentSliceEnv)
	os.Exit(m.Run())
}
