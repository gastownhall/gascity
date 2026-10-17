package exec

import (
	"context"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/runtime"
)

var _ runtime.FencedInputProvider = (*Provider)(nil)

// InputFencing reports how the pack's sessions take fenced input: atomic for
// a pack that declares the exec connection (proc.exec), whose input the tmux
// carrier guards with the tmux provider's if-shell on the agent pane's fence;
// none for a pack driven by the dedicated wire ops, which cannot condition
// input on an incarnation. Like the pack's other traits it is read from the
// handshake, so it is static per provider.
func (p *Provider) InputFencing(string) runtime.InputFencing {
	if p.handshakeCapability(runtime.ProtocolCapabilityConnectionExec) {
		return runtime.InputFencingAtomic
	}
	return runtime.InputFencingNone
}

// fencedCarrier is the carrier for fenced input to name, or
// ErrInputFenceUnsupported for a pack that cannot fence.
func (p *Provider) fencedCarrier(name string) (runtime.Carrier, error) {
	if p.InputFencing(name) == runtime.InputFencingNone {
		return nil, fmt.Errorf("session %q: %w", name, runtime.ErrInputFenceUnsupported)
	}
	return p.carrier(), nil
}

// NudgeFenced delivers content as Nudge does, through the carrier guard. The
// exec provider has no idle wait to skip and cannot deduplicate, so
// n.Immediate and n.DeliveryKey change nothing.
func (p *Provider) NudgeFenced(ctx context.Context, name string, content []runtime.ContentBlock, n runtime.FencedNudge) error {
	c, err := p.fencedCarrier(name)
	if err != nil {
		return err
	}
	return c.NudgeFenced(ctx, name, content, n.Fence)
}

// SendKeysFenced sends keys as SendKeys does, through the carrier guard.
func (p *Provider) SendKeysFenced(ctx context.Context, name string, fence runtime.InputFence, keys ...string) error {
	c, err := p.fencedCarrier(name)
	if err != nil {
		return err
	}
	return c.SendKeysFenced(ctx, name, fence, keys...)
}

// InterruptFenced interrupts as Interrupt does, through the carrier guard.
func (p *Provider) InterruptFenced(ctx context.Context, name string, fence runtime.InputFence) error {
	c, err := p.fencedCarrier(name)
	if err != nil {
		return err
	}
	return c.InterruptFenced(ctx, name, fence)
}

// publishInputFence records the incarnation cfg.Env names on the agent pane
// a welded start op created, for a pack that can fence. Best-effort: a pane
// left without a fence is unproven, so fenced input to it misses rather than
// reaching it unchecked.
func (p *Provider) publishInputFence(ctx context.Context, name string, cfg runtime.Config) {
	f, ok := runtime.InputFenceFromEnv(cfg.Env)
	if !ok || p.InputFencing(name) == runtime.InputFencingNone {
		return
	}
	if err := p.carrier().PublishInputFence(ctx, name, f); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "exec provider: warning: session %q: %v\n", name, err)
	}
}

// launchFenceArgs is the tmux command, joined to the launch's own as one
// command list, that sets the launched agent pane's fence to the
// incarnation cfg.Env names, or clears it when cfg.Env names none: a
// respawned pane keeps its option, so its predecessor's fence must never
// name it. Nil for a pack that cannot fence.
func (p *Provider) launchFenceArgs(name string, cfg runtime.Config) []string {
	if p.InputFencing(name) == runtime.InputFencingNone {
		return nil
	}
	if f, ok := runtime.InputFenceFromEnv(cfg.Env); ok {
		return append([]string{";"}, runtime.TmuxPublishInputFenceArgs(execTmuxSession, f)...)
	}
	return append([]string{";"}, runtime.TmuxClearInputFenceArgs(execTmuxSession)...)
}
