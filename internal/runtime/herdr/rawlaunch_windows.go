//go:build windows

package herdr

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/runtime/proctable"
)

// launchRaw types the raw command into the pane's PowerShell as one statement
// that runs it in a child interpreter and then exits the shell with its status
// ([runtime.ShellCommandPrompt]), so the pane (and tab) close when the command
// ends, preserving the tmux contract that a session ends with its command.
// Windows has no exec: the shell stays the pane's root, and the command runs as
// its child.
//
// The statement is PowerShell, so a pane whose shell is anything else is
// refused rather than typed into. Typed into another shell it would fail, and a
// shell that echoes a failed line puts text on screen that can read as the
// command's own output. A command the platform refuses
// ([runtime.ShellCommandUnavailable]) is not typed at all.
func (p *Provider) launchRaw(ctx context.Context, paneID, raw string) error {
	if reason := runtime.ShellCommandUnavailable(raw); reason != "" {
		return fmt.Errorf("raw command refused: %s", reason)
	}
	shell, err := p.awaitPaneShell(ctx, paneID)
	if err != nil {
		return err
	}
	if key := proctable.ProcessNameKey(shell); key != "powershell" && key != "pwsh" {
		if shell == "" {
			shell = "an unidentified shell"
		}
		return fmt.Errorf("pane %s runs %s; a raw command is launched on Windows only through a PowerShell pane", paneID, shell)
	}
	launch, err := runtime.ShellCommandPrompt(raw)
	if err != nil {
		return err
	}
	if err := p.c.paneRunCommand(ctx, paneID, launch.Statement); err != nil {
		return err
	}
	p.waitPaneLaunched(ctx, paneID, launch)
	return nil
}

// awaitPaneShell returns the image name of the pane's shell, or "" if herdr
// has not listed it within paneShellReadyWait. A fresh pane's process-info can
// omit the shell while it is still starting (measured, herdr 0.9.1), so one
// read would refuse a pane that is about to be a PowerShell prompt.
//
// A read that fails is retried within the same bound, as the Unix launch,
// which types without reading first, never fails on one: only a pane that is
// gone, or a failure that outlasts the bound, is returned.
func (p *Provider) awaitPaneShell(ctx context.Context, paneID string) (string, error) {
	deadline := time.Now().Add(paneShellReadyWait)
	for {
		shellPID, fg, err := p.c.processInfo(ctx, paneID)
		switch {
		case err != nil && (paneGone(err) || time.Now().After(deadline)):
			return "", err
		case err == nil:
			if name := paneShellName(shellPID, fg); name != "" || time.Now().After(deadline) {
				return name, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// paneGone reports whether err is herdr saying the pane no longer exists.
func paneGone(err error) bool {
	return strings.Contains(err.Error(), "not_found") || strings.Contains(err.Error(), "not found")
}

// paneShellName is the image name herdr reports for the pane's shell, or ""
// when process-info does not list it.
func paneShellName(shellPID int, fg []proc) string {
	if shellPID == 0 {
		return ""
	}
	for _, pr := range fg {
		if pr.PID == shellPID {
			return pr.Name
		}
	}
	return ""
}

// waitPaneLaunched polls the pane until the typed statement has started its
// child interpreter, the pane is gone (the command already ran and exited), or
// the bound elapses. Best-effort by design, as on Unix.
//
// The child is recognized by its command line, which carries the launch's
// encoded command. An image name and a creation time are not enough: the
// shell can start an interpreter of the same image for itself, a profile's
// Start-Job for one, and that child satisfied the wait before the launch ran
// (measured).
func (p *Provider) waitPaneLaunched(ctx context.Context, paneID string, launch runtime.PromptLaunch) {
	commandLine := func(child proctable.ChildProcess) (string, error) {
		return paneChildCommandLine(p.c.bin, child)
	}
	rejected := make(map[launchCandidate]bool)
	deadline := time.Now().Add(rawLaunchWait)
	for time.Now().Before(deadline) {
		shellPID, _, err := p.c.processInfo(ctx, paneID)
		switch {
		case err != nil && paneGone(err):
			return // pane already gone: the command ran and exited
		case err == nil && shellPID != 0:
			if children, cerr := paneShellChildren(p.c.bin, shellPID); cerr == nil && launchLanded(children, launch, commandLine, rejected) {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// launchCandidate identifies one process across polls.
type launchCandidate struct {
	pid     int
	created int64
}

// launchLanded reports whether children holds the interpreter launch started:
// a child of the launch's interpreter image whose command line carries its
// encoded command. rejected remembers children whose command lines were read
// and belong to other processes, so each is read once; a command line that
// could not be read is tried again on the next poll.
func launchLanded(children []paneChild, launch runtime.PromptLaunch, commandLine func(proctable.ChildProcess) (string, error), rejected map[launchCandidate]bool) bool {
	want := proctable.ProcessNameKey(filepath.Base(launch.Interpreter))
	for _, child := range children {
		key := launchCandidate{pid: child.PID, created: child.Created.UnixNano()}
		if proctable.ProcessNameKey(child.Name) != want || rejected[key] {
			continue
		}
		line, err := commandLine(child.ChildProcess)
		if err != nil {
			continue
		}
		if strings.Contains(line, launch.EncodedCommand) {
			return true
		}
		rejected[key] = true
	}
	return false
}
