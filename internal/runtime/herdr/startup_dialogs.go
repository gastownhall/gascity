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
// pane. Best-effort: a failure is reported as one warning line, never returned,
// because the session is live and failing Start would only trigger a respawn.
//
// workDir is the launch's effective working directory: it decides which
// in-repo CLAUDE.md imports the matcher may auto-allow.
func (p *Provider) acceptStartupDialogs(ctx context.Context, name, paneID, workDir string) {
	peek := func(lines int) (string, error) { return p.c.paneRead(ctx, paneID, "visible", lines) }
	send := func(keys ...string) error { return p.c.sendKeys(ctx, paneID, translateKeys(keys)...) }
	err := runtime.AcceptStartupDialogs(ctx, peek, send,
		runtime.WithTrustedImportRoot(runtime.WorkspaceImportTrustRoot(ctx, workDir)))
	if err != nil {
		_, _ = fmt.Fprintf(startupDialogWarningOut, "warning: session %q: startup dialogs: %v\n", name, err)
	}
}
