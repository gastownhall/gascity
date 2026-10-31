//go:build windows

package herdr

import (
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// paneChild is a running child of a pane's shell.
type paneChild struct {
	proctable.ChildProcess
	// Attached reports whether the child shares the shell's console, and so
	// runs in the pane, as a typed command does. A child started detached, in a
	// window of its own or with none (Start-Job, Start-Process without
	// -NoNewWindow) runs beside the pane rather than in it.
	Attached bool
}

// paneShellChildren lists the running children of a pane's shell. bin is the
// herdr the pane belongs to. A variable so tests can answer for panes that exist
// only in a fake herdr.
var paneShellChildren = func(_ string, shellPID int) ([]paneChild, error) {
	return consoleChildren(shellPID)
}

// paneChildCommandLine is the command line of child, a child of a pane's
// shell. A variable for the same reason as paneShellChildren.
var paneChildCommandLine = func(_ string, child proctable.ChildProcess) (string, error) {
	return proctable.ProcessCommandLine(child.PID, child.Created)
}

// consoleChildren lists shellPID's children, each marked by whether it shares
// the shell's console (see markAttached).
func consoleChildren(shellPID int) ([]paneChild, error) {
	children, err := proctable.ChildProcesses(shellPID)
	if err != nil || len(children) == 0 {
		return nil, err
	}
	return markAttached(shellPID, children, proctable.ConsoleHost), nil
}

// markAttached marks each of shellPID's children by whether it shares the
// shell's console, as consoleHost reports the console host of a process
// created at a given time, or at any time when that is zero. Where the shell's
// console or a child's cannot be read, the child counts as attached, because
// reading an unknown child as idle would reap a pane still running it. A shell
// with no console is such a case: nothing it starts can share one with it. A
// child that exited after the snapshot is dropped.
func markAttached(shellPID int, children []proctable.ChildProcess, consoleHost func(int, time.Time) (int, error)) []paneChild {
	shellHost, shellErr := consoleHost(shellPID, time.Time{})
	listed := make([]paneChild, 0, len(children))
	for _, child := range children {
		attached := true
		if shellErr == nil && shellHost != 0 {
			host, err := consoleHost(child.PID, child.Created)
			switch {
			case errors.Is(err, proctable.ErrProcessGone):
				continue
			case err == nil:
				attached = host == shellHost
			}
		}
		listed = append(listed, paneChild{ChildProcess: child, Attached: attached})
	}
	return listed
}

// shellProbe decides busyness from the pane shell's children.
//
// On Windows the shell is the pane's root for its whole life, since nothing
// exec's over it, and `pane process-info` lists only the shell even while a
// command runs in the pane (measured, herdr 0.9.1). The foreground list cannot
// tell a bare prompt from a running agent, and read the Unix way it made every
// pane busy forever.
//
// A pane is busy while a child of its shell shares the shell's console: the
// command typed into it, or the agent herdr started there. The console is the
// nearest Windows has to a terminal's foreground process group. A child
// counted merely for existing kept a pane busy forever once its shell had
// started anything in the background (a profile's Start-Job, a Start-Process
// helper), so an exited agent was never reaped (measured: live herdr pane).
//
// Two limits remain. Work the shell does itself, a slow profile or a cmdlet
// typed at the prompt, starts no child and reads idle; herdr's own
// available-shell check still refuses an agent start then. And a helper
// started with -NoNewWindow shares the console, so it reads busy, where a Unix
// shell's background job would not.
func (p *Provider) shellProbe(shellPID int, _ []proc) (paneProbe, error) {
	if shellPID == 0 {
		return paneProbe{}, nil
	}
	children, err := paneShellChildren(p.c.bin, shellPID)
	if err != nil {
		return paneProbe{}, fmt.Errorf("herdr: listing the children of pane shell %d: %w", shellPID, err)
	}
	for _, child := range children {
		if child.Attached {
			return paneProbe{Exists: true, Busy: true}, nil
		}
	}
	return paneProbe{Exists: true}, nil
}
