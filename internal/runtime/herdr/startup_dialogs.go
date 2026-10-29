package herdr

import (
	"context"
	"fmt"
	"io"
	"os"

	"github.com/gastownhall/gascity/internal/runtime"
)

// startupDialogWarningOut receives startup-dialog warnings (a var so tests
// can capture it).
var startupDialogWarningOut io.Writer = os.Stderr

// acceptStartupDialogs runs the shared startup-dialog matcher
// (runtime.AcceptStartupDialogs) against the pane a launch just landed in —
// herdr's equivalent of tmux's doStartSession dialog step. It answers the
// dialogs that park a headless agent at first launch (Claude's workspace-trust
// "Quick safety check", the bypass-permissions warning, …; #7027), which herdr
// has no native verb for.
//
// Scoped to the launch's final pane id rather than Provider.Peek/SendKeys:
// Start already holds that pane, the provider-level closures build
// context.Background() (ignoring Start's cancellation) and return nil without
// sending when the pane cannot be resolved (masking a broken step), and the
// pane id does not depend on the agent-name registry herdr clears on occupant
// change. Key names are translated (tmux-style "Enter" → herdr "enter") before
// send-keys. workDir is the directory the agent was launched in (not
// cfg.WorkDir, which may be empty and resolved through a fallback): it scopes
// external-CLAUDE.md-import auto-acceptance to the repository the session runs
// in via WorkspaceImportTrustRoot.
//
// Best-effort: a dialog left unconfirmed — or a pane read/send-keys failure,
// or Start cancellation — is reported as one warning line and never fails
// Start; the session is live, and failing Start would only feed the respawn
// storm.
func (p *Provider) acceptStartupDialogs(ctx context.Context, name, paneID, workDir string) {
	peek := func(lines int) (string, error) { return p.c.paneRead(ctx, paneID, "visible", lines) }
	send := func(keys ...string) error { return p.c.sendKeys(ctx, paneID, translateKeys(keys)...) }
	if err := runtime.AcceptStartupDialogs(ctx, peek, send,
		runtime.WithTrustedImportRoot(runtime.WorkspaceImportTrustRoot(ctx, workDir))); err != nil {
		_, _ = fmt.Fprintf(startupDialogWarningOut, "warning: session %q: %v\n", name, err)
	}
}
