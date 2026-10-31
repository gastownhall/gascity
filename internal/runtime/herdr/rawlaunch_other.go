//go:build !windows

package herdr

import (
	"context"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// launchRaw execs the raw command through the pane's shell, so the pane's root
// process becomes the command: when it exits the pane (and tab) close,
// preserving the tmux contract that a session ends with its command. The typed
// command executes only after the fresh pane's shell finishes initializing, so
// wait (bounded) for the launch to actually land — otherwise callers probing
// right after Start see a bare shell.
func (p *Provider) launchRaw(ctx context.Context, paneID, raw string) error {
	if err := p.c.paneRunCommand(ctx, paneID, "exec /bin/sh -c "+shellquote.Quote(raw)); err != nil {
		return err
	}
	p.waitPaneLaunched(ctx, paneID, raw)
	return nil
}

// waitPaneLaunched polls the pane until the launched `/bin/sh -c <raw>` shows
// up in its foreground (exec preserves argv), the pane is gone (the command
// already ran and exited), or the bound elapses. Best-effort by design.
func (p *Provider) waitPaneLaunched(ctx context.Context, paneID, raw string) {
	deadline := time.Now().Add(rawLaunchWait)
	for time.Now().Before(deadline) {
		shellPID, fg, err := p.c.processInfo(ctx, paneID)
		switch {
		case err != nil && (strings.Contains(err.Error(), "not_found") || strings.Contains(err.Error(), "not found")):
			return // pane already gone: the command ran and exited
		case err == nil && shellPID != 0 && (paneRunsCommand(fg, raw) || paneRootReplaced(shellPID, fg)):
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(200 * time.Millisecond):
		}
	}
}
