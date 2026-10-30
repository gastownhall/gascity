package tmux

import "github.com/gastownhall/gascity/internal/runtime"

var _ runtime.SteerCapabilityProvider = (*Provider)(nil)

// SteerMode reports that tmux steers by keystroke: NudgeNow, and a fenced
// nudge with Immediate set, type into the agent pane without waiting for
// idle. It is a static trait and reads no tmux state.
func (p *Provider) SteerMode(string) runtime.SteerMode {
	return runtime.SteerKeystroke
}
