//go:build !windows

package herdr

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/shellquote"
)

// rawLaunchWarningOut receives the raw-launch-unconfirmed diagnostic (a var so
// tests can capture it).
var rawLaunchWarningOut io.Writer = os.Stderr

// reportRawLaunch waits for a raw launch to land and reports one that did not
// within the bound. Start stays best-effort (the reconciler tolerates a slow
// launch), but an operator reading stderr learns that the launch was never
// seen. The report names the session, pane and bound only: the raw command and
// the session env can carry secrets.
func (p *Provider) reportRawLaunch(ctx context.Context, name, paneID, raw string) {
	wait := p.launchWait
	if wait <= 0 {
		wait = rawLaunchWait
	}
	if p.waitPaneLaunched(ctx, paneID, raw, wait) || ctx.Err() != nil {
		return
	}
	fmt.Fprintf(rawLaunchWarningOut, "herdr: start %q: raw launch in pane %s not confirmed within %s; Start proceeds best-effort\n", name, paneID, wait) //nolint:errcheck // best-effort diagnostic
}

// launchRaw execs the raw command through the pane's shell, so the pane's root
// process becomes the command: when it exits the pane (and tab) close,
// preserving the tmux contract that a session ends with its command. The typed
// command executes only after the fresh pane's shell finishes initializing, so
// wait (bounded) for the launch to actually land — otherwise callers probing
// right after Start see a bare shell. A launch that never lands is reported,
// not failed: see reportRawLaunch.
func (p *Provider) launchRaw(ctx context.Context, name, paneID, raw string) error {
	if err := p.c.paneRunCommand(ctx, paneID, "exec /bin/sh -c "+shellquote.Quote(raw)); err != nil {
		return err
	}
	p.reportRawLaunch(ctx, name, paneID, raw)
	return nil
}

// waitPaneLaunched polls the pane until the launched `/bin/sh -c <raw>` shows
// up in its foreground (exec preserves argv), the root process is no longer an
// interactive shell, the pane is gone (the command already ran and exited), or
// wait elapses. It reports whether the launch was confirmed (or the pane is
// gone); false means the bound elapsed or ctx ended first.
func (p *Provider) waitPaneLaunched(ctx context.Context, paneID, raw string, wait time.Duration) bool {
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		shellPID, fg, err := p.c.processInfo(ctx, paneID)
		switch {
		case err != nil && (strings.Contains(err.Error(), "not_found") || strings.Contains(err.Error(), "not found")):
			return true // pane already gone: the command ran and exited
		case err == nil && shellPID != 0 && (paneRunsCommand(fg, raw) || paneRootReplaced(shellPID, fg)):
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
	}
	return false
}
