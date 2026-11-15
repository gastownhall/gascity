package herdr

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/runtime"
)

// startupDialogWarningOut receives startup-dialog warnings (a var so tests can
// capture them).
var startupDialogWarningOut io.Writer = os.Stderr

// acceptStartupDialogs answers the known startup dialogs (workspace trust,
// bypass permissions, …) on a freshly launched agent's pane — herdr's
// equivalent of tmux's doStartSession Step 3. It reads and keys the launch's
// own pane under Start's ctx rather than going through Peek/SendKeys, which
// resolve by name, ignore cancellation, and (SendKeys) swallow an unresolvable
// pane. It returns the matcher's error and leaves the verdict to Start (see
// launchOrchestration): ErrWorkspaceTrustUnconfirmed means the trust dialog is
// still on screen with its cursor off the trust row.
//
// workDir is the launch's effective working directory: it decides which
// in-repo CLAUDE.md imports the matcher may auto-allow.
func (p *Provider) acceptStartupDialogs(ctx context.Context, paneID, workDir string) error {
	peek := func(lines int) (string, error) { return p.c.paneRead(ctx, paneID, "visible", lines) }
	send := func(keys ...string) error { return p.c.sendKeys(ctx, paneID, translateKeys(keys)...) }
	return runtime.AcceptStartupDialogs(ctx, peek, send,
		runtime.WithTrustedImportRoot(runtime.WorkspaceImportTrustRoot(ctx, workDir)))
}

// confirmSurvivedStartupDialogs is tmux's Step 5 for the dialog passes: a
// session that ended during them died during startup. A dialog answer can end
// the agent. The rate-limit step selects Stop on purpose, and the bypass step
// sends a blind Down then Enter, so a dropped Down confirms "No, exit". A kind
// launch's shell outlives its agent, and going on would type the first turn
// into that shell. A check that cannot answer proves nothing either way, so
// it warns and Start goes on: deliverStartupTurn never types into a kind
// launch's pane.
func (p *Provider) confirmSurvivedStartupDialogs(ctx context.Context, name string, launch launchedPane) error {
	alive, err := p.launchAlive(ctx, launch)
	switch {
	case err != nil:
		_, _ = fmt.Fprintf(startupDialogWarningOut, "warning: session %q: checking it survived its startup dialogs: %v\n", name, err)
	case !alive:
		return fmt.Errorf("%w: session %q", runtime.ErrSessionDiedDuringStartup, name)
	}
	return nil
}
