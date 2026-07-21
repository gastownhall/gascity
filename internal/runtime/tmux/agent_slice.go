package tmux

import (
	"os"

	"github.com/gastownhall/gascity/internal/runtime/systemdscope"
	"github.com/gastownhall/gascity/internal/shellquote"
)

// AgentSliceEnv names the environment variable that, when set to a systemd
// user slice (e.g. "gascity-agents.slice"), makes the tmux provider wrap
// every pane's initial command in a transient systemd user scope:
//
//	systemd-run --user --scope --slice=<slice> --collect --quiet -- sh -c '<command>'
//
// Rationale: systemd-enabled tmux builds (stock Ubuntu) move every pane into
// a transient tmux-spawn-*.scope under the default user slice, so agent
// processes escape whatever slice the tmux server itself runs in. Wrapping
// the pane command re-parents the agent's process tree into a dedicated user
// slice where resource weights can be applied. Default-off: when unset, pane
// commands run unwrapped exactly as before.
const AgentSliceEnv = "GC_AGENT_SLICE"

// wrapperCommands lists pane-root wrapper binaries produced by pane-command
// wrapping. A wrapped pane reports the wrapper as pane_current_command for
// the pane's whole lifetime, so command-wait and detection paths must treat
// these like shells: the agent is identified through descendant inspection,
// never by the pane command itself.
var wrapperCommands = []string{"systemd-run"}

// isWrapperCommand reports whether cmd is a known pane-root wrapper binary
// (see wrapperCommands).
func isWrapperCommand(cmd string) bool {
	for _, w := range wrapperCommands {
		if cmd == w {
			return true
		}
	}
	return false
}

// agentSliceWrapper decides whether pane commands are wrapped in a transient
// systemd user scope, deferring the systemd-run mechanics and the once-only
// availability probe to systemdscope. Panes need the wrapper as a shell string
// rather than argv, so the scope is built here from the shared constructor
// instead of through systemdscope.Wrapper.Wrap.
//
// The probe caveat it inherits: the probe runs in the gc process's
// environment, while pane commands execute with the tmux server's environment.
// gc normally spawns the tmux server itself, so the server inherits gc's
// environment and the probe is representative — but a pre-existing server
// whose global environment lacks a reachable user bus (XDG_RUNTIME_DIR,
// DBUS_SESSION_BUS_ADDRESS) can still fail wrapped spawns after a successful
// probe here.
type agentSliceWrapper struct {
	systemdscope.Wrapper
}

// wrap returns command wrapped for the given slice, or command unchanged
// when slice is empty, command is empty, or transient user scopes are
// unavailable on this host.
func (w *agentSliceWrapper) wrap(slice, command string) string {
	if command == "" || !w.Available(slice) {
		return command
	}
	return shellquote.Join(systemdscope.Argv(slice, []string{"sh", "-c", command}))
}

// wrapPaneCommand applies the GC_AGENT_SLICE systemd user-scope wrapper to a
// pane's initial command. See [AgentSliceEnv]. The environment variable is
// read per call but the availability probe result is cached, so the first
// non-empty slice value decides whether wrapping is active for this Tmux.
func (t *Tmux) wrapPaneCommand(command string) string {
	return t.agentSlice.wrap(os.Getenv(AgentSliceEnv), command)
}
