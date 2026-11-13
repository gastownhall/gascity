package k8s

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// k8sAcceptsStartupDialogs reports whether Start and Relaunch run the shared
// startup-dialog pass for cfg: the explicit AcceptStartupDialogs setting, or
// acceptance inferred from the managed process hints. A one-shot command is
// never polled, keeping its Start a fire-and-forget launch.
func k8sAcceptsStartupDialogs(cfg runtime.Config) bool {
	return k8sRequiresPostStartLiveness(cfg) && runtime.ShouldAcceptStartupDialogs(cfg)
}

// acceptStartupDialogs runs the shared startup-dialog pass
// ([runtime.AcceptStartupDialogs]) over the session's tmux carrier under ctx,
// so the dialog rules stay those every tmux-in-a-box runtime shares. Startup
// keys go straight to the carrier, as tmux's own startup pass sends them.
func (p *Provider) acceptStartupDialogs(ctx context.Context, name string) error {
	c := p.carrier()
	return runtime.AcceptStartupDialogs(ctx,
		func(lines int) (string, error) { return c.Peek(ctx, name, lines) },
		func(keys ...string) error { return c.SendKeys(ctx, name, keys...) },
	)
}

// clearRelaunchHistory clears the warm pane's scrollback after a respawn so
// the previous agent's prompt cannot read as the new agent being ready. The
// visible screen and the pipe-pane log are untouched. A failure is reported
// on stderr and the relaunch continues; only an ended ctx is returned.
func (p *Provider) clearRelaunchHistory(ctx context.Context, name string) error {
	if err := p.carrier().ClearScrollback(ctx, name); err != nil {
		fmt.Fprintf(p.stderr, "gc: warning: session %q: clearing history before the startup-dialog pass: %v\n", name, err) //nolint:errcheck
	}
	return ctx.Err()
}

// runStartupDialogPass answers the agent's startup dialogs before the
// post-start liveness probe and the initial nudge, so the nudge reaches the
// agent rather than a workspace-trust or permission dialog. A pass that
// fails is best-effort: it is reported on stderr and startup continues. Only
// a ctx that ended during the pass is returned, so the caller stops before
// probing or nudging.
func (p *Provider) runStartupDialogPass(ctx context.Context, name string) error {
	dialogErr := p.acceptStartupDialogs(ctx, name)
	if err := ctx.Err(); err != nil {
		return err
	}
	if dialogErr != nil {
		fmt.Fprintf(p.stderr, "gc: warning: session %q: startup dialogs: %v\n", name, dialogErr) //nolint:errcheck
	}
	return nil
}
