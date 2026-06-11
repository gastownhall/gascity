package tmux

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

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

// agentSliceProbeTimeout bounds the one-time systemd-run availability probe.
// Test-overridable.
var agentSliceProbeTimeout = 5 * time.Second

// probeAgentSliceSupport verifies that systemd-run exists and the systemd
// user manager responds by running a no-op command in a transient scope on
// the target slice. This exercises the exact mechanism used for pane
// commands, so success here means pane wrapping will work.
func probeAgentSliceSupport(slice string) error {
	if _, err := exec.LookPath("systemd-run"); err != nil {
		return fmt.Errorf("systemd-run not found: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), agentSliceProbeTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "systemd-run",
		"--user", "--scope", "--slice="+slice, "--collect", "--quiet", "--", "true")
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if msg != "" {
			return fmt.Errorf("systemd user manager probe failed: %w: %s", err, msg)
		}
		return fmt.Errorf("systemd user manager probe failed: %w", err)
	}
	return nil
}

// agentSliceWrapper decides whether pane commands are wrapped in a transient
// systemd user scope. The availability probe runs at most once per Tmux
// instance; on failure it warns once and all subsequent commands run
// unwrapped (graceful fallback).
type agentSliceWrapper struct {
	probe func(slice string) error // test seam; nil means probeAgentSliceSupport
	warn  io.Writer                // test seam; nil means os.Stderr
	once  sync.Once
	ok    bool
}

// wrap returns command wrapped for the given slice, or command unchanged
// when slice is empty, command is empty, or transient user scopes are
// unavailable on this host.
func (w *agentSliceWrapper) wrap(slice, command string) string {
	if slice == "" || command == "" {
		return command
	}
	w.once.Do(func() {
		probe := w.probe
		if probe == nil {
			probe = probeAgentSliceSupport
		}
		if err := probe(slice); err != nil {
			out := w.warn
			if out == nil {
				out = os.Stderr
			}
			_, _ = fmt.Fprintf(out, "gc: %s=%q set but transient user scopes are unavailable; pane commands run unwrapped: %v\n",
				AgentSliceEnv, slice, err)
			return
		}
		w.ok = true
	})
	if !w.ok {
		return command
	}
	return shellquote.Join([]string{
		"systemd-run", "--user", "--scope", "--slice=" + slice,
		"--collect", "--quiet", "--", "sh", "-c", command,
	})
}

// wrapPaneCommand applies the GC_AGENT_SLICE systemd user-scope wrapper to a
// pane's initial command. See [AgentSliceEnv]. The environment variable is
// read per call but the availability probe result is cached, so the first
// non-empty slice value decides whether wrapping is active for this Tmux.
func (t *Tmux) wrapPaneCommand(command string) string {
	return t.agentSlice.wrap(os.Getenv(AgentSliceEnv), command)
}
