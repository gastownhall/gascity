package k8s

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/runtime"
)

// k8sAcceptsStartupDialogs reports whether Start and Relaunch run the shared
// startup-dialog pass for cfg: the explicit AcceptStartupDialogs setting, or
// acceptance inferred from the managed process hints.
func k8sAcceptsStartupDialogs(cfg runtime.Config) bool {
	return runtime.ShouldAcceptStartupDialogs(cfg)
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

// runStartupDialogPass answers the agent's startup dialogs before the
// post-start liveness probe and the initial nudge, so the nudge reaches the
// agent rather than a workspace-trust or permission dialog. A pass that
// fails is best-effort: it is reported on stderr and startup continues.
func (p *Provider) runStartupDialogPass(ctx context.Context, name string) {
	if err := p.acceptStartupDialogs(ctx, name); err != nil {
		fmt.Fprintf(p.stderr, "gc: warning: session %q: startup dialogs: %v\n", name, err) //nolint:errcheck
	}
}
