package tmux

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"github.com/gastownhall/gascity/internal/runtime/programstatus"
	"github.com/gastownhall/gascity/internal/shellquote"
	"github.com/gastownhall/gascity/internal/userscope"
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
// never by the pane command itself. gc-osc7501 is the program-status relay
// (program_capture.go): it holds the pane's terminal and runs the agent on a
// pty of its own.
var wrapperCommands = []string{"systemd-run", programstatus.CompanionName}

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
// systemd user scope, delegating the availability probe and its graceful
// fallback to [userscope.Wrapper]. The probe runs at most once per Tmux
// instance; on failure it warns once and all subsequent commands run
// unwrapped.
//
// The probe runs in the gc process's environment, while pane commands
// execute with the tmux server's environment. gc normally spawns the tmux
// server itself, so the server inherits gc's environment and the probe is
// representative — but a pre-existing server whose global environment lacks
// a reachable user bus (XDG_RUNTIME_DIR, DBUS_SESSION_BUS_ADDRESS) can still
// fail wrapped spawns after a successful probe here.
type agentSliceWrapper struct {
	probe func(slice string) error // test seam; nil means userscope.Probe
	warn  io.Writer                // test seam; nil means the standard logger

	init  sync.Once
	scope *userscope.Wrapper
}

// wrap returns command wrapped for the given slice, or command unchanged
// when slice is empty, command is empty, or transient user scopes are
// unavailable on this host.
func (w *agentSliceWrapper) wrap(slice, command string) string {
	if command == "" || !w.active(slice) {
		return command
	}
	return shellquote.Join(userscope.Argv(slice, []string{"sh", "-c", command}))
}

// active reports whether pane commands are wrapped for the given slice: it
// is set and transient user scopes are available on this host.
func (w *agentSliceWrapper) active(slice string) bool {
	if slice == "" {
		return false
	}
	w.init.Do(func() {
		w.scope = &userscope.Wrapper{Probe: w.probe, Unavailable: w.unavailable}
	})
	return w.scope.Active(slice)
}

// unavailable reports, once, that GC_AGENT_SLICE is set but transient user
// scopes cannot be created.
func (w *agentSliceWrapper) unavailable(slice string, err error) {
	msg := fmt.Sprintf("%s=%q set but transient user scopes are unavailable; pane commands run unwrapped: %v",
		AgentSliceEnv, slice, err)
	if w.warn != nil {
		_, _ = fmt.Fprintln(w.warn, "gc: "+msg)
		return
	}
	log.Printf("tmux agent slice: %s", msg)
}

// wrapPaneCommand applies the GC_AGENT_SLICE systemd user-scope wrapper to a
// pane's initial command. See [AgentSliceEnv]. The environment variable is
// read per call but the availability probe result is cached, so the first
// non-empty slice value decides whether wrapping is active for this Tmux.
func (t *Tmux) wrapPaneCommand(command string) string {
	return t.agentSlice.wrap(os.Getenv(AgentSliceEnv), command)
}
